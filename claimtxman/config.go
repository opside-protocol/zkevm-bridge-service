package claimtxman

import "github.com/0xPolygonHermez/zkevm-node/config/types"

// Config is configuration for L2 claim transaction manager
type Config struct {
	//Enabled whether to enable this module
	Enabled      bool `mapstructure:"Enabled"`
	EnabledCross bool `mapstructure:"EnabledCross"`
	// FrequencyToMonitorTxs frequency of the resending failed txs
	FrequencyToMonitorTxs types.Duration `mapstructure:"FrequencyToMonitorTxs"`
	// PrivateKeyL2 defines the key store file that is going
	// to be read in order to provide the private key to sign the claim txs
	PrivateKeyL1 types.KeystoreFileConfig `mapstructure:"PrivateKeyL1"`
	PrivateKeyL2 types.KeystoreFileConfig `mapstructure:"PrivateKeyL2"`
}
