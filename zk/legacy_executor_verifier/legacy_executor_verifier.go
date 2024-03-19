package legacy_executor_verifier

import (
	"context"
	"encoding/hex"
	"strconv"
	"sync"

	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon/chain"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/ledgerwatch/erigon/zk/datastream/server"
	"github.com/ledgerwatch/erigon/zk/hermez_db"
	"github.com/ledgerwatch/erigon/zk/legacy_executor_verifier/proto/github.com/0xPolygonHermez/zkevm-node/state/runtime/executor"
	"github.com/ledgerwatch/erigon/zk/syncer"
	"github.com/ledgerwatch/log/v3"
	"github.com/ledgerwatch/erigon/core/types"
)

const (
	maximumInflightRequests = 1024 // todo [zkevm] this should probably be from config

	ROLLUP_ID = 1 // todo [zkevm] this should be read from config to anticipate more than 1 rollup per manager contract
)

type VerifierRequest struct {
	BatchNumber uint64
	StateRoot   common.Hash
	CheckCount  int
}

type VerifierResponse struct {
	BatchNumber uint64
	Valid       bool
}

type ILegacyExecutor interface {
	Verify(*Payload, *common.Hash) (bool, error)
}

type WitnessGenerator interface {
	GenerateWitness(tx kv.Tx, ctx context.Context, startBlock, endBlock uint64, debug bool) ([]byte, error)
}

type LegacyExecutorVerifier struct {
	db            kv.RwDB
	cfg           ethconfig.Zk
	executors     []ILegacyExecutor
	executorLocks []*sync.Mutex
	available     *sync.Cond

	requestChan   chan *VerifierRequest
	responseChan  chan *VerifierResponse
	responses     []*VerifierResponse
	responseMutex *sync.Mutex
	quit          chan struct{}

	streamServer     *server.DataStreamServer
	witnessGenerator WitnessGenerator
	l1Syncer         *syncer.L1Syncer
	executorGrpc     executor.ExecutorServiceClient
	limbo            *Limbo

	unverifiedTxs []types.Transaction
	verifiedTxs   []types.Transaction
	uvtxsLock     sync.Mutex
	vtxsLock      sync.Mutex
}

func NewLegacyExecutorVerifier(
	cfg ethconfig.Zk,
	executors []ILegacyExecutor,
	chainCfg *chain.Config,
	db kv.RwDB,
	witnessGenerator WitnessGenerator,
	l1Syncer *syncer.L1Syncer,
	limbo *Limbo,
) *LegacyExecutorVerifier {
	executorLocks := make([]*sync.Mutex, len(executors))
	for i := range executorLocks {
		executorLocks[i] = &sync.Mutex{}
	}

	streamServer := server.NewDataStreamServer(nil, chainCfg.ChainID.Uint64(), server.ExecutorOperationMode)

	availableLock := sync.Mutex{}
	verifier := &LegacyExecutorVerifier{
		db:               db,
		cfg:              cfg,
		executors:        executors,
		executorLocks:    executorLocks,
		available:        sync.NewCond(&availableLock),
		requestChan:      make(chan *VerifierRequest, maximumInflightRequests),
		responseChan:     make(chan *VerifierResponse, maximumInflightRequests),
		responses:        make([]*VerifierResponse, 0),
		responseMutex:    &sync.Mutex{},
		quit:             make(chan struct{}),
		streamServer:     streamServer,
		witnessGenerator: witnessGenerator,
		l1Syncer:         l1Syncer,
		limbo:            limbo,
		unverifiedTxs:    make([]types.Transaction, 0),
		verifiedTxs:      make([]types.Transaction, 0),
		uvtxsLock:        sync.Mutex{},
		vtxsLock:         sync.Mutex{},
	}

	return verifier
}

func (v *LegacyExecutorVerifier) VerifySynchronously(tx kv.RwTx, request *VerifierRequest) (*VerifierResponse, error) {
	ctx := context.Background()
	return v.handleRequestSynchronously(ctx, tx, request)
}

func (v *LegacyExecutorVerifier) StopWork() {
	close(v.quit)
}

func (v *LegacyExecutorVerifier) StartWork() {
	go func() {
	LOOP:
		for {
			select {
			case <-v.quit:
				break LOOP
			case request := <-v.requestChan:
				go func() {
					ctx := context.Background()
					err := v.handleRequest(ctx, request)
					if err != nil {
						log.Error("[Verifier] error handling request", "err", err)

						// requeue the request, could be a transient error
						v.requestChan <- request
					}
				}()
			case response := <-v.responseChan:
				v.handleResponse(response)
			}
		}
	}()
}

