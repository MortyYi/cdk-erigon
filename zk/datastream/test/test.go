package main

import (
	"fmt"
	"strconv"

	"github.com/ledgerwatch/erigon/zk/datastream"
	"github.com/ledgerwatch/erigon/zk/datastream/client"
	"github.com/ledgerwatch/erigon/zk/datastream/test/utils"
	"github.com/ledgerwatch/erigon/zk/datastream/types"
	"github.com/ledgerwatch/erigon/zkevm/log"
)

// This code downloads headers and blocks from a datastream server.
func main() {
	// Create client
	c := client.NewClient(datastream.TestDatastreamUrl)

	// Start client (connect to the server)
	defer c.Stop()
	if err := c.Start(); err != nil {
		panic(err)
	}

	// Get header from server
	if err := c.GetHeader(); err != nil {
		panic(err)
	}

	// Read all entries from server 38006
	blocksRead, bookmarks, entriesReadAmount, err := c.ReadEntries(0, 1)
	if err != nil {
		panic(err)
	}
	fmt.Println(bookmarks)
	fmt.Println("Entries read amount: ", entriesReadAmount)
	fmt.Println("Blocks read amount: ", len(*blocksRead))

	for i, dsBlock := range *blocksRead {
		if i == 0 {
			continue
		}
		log.Info("Block number: ", dsBlock.L2BlockNumber)
		rpcBlock, err := utils.GetBlockByHash(dsBlock.L2Blockhash.String())
		if err != nil {
			panic(err)
		}

		match := matchBlocks(dsBlock, rpcBlock)
		if !match {
			log.Error("Blocks don't match")
			return
		}
	}
}

func matchBlocks(dsBlock types.FullL2Block, rpcBlock utils.Result) bool {
	decimal_num, err := strconv.ParseUint(rpcBlock.Number[2:], 16, 64)
	if err != nil {
		log.Errorf("Error parsing block number. Error: %v, BlockNumber: %d, rpcBlockNumber: %d", err, dsBlock.L2BlockNumber, rpcBlock.Number)
		return false
	}

	if decimal_num != dsBlock.L2BlockNumber {
		log.Errorf("Block numbers don't match. BlockNumber: %d, rpcBlockNumber: %d", dsBlock.L2BlockNumber, decimal_num)
		return false
	}

	if rpcBlock.StateRoot != dsBlock.StateRoot.String() {
		log.Errorf("Block state roots don't match. BlockNumber: %d, dsBlockStateRoot: %s, rpcBlockStateRoot: %s", dsBlock.L2BlockNumber, dsBlock.StateRoot.String(), rpcBlock.StateRoot)
		return false
	}

	decimal_timestamp, err := strconv.ParseUint(rpcBlock.Timestamp[2:], 16, 64)
	if err != nil {
		log.Errorf("Error parsing block timestamp. Error: %v, BlockNumber: %d, rpcBlockTimestamp: %d", err, dsBlock.L2BlockNumber, rpcBlock.Timestamp)
		return false
	}

	if decimal_timestamp != uint64(dsBlock.Timestamp) {
		log.Errorf("Block timestamps don't match. BlockNumber: %d, dsBlockTimestamp: %d, rpcBlockTimestamp: %d", dsBlock.L2BlockNumber, dsBlock.Timestamp, decimal_timestamp)
		return false
	}

	if len(dsBlock.L2Txs) != len(rpcBlock.Transactions) {
		log.Errorf("Block txs don't match. BlockNumber: %d, dsBlockTxs: %d, rpcBlockTxs: %d", dsBlock.L2BlockNumber, len(dsBlock.L2Txs), len(rpcBlock.Transactions))
		return false
	}

	// for i, tx := range dsBlock.L2Txs {
	// 	if tx..String() != rpcBlock.Transactions[i] {
	// 		log.Error("Block txs don't match", "blockNumber", dsBlock.L2BlockNumber, "dsBlockTx", tx.String(), "rpcBlockTx", rpcBlock.Transactions[i])
	// 		return false
	// 	}
	// }

	return true
}