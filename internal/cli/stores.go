package cli

import (
	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/resources"
)

func configuredStores(cfg config.Config) (resources.StoreSet, error) {
	return resources.NewStoreSetForConfig(cfg)
}