func (v *LegacyExecutorVerifier) handleRequest(ctx context.Context, request *VerifierRequest) error {
	// if we have no executor config then just skip this step and treat everything as OK
	if len(v.executors) == 0 {
		response := &VerifierResponse{
			BatchNumber: request.BatchNumber,
			Valid:       true,
		}
		v.responseChan <- response
		return nil
	}

	// todo [zkevm] for now just using one executor but we need to use more
	execer := v.executors[0]

	// mapmutation has some issue with us not having a quit channel on the context call to `Done` so
	// here we're creating a cancelable context and just deferring the cancel
	innerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	tx, err := v.db.BeginRo(innerCtx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	hermezDb := hermez_db.NewHermezDbReader(tx)

	// get the data stream bytes
	blocks, err := hermezDb.GetL2BlockNosByBatch(request.BatchNumber)
	if err != nil {
		return err
	}

	// we might not have blocks yet as the underlying stage loop might still be running and the tx hasn't been
	// committed yet so just requeue the request
	if len(blocks) == 0 {
		request.CheckCount++
		v.requestChan <- request
		return nil
	}

	streamBytes, err := v.GetStreamBytes(request, tx, blocks, hermezDb)
	if err != nil {
		return err
	}

	witness, err := v.witnessGenerator.GenerateWitness(tx, innerCtx, blocks[0], blocks[len(blocks)-1], false)
	if err != nil {
		return err
	}

	log.Debug("witness generated", "data", hex.EncodeToString(witness))

	oldAccInputHash, err := v.l1Syncer.GetOldAccInputHash(innerCtx, &v.cfg.L1PolygonRollupManager, ROLLUP_ID, request.BatchNumber)
	if err != nil {
		return err
	}

	// now we need to figure out the timestamp limit for this payload.  It must be:
	// timestampLimit >= currentTimestamp (from batch pre-state) + deltaTimestamp
	// so to ensure we have a good value we can take the timestamp of the last block in the batch
	// and just add 5 minutes
	lastBlock, err := rawdb.ReadBlockByNumber(tx, blocks[len(blocks)-1])
	if err != nil {
		return err
	}
	timestampLimit := lastBlock.Time()

	payload := &Payload{
		Witness:           witness,
		DataStream:        streamBytes,
		Coinbase:          v.cfg.SequencerAddress.String(),
		OldAccInputHash:   oldAccInputHash.Bytes(),
		L1InfoRoot:        nil,
		TimestampLimit:    timestampLimit,
		ForcedBlockhashL1: []byte{0},
		ContextId:         strconv.Itoa(int(request.BatchNumber)),
	}

	// todo [zkevm] do something with the result but for now just move on in a happy state, we also need to handle errors
	success, err := execer.Verify(payload, &request.StateRoot)
	if err != nil {
		// TODO: could an error signify we need limbo or is it just a failure? (check)
	}

	// TODO [limbo]: later enable this once limbo is working
	//// if the verification failed, then set limbo state to true - the node is 'in limbo' but won't do anything about it until executor verification stage
	//// TODO: perhaps we can remove this from here and allow the verification stage alone to deal with it
	if !success {
		inLimbo, _ := v.limbo.CheckLimboMode()
		if !inLimbo {
			log.Debug("entering limbo!!!!!!")
			v.limbo.EnterLimboMode(request.BatchNumber)
		}
	}
	//
	//		// stop work and empty requests
	//		v.StopWork()
	//		close(v.requestChan)
	//		for range v.requestChan {
	//		}
	//	}
	//}

	response := &VerifierResponse{
		BatchNumber: request.BatchNumber,
		Valid:       success,
	}
	v.responseChan <- response

	return nil
}

func (v *LegacyExecutorVerifier) handleRequestSynchronously(ctx context.Context, tx kv.RwTx, request *VerifierRequest) (*VerifierResponse, error) {
	// if we have no executor config then just skip this step and treat everything as OK
	if len(v.executors) == 0 {
		response := &VerifierResponse{
			BatchNumber: request.BatchNumber,
			Valid:       true,
		}
		return response, nil
	}

	// todo [zkevm] for now just using one executor but we need to use more
	execer := v.executors[0]

	hermezDb := hermez_db.NewHermezDbReader(tx)

	// get the data stream bytes
	blocks, err := hermezDb.GetL2BlockNosByBatch(request.BatchNumber)
	if err != nil {
		return nil, err
	}

	streamBytes, err := v.GetStreamBytes(request, tx, blocks, hermezDb)
	if err != nil {
		return nil, err
	}

	witness, err := v.witnessGenerator.GenerateWitness(tx, ctx, blocks[0], blocks[len(blocks)-1], false)
	if err != nil {
		return nil, err
	}

	log.Debug("witness generated", "data", hex.EncodeToString(witness))

	oldAccInputHash, err := v.l1Syncer.GetOldAccInputHash(ctx, &v.cfg.L1PolygonRollupManager, ROLLUP_ID, request.BatchNumber)
	if err != nil {
		return nil, err
	}

	// now we need to figure out the timestamp limit for this payload.  It must be:
	// timestampLimit >= currentTimestamp (from batch pre-state) + deltaTimestamp
	// so to ensure we have a good value we can take the timestamp of the last block in the batch
	// and just add 5 minutes
	lastBlock, err := rawdb.ReadBlockByNumber(tx, blocks[len(blocks)-1])
	if err != nil {
		return nil, err
	}
	timestampLimit := lastBlock.Time()

	payload := &Payload{
		Witness:           witness,
		DataStream:        streamBytes,
		Coinbase:          v.cfg.SequencerAddress.String(),
		OldAccInputHash:   oldAccInputHash.Bytes(),
		L1InfoRoot:        nil,
		TimestampLimit:    timestampLimit,
		ForcedBlockhashL1: []byte{0},
		ContextId:         strconv.Itoa(int(request.BatchNumber)),
	}

	// todo [zkevm] do something with the result but for now just move on in a happy state, we also need to handle errors
	success, err := execer.Verify(payload, &request.StateRoot)
	if err != nil {
		// TODO: could an error signify we need limbo or is it just a failure? (check)
	}

	// if the verification failed, then set limbo state to true - the node is 'in limbo' but won't do anything about it until executor verification stage
	// TODO: perhaps we can remove this from here and allow the verification stage alone to deal with it
	//if !success {
	//	inLimbo, _ := v.limbo.CheckLimboMode()
	//	if !inLimbo {
	//		log.Debug("entering limbo!!!!!!")
	//		v.limbo.EnterLimboMode(request.BatchNumber)
	//
	//		// stop work and empty requests
	//		v.StopWork()
	//		close(v.requestChan)
	//		for range v.requestChan {
	//		}
	//	}
	//}

	response := &VerifierResponse{
		BatchNumber: request.BatchNumber,
		Valid:       success,
	}
	return response, nil
}

func (v *LegacyExecutorVerifier) GetStreamBytes(request *VerifierRequest, tx kv.Tx, blocks []uint64, hermezDb *hermez_db.HermezDbReader) ([]byte, error) {
	lastBlock, err := rawdb.ReadBlockByNumber(tx, blocks[0]-1)
	if err != nil {
		return nil, err
	}
	var streamBytes []byte
	for _, blockNumber := range blocks {
		block, err := rawdb.ReadBlockByNumber(tx, blockNumber)
		if err != nil {
			return nil, err
		}
		sBytes, err := v.streamServer.CreateAndBuildStreamEntryBytes(block, hermezDb, lastBlock, request.BatchNumber, true)
		if err != nil {
			return nil, err
		}
		streamBytes = append(streamBytes, sBytes...)
		lastBlock = block
	}
	return streamBytes, nil
}

func (v *LegacyExecutorVerifier) handleResponse(response *VerifierResponse) {
	v.responseMutex.Lock()
	defer v.responseMutex.Unlock()
	v.responses = append(v.responses, response)
}

func (v *LegacyExecutorVerifier) AddRequest(request *VerifierRequest) {
	v.responseMutex.Lock()
	defer v.responseMutex.Unlock()

	// check we don't already have a response for this to save doubling up work
	for _, response := range v.responses {
		if response.BatchNumber == request.BatchNumber {
			return
		}
	}

	v.requestChan <- request
}

func (v *LegacyExecutorVerifier) GetAllResponses() []*VerifierResponse {
	v.responseMutex.Lock()
	defer v.responseMutex.Unlock()
	result := make([]*VerifierResponse, len(v.responses))
	copy(result, v.responses)
	return result
}

func (v *LegacyExecutorVerifier) RemoveResponse(batchNumber uint64) {
	v.responseMutex.Lock()
	defer v.responseMutex.Unlock()

	result := make([]*VerifierResponse, 0, len(v.responses))
	for _, response := range v.responses {
		if response.BatchNumber != batchNumber {
			result = append(result, response)
		}
	}
	v.responses = result
}

func (v *LegacyExecutorVerifier) AddUnverifiedTxs(txs []types.Transaction) {
	v.uvtxsLock.Lock()
	defer v.uvtxsLock.Unlock()
	v.unverifiedTxs = append(v.unverifiedTxs, txs...)
}

func (v *LegacyExecutorVerifier) AddVerifiedTx(tx types.Transaction) {
	v.vtxsLock.Lock()
	defer v.vtxsLock.Unlock()
	v.verifiedTxs = append(v.verifiedTxs, tx)
}

func (v *LegacyExecutorVerifier) GetTxsForPool() []types.Transaction {
	v.uvtxsLock.Lock()
	defer v.uvtxsLock.Unlock()
	// [limbo] return all txs which were successful or not tested to be added back to the pool
	v.unverifiedTxs = append(v.unverifiedTxs, v.verifiedTxs...)
	return v.unverifiedTxs
}

func (v *LegacyExecutorVerifier) GetNextTx() []types.Transaction {
	v.uvtxsLock.Lock()
	defer v.uvtxsLock.Unlock()
	if len(v.unverifiedTxs) == 0 {
		return nil
	}
	result := make([]types.Transaction, 1)
	copy(result, v.unverifiedTxs[:1])
	v.unverifiedTxs = v.unverifiedTxs[1:]
	return result
}

func (v *LegacyExecutorVerifier) ClearTxs() {
	v.uvtxsLock.Lock()
	defer v.uvtxsLock.Unlock()
	v.unverifiedTxs = make([]types.Transaction, 0)
}
