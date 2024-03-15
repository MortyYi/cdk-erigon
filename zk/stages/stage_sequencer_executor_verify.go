package stages

import (
	"context"
	"sort"

	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/eth/stagedsync"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
	"github.com/ledgerwatch/erigon/zk/hermez_db"
	"github.com/ledgerwatch/erigon/zk/legacy_executor_verifier"
	"github.com/ledgerwatch/erigon/zk/txpool"
	"fmt"
	libcommon "github.com/ledgerwatch/erigon-lib/common"
	types2 "github.com/ledgerwatch/erigon-lib/types"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/zkevm/log"
)

type SequencerExecutorVerifyCfg struct {
	db       kv.RwDB
	verifier *legacy_executor_verifier.LegacyExecutorVerifier
	txPool   *txpool.TxPool
	limbo    *legacy_executor_verifier.Limbo
}

func StageSequencerExecutorVerifyCfg(
	db kv.RwDB,
	verifier *legacy_executor_verifier.LegacyExecutorVerifier,
	limbo *legacy_executor_verifier.Limbo,
) SequencerExecutorVerifyCfg {
	return SequencerExecutorVerifyCfg{
		db:       db,
		verifier: verifier,
		limbo:    limbo,
	}
}

func SpawnSequencerExecutorVerifyStage(
	s *stagedsync.StageState,
	u stagedsync.Unwinder,
	tx kv.RwTx,
	ctx context.Context,
	cfg SequencerExecutorVerifyCfg,
	initialCycle bool,
	quiet bool,
) error {
	var err error
	freshTx := tx == nil
	if freshTx {
		tx, err = cfg.db.BeginRw(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	hermezDb := hermez_db.NewHermezDb(tx)

	// progress here is at the batch level
	progress, err := stages.GetStageProgress(tx, stages.SequenceExecutorVerify)
	if err != nil {
		return err
	}

	inLimbo, limboBatch := cfg.limbo.CheckLimboMode()

	if inLimbo {
		log.Info("in limbo mode", "batch", limboBatch)
	}

	// [limbo] if not in limbo, then send via channel
	if !inLimbo {
		// get the latest responses from the verifier then sort them, so we can make sure we're handling verifications
		// in order
		responses := cfg.verifier.GetAllResponses()

		// sort responses by batch number in ascending order
		sort.Slice(responses, func(i, j int) bool {
			return responses[i].BatchNumber < responses[j].BatchNumber
		})

		failingBatches := make([]uint64, 0)

		for _, response := range responses {
			// ensure that the first response is the next batch based on the current stage progress
			// otherwise just return early until we get it
			if response.BatchNumber != progress+1 {
				return nil
			}

			// now check that we are indeed in a good state to continue
			if !response.Valid {
				// [limbo] collect failing batches
				failingBatches = append(failingBatches, response.BatchNumber)
			}

			// now let the verifier know we have got this message, so it can release it
			cfg.verifier.RemoveResponse(response.BatchNumber)
			progress = response.BatchNumber
		}

		// [limbo] if we have any failing batches, we need to enter limbo mode at the lowest failing batch
		if len(failingBatches) > 0 {
			minBatch := failingBatches[0]
			// double check min just in case logic above changes
			for _, batch := range failingBatches {
				if batch < minBatch {
					minBatch = batch
				}
			}

			// [limbo] set limbo mode true at this batch
			cfg.limbo.EnterLimboMode(minBatch)

			// [limbo] set progress to the batch before the lowest failing batch
			progress = minBatch - 1

			// [limbo] unwind the node to the highest block in the batch before the lowest failing batch
			blockNo, err2 := hermezDb.GetHighestBlockInBatch(progress)
			if err2 != nil {
				return err2
			}

			// [limbo] unwind and exit the verification stage
			u.UnwindTo(blockNo, libcommon.Hash{})
			return nil
		}

		// update stage progress batch number to 'progress'
		if err = stages.SaveStageProgress(tx, stages.SequenceExecutorVerify, progress); err != nil {
			return err
		}
	}

	// progress here is at the block level
	intersProgress, err := stages.GetStageProgress(tx, stages.IntermediateHashes)
	if err != nil {
		return err
	}

	// we need to get the batch number for the latest block, so we can search for new batches to send for
	// verification
	intersBatch, err := hermezDb.GetBatchNoByL2Block(intersProgress)
	if err != nil {
		return err
	}

	// send off the new batches to the verifier to be processed
	for batch := progress + 1; batch <= intersBatch; batch++ {
		// we do not need to verify batch 1 as this is the injected batch so just updated progress and move on
		if batch == injectedBatchNumber {
			if err = stages.SaveStageProgress(tx, stages.SequenceExecutorVerify, injectedBatchNumber); err != nil {
				return err
			}
		} else {
			// we need the state root of the last block in the batch to send to the executor
			blocks, err := hermezDb.GetL2BlockNosByBatch(batch)
			if err != nil {
				return err
			}
			sort.Slice(blocks, func(i, j int) bool {
				return blocks[i] > blocks[j]
			})
			lastBlockNumber := blocks[0]
			block, err := rawdb.ReadBlockByNumber(tx, lastBlockNumber)
			if err != nil {
				return err
			}

			if inLimbo {
				result, err2 := cfg.verifier.VerifySynchronously(tx, &legacy_executor_verifier.VerifierRequest{BatchNumber: batch, StateRoot: block.Root()})
				if err2 != nil {
					return err2
				}

				if !result.Valid {
					txs := block.Body().Transactions
					if len(txs) > 1 {
						return fmt.Errorf("block %d has more than 1 tx in limbo", lastBlockNumber)
					}

					tx0 := txs[0]

					vtxs := cfg.verifier.GetTxsForPool()

					// find and remove tx from vtxs
					for i, vtx := range vtxs {
						if vtx.Hash() == tx0.Hash() {
							vtxs = append(vtxs[:i], vtxs[i+1:]...)
							break
						}
					}

					// add valid/assumed valid txs back to the txpool
					slots, err := addTransactionsToSlots(vtxs)
					if err != nil {
						return err
					}
					_, err = cfg.txPool.AddLocalTxs(ctx, slots, tx)
					if err != nil {
						return err
					}

					cfg.verifier.ClearTxs()
					cfg.limbo.ExitLimboMode()

					// unwind node to known last good block
					lastGoodBlockNumber, err := hermezDb.GetHighestBlockInBatch(limboBatch - 1)
					if err != nil {
						return err
					}
					u.UnwindTo(lastGoodBlockNumber, libcommon.Hash{})
					return nil
				}
			} else {
				cfg.verifier.AddRequest(&legacy_executor_verifier.VerifierRequest{BatchNumber: batch, StateRoot: block.Root()})
			}
		}
	}

	if freshTx {
		if err = tx.Commit(); err != nil {
			return err
		}
	}

	return nil
}

func UnwindSequencerExecutorVerifyStage(
	u *stagedsync.UnwindState,
	s *stagedsync.StageState,
	tx kv.RwTx,
	ctx context.Context,
	cfg SequencerExecutorVerifyCfg,
	initialCycle bool,
) error {
	return nil
}

func PruneSequencerExecutorVerifyStage(
	s *stagedsync.PruneState,
	tx kv.RwTx,
	cfg SequencerExecutorVerifyCfg,
	ctx context.Context,
	initialCycle bool,
) error {
	return nil
}

func addTransactionsToSlots(transactions []types.Transaction) (types2.TxSlots, error) {
	// todo [limbo] finish implementation
	slot := types2.TxSlots{
		Txs:     nil,
		Senders: nil,
		IsLocal: nil,
	}
	return slot, nil
}
