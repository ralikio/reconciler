// solution from https://github.com/billiford/go-clouddriver/blob/master/pkg/kubernetes/patcher/patcher.go

package kubeclient

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/jonboulle/clockwork"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/jsonmergepatch"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/resource"
	oapi "k8s.io/kube-openapi/pkg/util/proto"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util"
	"k8s.io/kubectl/pkg/util/openapi"
)

const (
	// maxPatchRetry is the maximum number of conflicts retry for during a patch operation before returning failure
	maxPatchRetry = 5
	// backOffPeriod is the period to back off when kubeClient patch results in error.
	backOffPeriod = 1 * time.Second
	// how many times we can retry before back off
	triesBeforeBackOff = 1
)

func newPatcher(info *resource.Info, helper *resource.Helper) *Patcher {
	var openapiSchema openapi.Resources

	return &Patcher{
		Mapping:       info.Mapping,
		Helper:        helper,
		Overwrite:     true,
		BackOff:       clockwork.NewRealClock(),
		Force:         false,
		Cascade:       true,
		Timeout:       time.Duration(0),
		GracePeriod:   -1,
		OpenapiSchema: openapiSchema,
		Retries:       0,
	}
}

type Patcher struct {
	Mapping *meta.RESTMapping
	Helper  *resource.Helper

	Overwrite bool
	BackOff   clockwork.Clock

	Force       bool
	Cascade     bool
	Timeout     time.Duration
	GracePeriod int

	// If set, forces the patch against a specific resourceVersion
	ResourceVersion *string

	// Number of retries to make if the patch fails with conflict
	Retries int

	OpenapiSchema openapi.Resources
}

// Patch tries to patch an OpenAPI resource. On success, returns the merge patch as well
// the final patched object. On failure, returns an error.
func (p *Patcher) Patch(current runtime.Object, modified []byte,
	namespace, name string) ([]byte, runtime.Object, error) {
	var getErr error

	patchBytes, patchObject, err := p.patchSimple(current, modified, namespace, name)

	if p.Retries == 0 {
		p.Retries = maxPatchRetry
	}

	for i := 1; i <= p.Retries && errors.IsConflict(err); i++ {
		if i > triesBeforeBackOff {
			p.BackOff.Sleep(backOffPeriod)
		}

		current, getErr = p.Helper.Get(namespace, name)
		if getErr != nil {
			return nil, nil, getErr
		}

		patchBytes, patchObject, err = p.patchSimple(current, modified, namespace, name)
	}

	if err != nil && (errors.IsConflict(err) || errors.IsInvalid(err)) && p.Force {
		patchBytes, patchObject, err = p.deleteAndCreate(current, modified, namespace, name)
	}

	return patchBytes, patchObject, err
}

