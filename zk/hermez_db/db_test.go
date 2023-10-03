package hermez_db

import (
	"context"
	"fmt"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/mdbx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

type IHermezDb interface {
	WriteSequence(uint64, uint64, common.Hash) error
	WriteVerification(uint64, uint64, common.Hash) error
}

func GetDbTx() kv.RwTx {
	dbi, _ := mdbx.NewTemporaryMdbx()
	tx, _ := dbi.BeginRw(context.Background())

	return tx
}

func TestNewHermezDb(t *testing.T) {
	tx := GetDbTx()
	db, err := NewHermezDb(tx)
	require.NoError(t, err)
	assert.NotNil(t, db)
}

func TestGetSequenceByL1Block(t *testing.T) {
	tx := GetDbTx()
	db, err := NewHermezDb(tx)
	require.NoError(t, err)

	require.NoError(t, db.WriteSequence(1, 1001, common.HexToHash("0xabc")))
	require.NoError(t, db.WriteSequence(2, 1002, common.HexToHash("0xdef")))

	info, err := db.GetSequenceByL1Block(1)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), info.L1BlockNo)
	assert.Equal(t, uint64(1001), info.BatchNo)
	assert.Equal(t, common.HexToHash("0xabc"), info.L1TxHash)

	info, err = db.GetSequenceByL1Block(2)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), info.L1BlockNo)
	assert.Equal(t, uint64(1002), info.BatchNo)
	assert.Equal(t, common.HexToHash("0xdef"), info.L1TxHash)
}

func TestGetSequenceByBatchNo(t *testing.T) {
	tx := GetDbTx()
	db, err := NewHermezDb(tx)
	require.NoError(t, err)

	require.NoError(t, db.WriteSequence(1, 1001, common.HexToHash("0xabc")))
	require.NoError(t, db.WriteSequence(2, 1002, common.HexToHash("0xdef")))

	info, err := db.GetSequenceByBatchNo(1001)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), info.L1BlockNo)
	assert.Equal(t, uint64(1001), info.BatchNo)
	assert.Equal(t, common.HexToHash("0xabc"), info.L1TxHash)

	info, err = db.GetSequenceByBatchNo(1002)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), info.L1BlockNo)
	assert.Equal(t, uint64(1002), info.BatchNo)
	assert.Equal(t, common.HexToHash("0xdef"), info.L1TxHash)
}

func TestGetVerificationByL1BlockAndBatchNo(t *testing.T) {
	tx := GetDbTx()
	db, err := NewHermezDb(tx)
	require.NoError(t, err)

	require.NoError(t, db.WriteVerification(3, 1003, common.HexToHash("0xghi")))
	require.NoError(t, db.WriteVerification(4, 1004, common.HexToHash("0xjkl")))

	info, err := db.GetVerificationByL1Block(3)
	require.NoError(t, err)
	assert.Equal(t, uint64(3), info.L1BlockNo)
	assert.Equal(t, uint64(1003), info.BatchNo)
	assert.Equal(t, common.HexToHash("0xghi"), info.L1TxHash)

	info, err = db.GetVerificationByBatchNo(1004)
	require.NoError(t, err)
	assert.Equal(t, uint64(4), info.L1BlockNo)
	assert.Equal(t, uint64(1004), info.BatchNo)
	assert.Equal(t, common.HexToHash("0xjkl"), info.L1TxHash)
}

func TestGetAndSetLatest(t *testing.T) {

	testCases := []struct {
		desc          string
		table         string
		writeMethod   func(IHermezDb, uint64, uint64, common.Hash) error
		l1BlockNo     uint64
		batchNo       uint64
		l1TxHashBytes common.Hash
	}{
		{"sequence 1", L1SEQUENCES, IHermezDb.WriteSequence, 1, 1001, common.HexToHash("0xabc")},
		{"sequence 2", L1SEQUENCES, IHermezDb.WriteSequence, 2, 1002, common.HexToHash("0xdef")},
		{"verification 1", L1VERIFICATIONS, IHermezDb.WriteVerification, 3, 1003, common.HexToHash("0xghi")},
		{"verification 2", L1VERIFICATIONS, IHermezDb.WriteVerification, 4, 1004, common.HexToHash("0xjkl")},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			tx := GetDbTx()
			db, err := NewHermezDb(tx)
			require.NoError(t, err)
			err = tc.writeMethod(db, tc.l1BlockNo, tc.batchNo, tc.l1TxHashBytes)
			assert.Nil(t, err)

			info, err := db.getLatest(tc.table)
			assert.Nil(t, err)
			assert.Equal(t, tc.batchNo, info.BatchNo)
			assert.Equal(t, tc.l1BlockNo, info.L1BlockNo)
			assert.Equal(t, tc.l1TxHashBytes, info.L1TxHash)
		})
	}
}

