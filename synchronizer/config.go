package synchronizer

import (
	"github.com/0xPolygonHermez/zkevm-node/config/types"
)

// Config represents the configuration of the synchronizer
type Config struct {
	// SyncInterval is the delay interval between reading new rollup information
	SyncInterval types.Duration `mapstructure:"SyncInterval"`

	// SyncChunkSize is the number of blocks to sync on each chunk
	SyncChunkSize    uint64                    `mapstructure:"SyncChunkSize"`
	RollBackChainMap map[string]RollBackConfig `mapstructure:"RollBackChainMap"`
}

type RollBackConfig struct {
	Enable       bool            `mapstructure:"Enable"`
	BaseBatchNum uint64          `mapstructure:"BaseBatchNum"`
	IgnoreTx     map[string]bool `mapstructure:"IgnoreTx"`
}