func (p *Patcher) patchSimple(obj runtime.Object, modified []byte, namespace, name string) ([]byte, runtime.Object, error) {
	// Serialize the current configuration of the object from the server.
	current, err := runtime.Encode(unstructured.UnstructuredJSONScheme, obj)
	if err != nil {
		return nil, nil, err
	}

	// Retrieve the original configuration of the object from the annotation.
	original, err := util.GetOriginalConfiguration(obj)
	if err != nil {
		return nil, nil, err
	}

	var (
		patchType       types.PatchType
		patch           []byte
		lookupPatchMeta strategicpatch.LookupPatchMeta
		schema          oapi.Schema
	)
	// createPatchErrFormat := "creating patch with:\noriginal:\n%s\nmodified:\n%s\ncurrent:\n%s\nfor:"

	// Create the versioned types from the type defined in the restmapping
	// (which is the API version we'll be submitting the patch to)
	versionedObject, err := scheme.Scheme.New(p.Mapping.GroupVersionKind)

	switch {
	case runtime.IsNotRegisteredError(err):
		// fall back to generic JSON merge patch
		patchType = types.MergePatchType
		preconditions := []mergepatch.PreconditionFunc{mergepatch.RequireKeyUnchanged("apiVersion"),
			mergepatch.RequireKeyUnchanged("kind"), mergepatch.RequireMetadataKeyUnchanged("name")}

		patch, err = jsonmergepatch.CreateThreeWayJSONMergePatch(original, modified, current, preconditions...)
		if err != nil {
			if mergepatch.IsPreconditionFailed(err) {
				return nil, nil, fmt.Errorf("%s", "At least one of apiVersion, kind and name was changed")
			}

			return nil, nil, err
		}
	case err != nil:
		return nil, nil, err
	case err == nil:
		// Compute a three way strategic merge patch to send to server.
		patchType = types.StrategicMergePatchType

		// Try to use openapi first if the openapi spec is available and can successfully calculate the patch.
		// Otherwise, fall back to baked-in types.
		if p.OpenapiSchema != nil {
			if schema = p.OpenapiSchema.LookupResource(p.Mapping.GroupVersionKind); schema != nil {
				lookupPatchMeta = strategicpatch.PatchMetaFromOpenAPI{Schema: schema}
				if openapiPatch, err := strategicpatch.CreateThreeWayMergePatch(original, modified, current, lookupPatchMeta, p.Overwrite); err != nil {
					fmt.Fprintf(os.Stderr, "warning: error calculating patch from openapi spec: %v\n", err)
				} else {
					patchType = types.StrategicMergePatchType
					patch = openapiPatch
				}
			}
		}

		if patch == nil {
			lookupPatchMeta, err = strategicpatch.NewPatchMetaFromStruct(versionedObject)
			if err != nil {
				return nil, nil, err
			}

			patch, err = strategicpatch.CreateThreeWayMergePatch(original, modified, current, lookupPatchMeta, p.Overwrite)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	if string(patch) == "{}" {
		return patch, obj, nil
	}

	if p.ResourceVersion != nil {
		patch, err = addResourceVersion(patch, *p.ResourceVersion)
		if err != nil {
			return nil, nil, err
		}
	}

	patchedObj, err := p.Helper.Patch(namespace, name, patchType, patch, nil)

	return patch, patchedObj, err
}

func (p *Patcher) deleteAndCreate(original runtime.Object, modified []byte, namespace, name string) ([]byte, runtime.Object, error) {
	if err := p.delete(namespace, name); err != nil {
		return modified, nil, err
	}
	// TODO: use wait
	if err := wait.PollImmediate(1*time.Second, p.Timeout, func() (bool, error) {
		if _, err := p.Helper.Get(namespace, name); !errors.IsNotFound(err) {
			return false, err
		}

		return true, nil
	}); err != nil {
		return modified, nil, err
	}

	versionedObject, _, err := unstructured.UnstructuredJSONScheme.Decode(modified, nil, nil)
	if err != nil {
		return modified, nil, err
	}

	createdObject, err := p.Helper.Create(namespace, true, versionedObject)
	if err != nil {
		// restore the original object if we fail to create the new one
		// but still propagate and advertise error to user
		recreated, recreateErr := p.Helper.Create(namespace, true, original)
		if recreateErr != nil {
			err = fmt.Errorf("An error occurred force-replacing the existing object with the newly provided one:\n\n%v.\n\nAdditionally, an error occurred attempting to restore the original object:\n\n%v", err, recreateErr)
		} else {
			createdObject = recreated
		}
	}

	return modified, createdObject, err
}

func (p *Patcher) delete(namespace, name string) error {
	options := asDeleteOptions(p.Cascade, p.GracePeriod)
	_, err := p.Helper.DeleteWithOptions(namespace, name, &options)

	return err
}

func asDeleteOptions(cascade bool, gracePeriod int) metav1.DeleteOptions {
	options := metav1.DeleteOptions{}
	if gracePeriod >= 0 {
		options = *metav1.NewDeleteOptions(int64(gracePeriod))
	}

	policy := metav1.DeletePropagationForeground
	if !cascade {
		policy = metav1.DeletePropagationOrphan
	}

	options.PropagationPolicy = &policy

	return options
}

func addResourceVersion(patch []byte, rv string) ([]byte, error) {
	var patchMap map[string]interface{}

	err := json.Unmarshal(patch, &patchMap)
	if err != nil {
		return nil, err
	}

	u := unstructured.Unstructured{Object: patchMap}

	a, err := meta.Accessor(&u)
	if err != nil {
		return nil, err
	}

	a.SetResourceVersion(rv)

	return json.Marshal(patchMap)
}
