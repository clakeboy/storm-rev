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
	var err error

	n.tx, err = n.s.Bolt.Begin(writable)
	if err != nil {
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
	n.tx = nil
	n.txIndexDirty = nil
	n.txIndexDrops = nil
	n.txIndexRecords = nil
	n.txIndexDeletes = nil
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
	err := n.tx.Commit()
	n.tx = nil
	n.txIndexDirty = nil
	n.txIndexDrops = nil
	n.txIndexRecords = nil
	n.txIndexDeletes = nil
	if err != nil {
		return err
	}
	for tableName := range drops {
		if err := n.s.indexer.dropTable(tableName); err != nil {
			return err
		}
		delete(dirty, tableName)
		delete(records, tableName)
		delete(deletes, tableName)
	}
	for tableName, cfg := range dirty {
		if err := n.ReIndex(reflect.New(cfg.Type).Interface()); err != nil {
			n.s.indexer.markDirty(cfg.Name)
			return err
		}
		delete(records, tableName)
		delete(deletes, tableName)
	}
	for _, tableDeletes := range deletes {
		if err := n.deleteIndexedRecords(tableDeletes); err != nil {
			return err
		}
	}
	for _, tableRecords := range records {
		if err := n.indexSavedRecords(tableRecords); err != nil {
			return err
		}
	}
	// if err == bolt.ErrTxClosed {
	// 	return ErrNotInTransaction
	// }

	return nil
}
