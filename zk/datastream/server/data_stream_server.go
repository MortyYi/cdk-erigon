package server

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/0xPolygonHermez/zkevm-data-streamer/datastreamer"
	libcommon "github.com/ledgerwatch/erigon-lib/common"
	eritypes "github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/zk/datastream/types"
	"github.com/ledgerwatch/erigon/zk/hermez_db"
)

type BookmarkType byte

var BlockBookmarkType BookmarkType = 0

type OperationMode int

const (
	StandardOperationMode OperationMode = iota
	ExecutorOperationMode
)

var entryTypeMappings = map[types.EntryType]datastreamer.EntryType{
	types.EntryTypeStartL2Block: datastreamer.EntryType(1),
	types.EntryTypeL2Tx:         datastreamer.EntryType(2),
	types.EntryTypeEndL2Block:   datastreamer.EntryType(3),
	types.EntryTypeBookmark:     datastreamer.EntryType(176),
}

type DataStreamServer struct {
	stream  *datastreamer.StreamServer
	chainId uint64
	mode    OperationMode
}

type DataStreamEntry interface {
	EntryType() types.EntryType
	Bytes(bigEndian bool) []byte
}

func NewDataStreamServer(stream *datastreamer.StreamServer, chainId uint64, mode OperationMode) *DataStreamServer {
	return &DataStreamServer{
		stream:  stream,
		chainId: chainId,
		mode:    mode,
	}
}

