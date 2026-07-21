package storm

import "reflect"

// Tx is a transaction.
type Tx interface {
	// Commit writes all changes to disk.
	Commit() error

	// Rollback closes the transaction and ignores all previous updates.
	Rollback() error
}

// Begin starts a new transaction.
func (n node) Begin(writable bool) (Node, error) {
	if writable {
		n.s.indexCommitMu.Lock()
		n.txState = &transactionState{indexCommitLocked: true}
	}

	var err error
	n.tx, err = n.s.Bolt.Begin(writable)
	if err != nil {
		n.releaseTransactionIndexCommitLock()
		n.txState = nil
		return nil, err
	}
	n.txIndexDirty = make(map[string]*structConfig)
	n.txIndexDrops = make(map[string]bool)
	n.txIndexRecords = make(map[string][]*savedRecord)
	n.txIndexDeletes = make(map[string][]*deletedRecord)

	return &n, nil
}

// Rollback closes the transaction and ignores all previous updates.
func (n *node) Rollback() error {
	if n.tx == nil {
		return ErrNotInTransaction
	}

	err := n.tx.Rollback()
	n.clearTransaction()
	n.releaseTransactionIndexCommitLock()
	n.txState = nil
	// if err == bolt.ErrTxClosed {
	// 	return ErrNotInTransaction
	// }

	return err
}

// Commit writes all changes to disk.
func (n *node) Commit() error {
	if n.tx == nil {
		return ErrNotInTransaction
	}

	dirty := n.txIndexDirty
	drops := n.txIndexDrops
	records := n.txIndexRecords
	deletes := n.txIndexDeletes
	hasIndexWork := len(dirty) > 0 || len(drops) > 0 || len(records) > 0 || len(deletes) > 0

	var dropNames []string
	for tableName := range drops {
		dropNames = append(dropNames, tableName)
		delete(dirty, tableName)
		delete(records, tableName)
		delete(deletes, tableName)
	}
	for tableName := range dirty {
		delete(records, tableName)
		delete(deletes, tableName)
	}
	var pendingRecords []*savedRecord
	for _, tableRecords := range records {
		pendingRecords = append(pendingRecords, tableRecords...)
	}
	var pendingDeletes []*deletedRecord
	for _, tableDeletes := range deletes {
		pendingDeletes = append(pendingDeletes, tableDeletes...)
	}

	job, err := persistDurableIndexJob(n.tx, n.rootBucket, pendingRecords, pendingDeletes, dropNames)
	if err != nil {
		_ = n.tx.Rollback()
	} else {
		err = n.tx.Commit()
	}
	n.clearTransaction()
	if err != nil {
		n.releaseTransactionIndexCommitLock()
		n.txState = nil
		return err
	}
	if !hasIndexWork {
		n.releaseTransactionIndexCommitLock()
		n.txState = nil
		return nil
	}
	req := n.s.indexer.submitDurableIndexJob(job)
	n.releaseTransactionIndexCommitLock()
	n.txState = nil
	if !n.s.bleveAsync {
		if err := req.wait(); err != nil {
			return err
		}
	}
	for _, cfg := range dirty {
		if err := n.ReIndex(reflect.New(cfg.Type).Interface()); err != nil {
			n.s.indexer.markDirty(cfg.Name)
			return err
		}
	}
	return nil
}

func (n *node) releaseTransactionIndexCommitLock() {
	if n.txState == nil || !n.txState.indexCommitLocked {
		return
	}
	n.txState.indexCommitLocked = false
	n.s.indexCommitMu.Unlock()
}

func (n *node) clearTransaction() {
	n.tx = nil
	n.txIndexDirty = nil
	n.txIndexDrops = nil
	n.txIndexRecords = nil
	n.txIndexDeletes = nil
}
