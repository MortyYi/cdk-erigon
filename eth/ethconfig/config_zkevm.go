package ethconfig

import (
	"github.com/ledgerwatch/erigon-lib/common"
	"math/big"
	"time"
)

type Zk struct {
	L2ChainId                   uint64
	L2RpcUrl                    string
	L2DataStreamerUrl           string
	L1ChainId                   uint64
	L1RpcUrl                    string
	L1PolygonRollupManager      common.Address
	L1Rollup                    common.Address
	L1TopicVerification         common.Hash
	L1TopicSequence             common.Hash
	L1BlockRange                uint64
	L1QueryDelay                uint64
	L1MaticContractAddress      common.Address
	L1GERManagerContractAddress common.Address
	L1FirstBlock                uint64
	RpcRateLimits               int
	DatastreamVersion           int
	SequencerAddress            common.Address
	ExecutorUrls                []string
	ExecutorStrictMode          bool

	RebuildTreeAfter uint64

	EffectiveGas      EffectiveGasPriceCfg
	GasPriceEstimator GasPriceEstimatorConfig
}

// EffectiveGasPriceCfg contains the configuration properties for the effective gas price
type EffectiveGasPriceCfg struct {
	// Enabled is a flag to enable/disable the effective gas price
	Enabled bool

	// L1GasPriceFactor is the percentage of the L1 gas price that will be used as the L2 min gas price
	L1GasPriceFactor float64

	// ByteGasCost is the gas cost per byte that is not 0
	ByteGasCost uint64

	// ZeroByteGasCost is the gas cost per byte that is 0
	ZeroByteGasCost uint64

	// NetProfit is the profit margin to apply to the calculated breakEvenGasPrice
	NetProfit float64

	// BreakEvenFactor is the factor to apply to the calculated breakevenGasPrice when comparing it with the gasPriceSigned of a tx
	BreakEvenFactor float64

	// FinalDeviationPct is the max allowed deviation percentage BreakEvenGasPrice on re-calculation
	FinalDeviationPct uint64

	// EthTransferGasPrice is the fixed gas price returned as effective gas price for txs tha are ETH transfers (0 means disabled)
	// Only one of EthTransferGasPrice or EthTransferL1GasPriceFactor params can be different than 0. If both params are set to 0, the sequencer will halt and log an error
	EthTransferGasPrice uint64

	// EthTransferL1GasPriceFactor is the percentage of L1 gas price returned as effective gas price for txs tha are ETH transfers (0 means disabled)
	// Only one of EthTransferGasPrice or EthTransferL1GasPriceFactor params can be different than 0. If both params are set to 0, the sequencer will halt and log an error
	EthTransferL1GasPriceFactor float64

	// L2GasPriceSuggesterFactor is the factor to apply to L1 gas price to get the suggested L2 gas price used in the
	// calculations when the effective gas price is disabled (testing/metrics purposes)
	L2GasPriceSuggesterFactor float64
}

// EstimatorType different gas estimator types.
type EstimatorType string

const (
	// DefaultType default gas price from config is set.
	DefaultType EstimatorType = "default"
	// LastNBatchesType calculate average gas tip from last n batches.
	LastNBatchesType EstimatorType = "lastnbatches"
	// FollowerType calculate the gas price basing on the L1 gasPrice.
	FollowerType EstimatorType = "follower"
)

type GasPriceEstimatorConfig struct {
	Type EstimatorType

	// DefaultGasPriceWei is used to set the gas price to be used by the default gas pricer or as minimim gas price by the follower gas pricer.
	DefaultGasPriceWei uint64
	// MaxGasPriceWei is used to limit the gas price returned by the follower gas pricer to a maximum value. It is ignored if 0.
	MaxGasPriceWei uint64
	MaxPrice       *big.Int
	IgnorePrice    *big.Int
	CheckBlocks    int
	Percentile     int
	UpdatePeriod   time.Duration

	Factor float64
}
