package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/kyma-incubator/reconciler/pkg/cluster"
	"github.com/kyma-incubator/reconciler/pkg/keb"
	"github.com/kyma-incubator/reconciler/pkg/logger"
	"github.com/kyma-incubator/reconciler/pkg/model"
	"github.com/kyma-incubator/reconciler/pkg/reconciler"
	"github.com/panjf2000/ants/v2"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"sync"
)

type concurrency bool

const (
	defaultPoolSize                   = 50
	concurrencyNotAllowed concurrency = false
	concurrencyAllowed    concurrency = true
	doNotInstallCRD       bool        = false
	doInstallCRD          bool        = true
)

type Scheduler interface {
	Run(ctx context.Context) error
}

type RemoteScheduler struct {
	inventoryWatch InventoryWatcher
	workerFactory  WorkerFactory
	mothershipCfg  reconciler.MothershipReconcilerConfig
	poolSize       int
	logger         *zap.SugaredLogger
}

func NewRemoteScheduler(inventoryWatch InventoryWatcher, workerFactory WorkerFactory, mothershipCfg reconciler.MothershipReconcilerConfig, workers int, debug bool) (Scheduler, error) {
	l, err := logger.NewLogger(debug)
	if err != nil {
		return nil, err
	}
	return &RemoteScheduler{
		inventoryWatch: inventoryWatch,
		workerFactory:  workerFactory,
		mothershipCfg:  mothershipCfg,
		poolSize:       workers,
		logger:         l,
	}, nil
}

func (rs *RemoteScheduler) validate() error {
	if rs.poolSize < 0 {
		return fmt.Errorf("Worker pool size cannot be < 0")
	}
	if rs.poolSize == 0 {
		rs.poolSize = defaultPoolSize
	}
	return nil
}

func (rs *RemoteScheduler) Run(ctx context.Context) error {
	if err := rs.validate(); err != nil {
		return err
	}

	queue := make(chan cluster.State, rs.poolSize)

	rs.logger.Debugf("Starting worker pool with capacity %d workers", rs.poolSize)
	workersPool, err := ants.NewPoolWithFunc(rs.poolSize, func(i interface{}) {
		rs.schedule(i.(cluster.State))
	})
	if err != nil {
		return errors.Wrap(err, "failed to create worker pool of remote-scheduler")
	}

	go func(ctx context.Context, queue chan cluster.State) {
		if err := rs.inventoryWatch.Run(ctx, queue); err != nil {
			rs.logger.Errorf("Failed to run inventory watch: %s", err)
		}
	}(ctx, queue)

	for {
		select {
		case clusterState := <-queue:
			go func(workersPool *ants.PoolWithFunc) {
				if err := workersPool.Invoke(clusterState); err != nil {
					rs.logger.Errorf("Failed to pass cluster to cluster-pool worker: %s", err)
				}
			}(workersPool)
		case <-ctx.Done():
			rs.logger.Debug("Stopping remote scheduler because parent context got closed")
			return nil
		}
	}
}

func (rs *RemoteScheduler) schedule(state cluster.State) {
	schedulingID := uuid.NewString()
	components, err := state.Configuration.GetComponents()
	if err != nil {
		rs.logger.Errorf("Failed to get components for cluster %s: %s", state.Cluster.Cluster, err)
		return
	}

	if len(components) == 0 {
		rs.logger.Infof("No components to reconcile for cluster %s", state.Cluster.Cluster)
		return
	}

	//Reconcile CRD components first
	for _, component := range components {
		if rs.isCRDComponent(component.Component) {
			rs.reconcile(component, state, schedulingID, doInstallCRD, concurrencyNotAllowed)
		}
	}

	//Reconcile pre components
	for _, component := range components {
		if rs.isPreComponent(component.Component) {
			rs.reconcile(component, state, schedulingID, doNotInstallCRD, concurrencyNotAllowed)
		}
	}

	//Reconcile the rest
	for _, component := range components {
		if rs.isPreComponent(component.Component) || rs.isCRDComponent(component.Component) {
			continue
		}
		rs.reconcile(component, state, schedulingID, doNotInstallCRD, concurrencyAllowed)
	}
}