func TestGetAndSetForkId(t *testing.T) {

	testCases := []struct {
		batchNo uint64
		forkId  uint64
	}{
		{9, 0},    // batchNo < 10 -> forkId = 0
		{10, 1},   // batchNo = 10 -> forkId = 1
		{11, 1},   // batchNo > 10 -> forkId = 1
		{99, 1},   // batchNo < 100 -> forkId = 1
		{100, 2},  // batchNo >= 100 -> forkId = 2
		{1000, 2}, // batchNo > 100 -> forkId = 2
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("BatchNo: %d ForkId: %d", tc.batchNo, tc.forkId), func(t *testing.T) {
			tx := GetDbTx()
			db, err := NewHermezDb(tx)
			require.NoError(t, err)

			err = db.WriteForkId(10, 1)
			require.NoError(t, err, "Failed to write ForkId")
			err = db.WriteForkId(100, 2)
			require.NoError(t, err, "Failed to write ForkId")

			fetchedForkId, err := db.GetForkId(tc.batchNo)
			require.NoError(t, err, "Failed to get ForkId")
			assert.Equal(t, tc.forkId, fetchedForkId, "Fetched ForkId doesn't match expected")
		})
	}
}

// Benchmarks

func BenchmarkWriteSequence(b *testing.B) {
	tx := GetDbTx()
	db, err := NewHermezDb(tx)
	require.NoError(b, err)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		err := db.WriteSequence(uint64(i), uint64(i+1000), common.HexToHash("0xabc"))
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteVerification(b *testing.B) {
	tx := GetDbTx()
	db, err := NewHermezDb(tx)
	require.NoError(b, err)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		err := db.WriteVerification(uint64(i), uint64(i+2000), common.HexToHash("0xdef"))
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGetSequenceByL1Block(b *testing.B) {
	tx := GetDbTx()
	db, err := NewHermezDb(tx)
	require.NoError(b, err)

	for i := 0; i < 1000; i++ {
		err := db.WriteSequence(uint64(i), uint64(i+1000), common.HexToHash("0xabc"))
		if err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := db.GetSequenceByL1Block(uint64(i % 1000))
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGetVerificationByL1Block(b *testing.B) {
	tx := GetDbTx()
	db, err := NewHermezDb(tx)
	require.NoError(b, err)

	for i := 0; i < 1000; i++ {
		err := db.WriteVerification(uint64(i), uint64(i+2000), common.HexToHash("0xdef"))
		if err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := db.GetVerificationByL1Block(uint64(i % 1000))
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGetSequenceByBatchNo(b *testing.B) {
	tx := GetDbTx()
	db, err := NewHermezDb(tx)
	require.NoError(b, err)

	for i := 0; i < 1000; i++ {
		err := db.WriteSequence(uint64(i), uint64(i+1000), common.HexToHash("0xabc"))
		if err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := db.GetSequenceByBatchNo(uint64(i % 1000))
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGetVerificationByBatchNo(b *testing.B) {
	tx := GetDbTx()
	db, err := NewHermezDb(tx)
	require.NoError(b, err)

	for i := 0; i < 1000; i++ {
		err := db.WriteVerification(uint64(i), uint64(i+2000), common.HexToHash("0xdef"))
		if err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := db.GetVerificationByBatchNo(uint64(i % 1000))
		if err != nil {
			b.Fatal(err)
		}
	}
}
