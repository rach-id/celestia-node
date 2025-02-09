package node

import (
	"context"

	"go.uber.org/fx"

	"github.com/celestiaorg/celestia-node/node/p2p"
)

// lightComponents keeps all the components as DI options required to built a Light Node.
func lightComponents(cfg *Config, repo Repository) fx.Option {
	return fx.Options(
		// manual providing
		fx.Provide(context.Background),
		fx.Provide(func() *Config {
			return cfg
		}),
		fx.Provide(func() ConfigLoader {
			return repo.Config
		}),
		fx.Provide(repo.Datastore),
		fx.Provide(repo.Keystore),
		// components
		p2p.Components(cfg.P2P),
	)
}
