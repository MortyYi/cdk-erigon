package legacy_executor_verifier

import (
	"github.com/ledgerwatch/erigon/core/types"
	"sync"
)

type Limbo struct {
	inLimboMode bool
	batchNo     uint64 // the lowest failing batch
	m           sync.Mutex
	txs         []types.Transaction
}

func NewLimbo() *Limbo {
	return &Limbo{
		inLimboMode: false,
		m:           sync.Mutex{},
		batchNo:     0,
	}
}

func (l *Limbo) EnterLimboMode(batchNo uint64) {
	l.m.Lock()
	defer l.m.Unlock()
	l.inLimboMode = true
	// record lowest batch no
	if l.batchNo == 0 || batchNo < l.batchNo {
		l.batchNo = batchNo
	}
}

func (l *Limbo) ExitLimboMode() {
	l.m.Lock()
	defer l.m.Unlock()
	l.batchNo = 0
	l.inLimboMode = false
}

func (l *Limbo) CheckLimboMode() (limboMode bool, batchNo uint64) {
	l.m.Lock()
	defer l.m.Unlock()
	return l.inLimboMode, l.batchNo
}
