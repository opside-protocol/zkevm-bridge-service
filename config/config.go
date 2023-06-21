package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/0xPolygonHermez/zkevm-bridge-service/bridgectrl"
	"github.com/0xPolygonHermez/zkevm-bridge-service/claimtxman"
	"github.com/0xPolygonHermez/zkevm-bridge-service/db"
	"github.com/0xPolygonHermez/zkevm-bridge-service/etherman"
	"github.com/0xPolygonHermez/zkevm-bridge-service/server"
	"github.com/0xPolygonHermez/zkevm-bridge-service/synchronizer"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"
)

var (
	rollBackInfoMap = map[string]synchronizer.RollBackConfig{
		"0x751ed0749546e39a69148E7BF38D18CCe98E0caD": {
			Enable:       true,
			BaseBatchNum: uint64(5424),
			IgnoreTx: map[string]bool{
				"0x1a8551508e24dd758f430c03ca418c498db845ffc8691286f3560fe7b19adc5c": true,
				"0x59c2f623dd1f31c4d5185b9b4160bad25a4ea7d62b183270ff7304b9cabf83f5": true,
				"0x525af446c06dd466fa4b836d2ad85321dadf8e4ed993f6e919dfce1cc8ef69ca": true,
				"0x84c04cbb4a34e0e2b69aa9544e6cb1ca53079769b26ebd2feec074d44a2e3379": true,
				"0x289e43a651f6c55f23cccaf625ec1ed555c7caf36fc1a1351567473c9a03a1a2": true,
				"0xcf72c7742bee86e5c34abd34ae73927d4772f54917a3308bd1d3b40dbc3fbae1": true,
				"0xb16a990333a96fbb8a5144786c34f6ce1bc0dabc6ea1e7efe3c20b078143b965": true,
			},
		},
	}
)

// Config struct
type Config struct {
	Log              log.Config
	SyncDB           db.Config
	ClaimTxManager   claimtxman.Config
	Etherman         etherman.Config
	Synchronizer     synchronizer.Config
	BridgeController bridgectrl.Config
	BridgeServer     server.Config
	NetworkConfig
}

// Load loads the configuration
func Load(configFilePath string, network string) (*Config, error) {
	var cfg Config
	viper.SetConfigType("toml")

	err := viper.ReadConfig(bytes.NewBuffer([]byte(DefaultValues)))
	if err != nil {
		return nil, err
	}
	err = viper.Unmarshal(&cfg, viper.DecodeHook(mapstructure.TextUnmarshallerHookFunc()))
	if err != nil {
		return nil, err
	}
	if configFilePath != "" {
		dirName, fileName := filepath.Split(configFilePath)

		fileExtension := strings.TrimPrefix(filepath.Ext(fileName), ".")
		fileNameWithoutExtension := strings.TrimSuffix(fileName, "."+fileExtension)

		viper.AddConfigPath(dirName)
		viper.SetConfigName(fileNameWithoutExtension)
		viper.SetConfigType(fileExtension)
	}
	viper.AutomaticEnv()
	replacer := strings.NewReplacer(".", "_")
	viper.SetEnvKeyReplacer(replacer)
	viper.SetEnvPrefix("ZKEVM_BRIDGE")
	err = viper.ReadInConfig()
	if err != nil {
		_, ok := err.(viper.ConfigFileNotFoundError)
		if ok {
			log.Infof("config file not found")
		} else {
			log.Infof("error reading config file: ", err)
			return nil, err
		}
	}

	err = viper.Unmarshal(&cfg, viper.DecodeHook(mapstructure.TextUnmarshallerHookFunc()))
	if err != nil {
		return nil, err
	}

	if viper.IsSet("NetworkConfig") && network != "" {
		return nil, errors.New("Network details are provided in the config file (the [NetworkConfig] section) and as a flag (the --network or -n). Configure it only once and try again please.")
	}
	if !viper.IsSet("NetworkConfig") && network == "" {
		return nil, errors.New("Network details are not provided. Please configure the [NetworkConfig] section in your config file, or provide a --network flag.")
	}
	if !viper.IsSet("NetworkConfig") && network != "" {
		cfg.loadNetworkConfig(network)
	}

	cfgJSON, _ := json.MarshalIndent(cfg, "", "  ")
	log.Infof("Configuration loaded: \n%s\n", string(cfgJSON))

	cfg.Synchronizer.RollBackChainMap = rollBackInfoMap
	return &cfg, nil
}