func (rs *RemoteScheduler) reconcile(component *keb.Components, state cluster.State, schedulingID string, installCRD bool, concurrent concurrency) {
	fn := func(component *keb.Components, state cluster.State, schedulingID string) {
		worker, err := rs.workerFactory.ForComponent(component.Component)
		if err != nil {
			rs.logger.Errorf("Error creating worker for component: %s", err)
			return
		}
		err = worker.Reconcile(component, state, schedulingID, installCRD)
		if err != nil {
			rs.logger.Errorf("Error while reconciling component %s: %s", component.Component, err)
		}
	}

	if bool(concurrent) {
		go fn(component, state, schedulingID)
	} else {
		fn(component, state, schedulingID)
	}
}

func (rs *RemoteScheduler) isCRDComponent(component string) bool {
	for _, c := range rs.mothershipCfg.CrdComponents {
		if component == c {
			return true
		}
	}
	return false
}

func (rs *RemoteScheduler) isPreComponent(component string) bool {
	for _, c := range rs.mothershipCfg.PreComponents {
		if component == c {
			return true
		}
	}
	return false
}

type LocalScheduler struct {
	cluster       keb.Cluster
	workerFactory WorkerFactory
	logger        *zap.SugaredLogger
}

func NewLocalScheduler(cluster keb.Cluster, workerFactory WorkerFactory, debug bool) (Scheduler, error) {
	log, err := logger.NewLogger(debug)
	if err != nil {
		return nil, err
	}
	return &LocalScheduler{
		cluster:       cluster,
		workerFactory: workerFactory,
		logger:        log,
	}, nil
}

func (ls *LocalScheduler) Run(ctx context.Context) error {
	schedulingID := uuid.NewString()

	clusterState, err := localClusterState(&ls.cluster)
	if err != nil {
		return fmt.Errorf("failed to convert to cluster state: %s", err)
	}

	components, err := clusterState.Configuration.GetComponents()
	if err != nil {
		return fmt.Errorf("failed to get components: %s", err)
	}

	results := make(chan error, len(components))

	var wg sync.WaitGroup
	wg.Add(len(components))

	//trigger all component reconcilers
	for _, component := range components {
		worker, err := ls.workerFactory.ForComponent(component.Component)
		if err != nil {
			return fmt.Errorf("failed to create a: %s", err)
		}

		go func(component *keb.Components, state cluster.State, schedulingID string) {
			defer wg.Done()
			err := worker.Reconcile(component, state, schedulingID, true)
			if err != nil {
				ls.logger.Errorf("Error while reconciling component %s: %s", component.Component, err)
			}
			results <- err
		}(component, *clusterState, schedulingID)
	}

	wg.Wait()

	close(results)

	for err := range results {
		if err != nil {
			return err
		}
	}

	return nil
}

func localClusterState(c *keb.Cluster) (*cluster.State, error) {
	var defaultContractVersion int64 = 1

	metadata, err := json.Marshal(c.Metadata)
	if err != nil {
		return nil, err
	}
	runtime, err := json.Marshal(c.RuntimeInput)
	if err != nil {
		return nil, err
	}
	clusterEntity := &model.ClusterEntity{
		Cluster:    c.Cluster,
		Runtime:    string(runtime),
		Metadata:   string(metadata),
		Kubeconfig: c.Kubeconfig,
		Contract:   defaultContractVersion,
	}

	components, err := json.Marshal(c.KymaConfig.Components)
	if err != nil {
		return nil, err
	}
	administrators, err := json.Marshal(c.KymaConfig.Administrators)
	if err != nil {
		return nil, err
	}
	configurationEntity := &model.ClusterConfigurationEntity{
		Cluster:        c.Cluster,
		KymaVersion:    c.KymaConfig.Version,
		KymaProfile:    c.KymaConfig.Profile,
		Components:     string(components),
		Administrators: string(administrators),
		Contract:       defaultContractVersion,
	}

	return &cluster.State{
		Cluster:       clusterEntity,
		Configuration: configurationEntity,
		Status:        &model.ClusterStatusEntity{},
	}, nil
}
