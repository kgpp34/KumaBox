package cli

import (
	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/state"
)

func configuredStores(cfg config.Config) (state.Set, error) {
	return state.Open(cfg)
}