func (srv *DataStreamServer) CommitEntriesToStream(entries []DataStreamEntry, bigEndian bool) error {
	for _, entry := range entries {
		entryType := entry.EntryType()
		if entryType == types.EntryTypeBookmark {
			_, err := srv.stream.AddStreamBookmark(entry.Bytes(bigEndian))
			if err != nil {
				return err
			}
		} else {
			mapped, ok := entryTypeMappings[entryType]
			if !ok {
				return fmt.Errorf("unsupported stream entry type: %v", entryType)
			}
			_, err := srv.stream.AddStreamEntry(mapped, entry.Bytes(bigEndian))
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (srv *DataStreamServer) CreateBookmarkEntry(t BookmarkType, marker uint64) *types.Bookmark {
	return &types.Bookmark{Type: byte(t), From: marker}
}

func (srv *DataStreamServer) CreateBlockStartEntry(block *eritypes.Block, batchNumber uint64, forkId uint16, ger libcommon.Hash, deltaTimestamp uint32, l1InfoIndex uint32, l1BlockHash libcommon.Hash) *types.StartL2Block {
	return &types.StartL2Block{
		BatchNumber:     batchNumber,
		L2BlockNumber:   block.NumberU64(),
		Timestamp:       int64(block.Time()),
		DeltaTimestamp:  deltaTimestamp,
		L1InfoTreeIndex: l1InfoIndex,
		L1BlockHash:     l1BlockHash,
		GlobalExitRoot:  ger,
		Coinbase:        block.Coinbase(),
		ForkId:          forkId,
		ChainId:         uint32(srv.chainId),
	}
}

func (srv *DataStreamServer) CreateBlockEndEntry(blockNumber uint64, blockHash, stateRoot libcommon.Hash) *types.EndL2Block {
	return &types.EndL2Block{
		L2BlockNumber: blockNumber,
		L2Blockhash:   blockHash,
		StateRoot:     stateRoot,
	}
}

func (srv *DataStreamServer) CreateTransactionEntry(
	effectiveGasPricePercentage uint8,
	stateRoot libcommon.Hash,
	fork uint16,
	tx eritypes.Transaction,
) (*types.L2Transaction, error) {
	buf := make([]byte, 0)
	writer := bytes.NewBuffer(buf)
	err := tx.EncodeRLP(writer)
	if err != nil {
		return nil, err
	}

	encoded := writer.Bytes()

	// we only want to append the effective price when not running in an executor context
	if fork >= 5 && srv.mode != ExecutorOperationMode {
		encoded = append(encoded, effectiveGasPricePercentage)
	}

	length := len(encoded)

	return &types.L2Transaction{
		EffectiveGasPricePercentage: effectiveGasPricePercentage,
		IsValid:                     1, // TODO: SEQ: we don't store this value anywhere currently as a sync node
		StateRoot:                   stateRoot,
		EncodedLength:               uint32(length),
		Encoded:                     encoded,
	}, nil
}

func (srv *DataStreamServer) CreateAndCommitEntriesToStream(
	block *eritypes.Block,
	reader *hermez_db.HermezDbReader,
	lastBlock *eritypes.Block,
	batchNumber uint64,
	bigEndian bool,
) error {
	entries, err := srv.CreateStreamEntries(block, reader, lastBlock, batchNumber)
	if err != nil {
		return err
	}
	return srv.CommitEntriesToStream(entries, bigEndian)
}

func (srv *DataStreamServer) CreateStreamEntries(
	block *eritypes.Block,
	reader *hermez_db.HermezDbReader,
	lastBlock *eritypes.Block,
	batchNumber uint64,
) ([]DataStreamEntry, error) {
	fork, err := reader.GetForkId(batchNumber)
	if err != nil {
		return nil, err
	}

	var entries []DataStreamEntry

	bookmark := srv.CreateBookmarkEntry(BlockBookmarkType, block.NumberU64())
	entries = append(entries, bookmark)

	deltaTimestamp := block.Time() - lastBlock.Time()

	var ger libcommon.Hash
	var l1BlockHash libcommon.Hash

	l1Index, err := reader.GetBlockL1InfoTreeIndex(block.NumberU64())
	if err != nil {
		return nil, err
	}

	if block.NumberU64() == 1 {
		// injected batch at the start of the network
		injected, err := reader.GetL1InjectedBatch(0)
		if err != nil {
			return nil, err
		}
		ger = injected.LastGlobalExitRoot
		l1BlockHash = injected.L1ParentHash

		// block 1 in the stream has a delta timestamp of the block time itself
		deltaTimestamp = block.Time()
	} else {
		// standard behaviour for non-injected or forced batches
		if l1Index != 0 {
			// read the index info itself
			l1Info, err := reader.GetL1InfoTreeUpdate(l1Index)
			if err != nil {
				return nil, err
			}
			if l1Info != nil {
				ger = l1Info.GER
				l1BlockHash = l1Info.ParentHash
			}
		}
	}

	if srv.mode == ExecutorOperationMode {
		deltaTimestamp = block.Time()
	}

	blockStart := srv.CreateBlockStartEntry(block, batchNumber, uint16(fork), ger, uint32(deltaTimestamp), uint32(l1Index), l1BlockHash)
	entries = append(entries, blockStart)

	for _, tx := range block.Transactions() {
		effectiveGasPricePercentage, err := reader.GetEffectiveGasPricePercentage(tx.Hash())
		if err != nil {
			return nil, err
		}
		stateRoot, err := reader.GetStateRoot(block.NumberU64())
		if err != nil {
			return nil, err
		}
		transaction, err := srv.CreateTransactionEntry(effectiveGasPricePercentage, stateRoot, uint16(fork), tx)
		if err != nil {
			return nil, err
		}
		entries = append(entries, transaction)
	}

	blockEnd := srv.CreateBlockEndEntry(block.NumberU64(), block.Root(), block.Root())
	entries = append(entries, blockEnd)

	return entries, nil
}

func (srv *DataStreamServer) CreateAndBuildStreamEntryBytes(
	block *eritypes.Block,
	reader *hermez_db.HermezDbReader,
	lastBlock *eritypes.Block,
	batchNumber uint64,
	bigEndian bool,
) ([]byte, error) {
	entries, err := srv.CreateStreamEntries(block, reader, lastBlock, batchNumber)
	if err != nil {
		return nil, err
	}

	var result []byte
	for _, entry := range entries {
		b := encodeEntryToBytes(entry, bigEndian)
		result = append(result, b...)
	}

	return result, nil
}

const (
	PACKET_TYPE_DATA = 2
	// NOOP_ENTRY_NUMBER is used because we don't care about the entry number when feeding an atrificial
	// stream to the executor, if this ever changes then we'll need to populate an actual number
	NOOP_ENTRY_NUMBER = 0
)

func encodeEntryToBytes(entry DataStreamEntry, bigEndian bool) []byte {
	data := entry.Bytes(bigEndian)
	var totalLength = 1 + 4 + 4 + 8 + uint32(len(data))
	buf := make([]byte, 1)
	buf[0] = PACKET_TYPE_DATA
	if bigEndian {
		buf = binary.BigEndian.AppendUint32(buf, totalLength)
		buf = binary.BigEndian.AppendUint32(buf, uint32(entry.EntryType()))
		buf = binary.BigEndian.AppendUint64(buf, uint64(NOOP_ENTRY_NUMBER))
	} else {
		buf = binary.LittleEndian.AppendUint32(buf, totalLength)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(entry.EntryType()))
		buf = binary.LittleEndian.AppendUint64(buf, uint64(NOOP_ENTRY_NUMBER))
	}
	buf = append(buf, data...)
	return buf
}
