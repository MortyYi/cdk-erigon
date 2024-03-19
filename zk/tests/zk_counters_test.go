package vm

import (
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
	seq "github.com/ledgerwatch/erigon/zk/sequencer"
	"errors"
	"github.com/ledgerwatch/erigon-lib/kv"
)

const root = "./testdata/counters"
const transactionGasLimit = 30000000

var (
	noop = state.NewNoopWriter()
)

type vector struct {
	BatchL2Data        string `json:"batchL2Data"`
	BatchL2DataDecoded []byte
	Genesis            []struct {
		Address  string `json:"address"`
		Nonce    string `json:"nonce"`
		Balance  string `json:"balance"`
		PvtKey   string `json:"pvtKey"`
		ByteCode string `json:"bytecode"`
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

func Test_RunTestVectors(t *testing.T) {
	// we need to ensure we're running in a sequencer context to wrap the jump table
	os.Setenv(seq.SEQUENCER_ENV_KEY, "1")
	defer os.Setenv(seq.SEQUENCER_ENV_KEY, "0")

	files, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	var tests []vector
	var fileNames []string
	for _, file := range files {
		var inner []vector
		contents, err := os.ReadFile(fmt.Sprintf("%s/%s", root, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		fileNames = append(fileNames, file.Name())

		if err = json.Unmarshal(contents, &inner); err != nil {
			t.Fatal(err)
		}

		tests = append(tests, inner...)
	}

	for idx, test := range tests {
		runTest(t, test, err, fileNames, idx)
	}
}

func runTest(t *testing.T, test vector, err error, fileNames []string, idx int) {
	test.BatchL2DataDecoded, err = hex.DecodeHex(test.BatchL2Data)
	if err != nil {
		t.Fatal(err)
	}

	decodedTransactions, _, _, err := tx.DecodeTxs(test.BatchL2DataDecoded, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(decodedTransactions) == 0 {
		t.Errorf("found no transactions in file %s", fileNames[idx])
	}

	db, tx := memdb.NewTestTx(t)
	defer db.Close()
	defer tx.Rollback()

	for _, table := range kv.ChaindataTables {
		if err = tx.CreateBucket(table); err != nil {
			t.Fatal(err)
		}
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
		balance, ok := new(big.Int).SetString(g.Balance, 10)
		if !ok {
			t.Fatal(errors.New("could not parse balance"))
		}
		code, err := hex.DecodeHex(g.ByteCode)
		if err != nil {
			t.Fatal(err)
		}
		acc := types.GenesisAccount{
			Balance:    balance,
			Nonce:      nonce,
			PrivateKey: key,
			Code:       code,
		}
		genesisAccounts[addr] = acc
	}

	genesis := &types.Genesis{
		Alloc: genesisAccounts,
		Config: &chain.Config{
			ChainID: big.NewInt(test.ChainId),
		},
	}

	_, _, err = core.WriteGenesisState(genesis, tx, "./temp")
	if err != nil {
		t.Fatal(err)
	}

	sequencer := common.HexToAddress(test.SequencerAddress)

	header := &types.Header{
		Number:     big.NewInt(1),
		Difficulty: big.NewInt(0),
	}
	getHeader := func(hash common.Hash, number uint64) *types.Header { return rawdb.ReadHeader(tx, hash, number) }

	chainConfig := params.ChainConfigByChainName("hermez-dev")
	chainConfig.ChainID = big.NewInt(test.ChainId)

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

	stateReader := state.NewPlainStateReader(tx)
	ibs := state.New(stateReader)

	const smtMaxLevel = 80
	batchCollector := vm.NewBatchCounterCollector(smtMaxLevel, 8)
	overflow, err := batchCollector.StartNewBlock()
	if err != nil {
		t.Fatal(err)
	}
	if overflow {
		t.Fatal("unexpected overflow")
	}

	for _, transaction := range decodedTransactions {
		txCounters := vm.NewTransactionCounter(transaction, smtMaxLevel)
		overflow, err = batchCollector.AddNewTransactionCounters(txCounters)
		gasPool := new(core.GasPool).AddGas(transactionGasLimit)

		vmCfg.CounterCollector = txCounters.ExecutionCounters()

		_, result, err := core.ApplyTransaction_zkevm(
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
			vmCfg,
			big.NewInt(0), // parent excess data gas
			zktypes.EFFECTIVE_GAS_PRICE_PERCENTAGE_MAXIMUM)

		if err != nil {
			t.Fatal(err)
		}
		if overflow {
			t.Fatal("unexpected overflow")
		}
		if err = txCounters.ProcessTx(ibs, result.ReturnData); err != nil {
			t.Fatal(err)
		}
	}

	combined, err := batchCollector.CombineCollectors()
	if err != nil {
		t.Fatal(err)
	}

	vc := test.VirtualCounters

	var errors []string
	if vc.Keccaks != combined[vm.K].Used() {
		errors = append(errors, fmt.Sprintf("K have: %v want: %v", combined[vm.K].Used(), vc.Keccaks))
	}
	if vc.Arith != combined[vm.A].Used() {
		errors = append(errors, fmt.Sprintf("A have: %v want: %v", combined[vm.A].Used(), vc.Arith))
	}
	if vc.Binary != combined[vm.B].Used() {
		errors = append(errors, fmt.Sprintf("B have: %v want: %v", combined[vm.B].Used(), vc.Binary))
	}
	if vc.Padding != combined[vm.D].Used() {
		errors = append(errors, fmt.Sprintf("D have: %v want: %v", combined[vm.D].Used(), vc.Padding))
	}
	if vc.Sha256 != combined[vm.SHA].Used() {
		errors = append(errors, fmt.Sprintf("SHA have: %v want: %v", combined[vm.S].Used(), vc.Sha256))
	}
	if vc.MemAlign != combined[vm.M].Used() {
		errors = append(errors, fmt.Sprintf("M have: %v want: %v", combined[vm.M].Used(), vc.MemAlign))
	}
	if vc.Poseidon != combined[vm.P].Used() {
		errors = append(errors, fmt.Sprintf("P have: %v want: %v", combined[vm.P].Used(), vc.Poseidon))
	}
	if vc.Steps != combined[vm.S].Used() {
		errors = append(errors, fmt.Sprintf("S have: %v want: %v", combined[vm.S].Used(), vc.Steps))
	}

	if len(errors) > 0 {
		t.Errorf("problems in file %s \n", fileNames[idx])
		for _, e := range errors {
			fmt.Println(e)
		}
		fmt.Println("")
	}
}
