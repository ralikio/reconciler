package cli

import (
	"fmt"
	"github.com/kyma-incubator/reconciler/pkg/db"
	file "github.com/kyma-incubator/reconciler/pkg/files"
	"github.com/spf13/viper"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"
)

func NewEncryptionKey(backup bool) error {
	keyFile := viper.GetString("db.encryption.keyFile")
	if keyFile == "" {
		return fmt.Errorf("encryption key file not configured")
	}
	if !filepath.IsAbs(keyFile) { //ensure key file path is absolute (if not, use config-file location as parent-dir)
		keyFile = filepath.Join(filepath.Dir(viper.ConfigFileUsed()), keyFile)
	}

	encKey, err := db.NewEncryptionKey()
	if err != nil {
		return err
	}

	if file.Exists(keyFile) && backup {
		keyFileBackup := fmt.Sprintf("%s.%d.bak", keyFile, time.Now().Unix())
		if err := os.Rename(keyFile, keyFileBackup); err != nil {
			return err
		}
	}

	return ioutil.WriteFile(keyFile, []byte(encKey), 0600)
}
