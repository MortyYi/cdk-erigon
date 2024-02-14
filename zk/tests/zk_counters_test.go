package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv/memdb"
	"github.com/ledgerwatch/erigon/chain"
	"github.com/ledgerwatch/erigon/consensus/ethash/ethashcfg"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/eth/ethconsensusconfig"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/erigon/zk/tx"
	zktypes "github.com/ledgerwatch/erigon/zk/types"
	"github.com/ledgerwatch/erigon/zkevm/hex"
	"math/big"
	"os"
	"strconv"
	"testing"
)

const root = "./testdata/counters"
const transactionGasLimit = 30000000

var (
	noop = state.NewNoopWriter()
)

func Test_RunTestVectors(t *testing.T) {
	type vector struct {
		BatchL2Data        string `json:"batchL2Data"`
		BatchL2DataDecoded []byte
		Genesis            []struct {
			Address string `json:"address"`
			Nonce   string `json:"nonce"`
			Balance string `json:"balance"`
			PvtKey  string `json:"pvtKey"`
		} `json:"genesis"`
		VirtualCounters struct {
			Steps    int `json:"steps"`
			Arith    int `json:"arith"`
			Binary   int `json:"binary"`
			MemAlign int `json:"memAlign"`
			Keccaks  int `json:"keccaks"`
			Padding  int `json:"padding"`
			Poseidon int `json:"poseidon"`
			Sha256   int `json:"sha256"`
		} `json:"virtualCounters"`
		SequencerAddress string `json:"sequencerAddress"`
		ChainId          int64  `json:"chainID"`
	}

	files, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, file := range files {
		contents, err := os.ReadFile(fmt.Sprintf("%s/%s", root, file.Name()))
		if err != nil {
			t.Fatal(err)
		}

		var tests []vector
		if err = json.Unmarshal(contents, &tests); err != nil {
			t.Fatal(err)
		}

		test := tests[0]

		test.BatchL2DataDecoded, err = hex.DecodeHex(test.BatchL2Data)
		if err != nil {
			t.Fatal(err)
		}

		decodedTransactions, _, _, err := tx.DecodeTxs(test.BatchL2DataDecoded, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(decodedTransactions) == 0 {
			t.Errorf("found no transactions in file %s", file.Name())
		}

		db := memdb.NewTestDB(t)
		tx, err := db.BeginRw(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		genesisAccounts := map[common.Address]types.GenesisAccount{}

		for _, g := range test.Genesis {
			addr := common.HexToAddress(g.Address)
			key, err := hex.DecodeHex(g.PvtKey)
			if err != nil {
				t.Fatal(err)
			}
			nonce, err := strconv.ParseUint(g.Nonce, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			balance, err := strconv.ParseUint(g.Balance, 10, 64)
			acc := types.GenesisAccount{
				Balance:    new(big.Int).SetUint64(balance),
				Nonce:      nonce,
				PrivateKey: key,
			}
			genesisAccounts[addr] = acc
		}

		genesis := &types.Genesis{
			Alloc: genesisAccounts,
			Config: &chain.Config{
				ChainID: big.NewInt(test.ChainId),
			},
		}

		_, ibs, err := core.WriteGenesisState(genesis, tx, "./temp")
		if err != nil {
			t.Fatal(err)
		}

		sequencer := common.HexToAddress(test.SequencerAddress)

		batchCollector := vm.NewBatchCounterCollector(80)
		overflow := batchCollector.StartNewBlock()
		if overflow {
			t.Fatal("unexpected overflow")
		}

		header := &types.Header{
			Number:     big.NewInt(1),
			Difficulty: big.NewInt(0),
		}
		getHeader := func(hash common.Hash, number uint64) *types.Header { return rawdb.ReadHeader(tx, hash, number) }

		chainConfig := params.ChainConfigByChainName("hermez-dev")

		ethashCfg := &ethashcfg.Config{
			CachesInMem:      1,
			CachesLockMmap:   true,
			DatasetDir:       "./dataset",
			DatasetsInMem:    1,
			DatasetsOnDisk:   1,
			DatasetsLockMmap: true,
			PowMode:          ethashcfg.ModeFake,
			NotifyFull:       false,
			Log:              nil,
		}

		engine := ethconsensusconfig.CreateConsensusEngine(chainConfig, ethashCfg, []string{}, true, "", "", true, "./datadir", nil, false /* readonly */, db)

		vmCfg := vm.ZkConfig{
			Config: vm.Config{
				Debug:         false,
				Tracer:        nil,
				NoRecursion:   false,
				NoBaseFee:     false,
				SkipAnalysis:  false,
				TraceJumpDest: false,
				NoReceipts:    false,
				ReadOnly:      false,
				StatelessExec: false,
				RestoreState:  false,
				ExtraEips:     nil,
			},
		}

		for _, transaction := range decodedTransactions {
			txCounters := vm.NewTransactionCounter(transaction, 80)
			overflow, err = batchCollector.AddNewTransactionCounters(txCounters)
			gasPool := new(core.GasPool).AddGas(transactionGasLimit)

			vmCfg.CounterCollector = txCounters.ExecutionCounters()

			_, _, err = core.ApplyTransaction(
				chainConfig,
				core.GetHashFn(header, getHeader),
				engine,
				&sequencer,
				gasPool,
				ibs,
				noop,
				header,
				transaction,
				&header.GasUsed,
				vmCfg.Config,
				big.NewInt(0), // parent excess data gas
				zktypes.EFFECTIVE_GAS_PRICE_PERCENTAGE_MAXIMUM)

			if err != nil {
				t.Fatal(err)
			}
			if overflow {
				t.Fatal("unexpected overflow")
			}
		}

		combined := batchCollector.CombineCollectors()

		vc := test.VirtualCounters
		if vc.Keccaks != combined[vm.K].Used() {
			t.Errorf("mismatched counters K have: %v want: %v", combined[vm.K].Used(), vc.Keccaks)
		}
	}
}
