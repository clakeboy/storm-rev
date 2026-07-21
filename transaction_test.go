package storm

import (
	"context"
	"testing"
	"time"

	"github.com/clakeboy/storm-rev/v2/q"
	"github.com/stretchr/testify/require"
)

func TestTransaction(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	err := db.Rollback()
	require.Error(t, err)

	err = db.Commit()
	require.Error(t, err)

	tx, err := db.Begin(true)
	require.NoError(t, err)

	ntx, ok := tx.(*node)
	require.True(t, ok)
	require.NotNil(t, ntx.tx)

	err = tx.Init(&SimpleUser{})
	require.NoError(t, err)

	err = tx.Save(&User{ID: 10, Name: "John"})
	require.NoError(t, err)

	err = tx.Save(&User{ID: 20, Name: "John"})
	require.NoError(t, err)

	err = tx.Save(&User{ID: 30, Name: "Steve"})
	require.NoError(t, err)

	var user User
	err = tx.One("ID", 10, &user)
	require.NoError(t, err)

	var users []User
	err = tx.AllByIndex("Name", &users)
	require.NoError(t, err)
	require.Len(t, users, 3)

	err = tx.All(&users)
	require.NoError(t, err)
	require.Len(t, users, 3)

	err = tx.Find("Name", "Steve", &users)
	require.NoError(t, err)
	require.Len(t, users, 1)

	err = tx.DeleteStruct(&user)
	require.NoError(t, err)

	err = tx.One("ID", 10, &user)
	require.Error(t, err)

	err = tx.Set("b1", "best friend's mail", "mail@provider.com")
	require.NoError(t, err)

	var str string
	err = tx.Get("b1", "best friend's mail", &str)
	require.NoError(t, err)
	require.Equal(t, "mail@provider.com", str)

	err = tx.Delete("b1", "best friend's mail")
	require.NoError(t, err)

	err = tx.Get("b1", "best friend's mail", &str)
	require.Error(t, err)

	err = tx.Commit()
	require.NoError(t, err)

	err = tx.Commit()
	require.Error(t, err)
	require.Equal(t, ErrNotInTransaction, err)

	err = db.One("ID", 30, &user)
	require.NoError(t, err)
	require.Equal(t, 30, user.ID)
}

func TestTransactionRollback(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	tx, err := db.Begin(true)
	require.NoError(t, err)

	err = tx.Save(&User{ID: 10, Name: "John"})
	require.NoError(t, err)

	var user User
	err = tx.One("ID", 10, &user)
	require.NoError(t, err)
	require.Equal(t, 10, user.ID)

	err = tx.Rollback()
	require.NoError(t, err)

	err = db.One("ID", 10, &user)
	require.Error(t, err)
}

func TestWritableTransactionDoesNotDeadlockConcurrentSave(t *testing.T) {
	db, cleanup := createDB(t, BleveAsyncWrites())
	defer cleanup()

	tx, err := db.Begin(true)
	require.NoError(t, err)
	require.NoError(t, tx.Save(&IndexedNameUser{ID: 1, Name: "transaction"}))

	saveStarted := make(chan struct{})
	saveDone := make(chan error, 1)
	go func() {
		close(saveStarted)
		saveDone <- db.Save(&IndexedNameUser{ID: 2, Name: "ordinary"})
	}()
	<-saveStarted

	// Give the ordinary Save enough time to reach the ordering gate. Before the
	// fix it acquired indexCommitMu and then waited for this transaction's Bolt
	// writer, while Commit waited for indexCommitMu.
	time.Sleep(50 * time.Millisecond)
	commitDone := make(chan error, 1)
	go func() { commitDone <- tx.Commit() }()

	select {
	case err := <-commitDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "explicit Commit deadlocked with ordinary Save")
	}
	select {
	case err := <-saveDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "ordinary Save did not resume after explicit Commit")
	}

	require.NoError(t, db.FlushBleve(context.Background()))
	var users []IndexedNameUser
	require.NoError(t, db.Find("Name", "transaction", &users))
	require.Len(t, users, 1)
	require.NoError(t, db.Find("Name", "ordinary", &users))
	require.Len(t, users, 1)
}

func TestWritableTransactionReleasesIndexCommitLock(t *testing.T) {
	t.Run("commit without index work", func(t *testing.T) {
		db, cleanup := createDB(t)
		defer cleanup()
		tx, err := db.Begin(true)
		require.NoError(t, err)
		require.NoError(t, tx.Set("raw", "key", "value"))
		require.NoError(t, tx.Commit())
		requireIndexCommitUnlocked(t, db)
	})

	t.Run("rollback", func(t *testing.T) {
		db, cleanup := createDB(t)
		defer cleanup()
		tx, err := db.Begin(true)
		require.NoError(t, err)
		require.NoError(t, tx.Rollback())
		requireIndexCommitUnlocked(t, db)
	})

	t.Run("commit error", func(t *testing.T) {
		db, cleanup := createDB(t)
		defer cleanup()
		tx, err := db.Begin(true)
		require.NoError(t, err)
		n := tx.(*node)
		require.NoError(t, n.tx.Rollback())
		require.Error(t, tx.Commit())
		requireIndexCommitUnlocked(t, db)
	})

	t.Run("begin error", func(t *testing.T) {
		db, cleanup := createDB(t)
		defer cleanup()
		require.NoError(t, db.Close())
		_, err := db.Begin(true)
		require.Error(t, err)
		requireIndexCommitUnlocked(t, db)
	})
}

func requireIndexCommitUnlocked(t *testing.T, db *DB) {
	t.Helper()
	locked := db.indexCommitMu.TryLock()
	require.True(t, locked, "indexCommitMu remained locked")
	if locked {
		db.indexCommitMu.Unlock()
	}
}

func TestTransactionSaveAll(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	tx, err := db.Begin(true)
	require.NoError(t, err)

	err = tx.SaveAll([]IndexedNameUser{
		{ID: 1, Name: "John"},
		{ID: 2, Name: "John"},
	})
	require.NoError(t, err)

	var inTx []IndexedNameUser
	err = tx.Find("Name", "John", &inTx)
	require.NoError(t, err)
	require.Len(t, inTx, 2)

	require.NoError(t, tx.Commit())

	var committed []IndexedNameUser
	err = db.Find("Name", "John", &committed)
	require.NoError(t, err)
	require.Len(t, committed, 2)

	tx, err = db.Begin(true)
	require.NoError(t, err)
	err = tx.SaveAll([]IndexedNameUser{{ID: 3, Name: "Rollback"}})
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())

	var rolledBack []IndexedNameUser
	err = db.Find("Name", "Rollback", &rolledBack)
	require.Equal(t, ErrNotFound, err)
}

func TestTransactionDeleteStructUsesIncrementalIndexDelete(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Save(&IndexedNameUser{ID: 1, Name: "John"}))
	require.NoError(t, db.Save(&IndexedNameUser{ID: 2, Name: "John"}))

	tx, err := db.Begin(true)
	require.NoError(t, err)

	err = tx.DeleteStruct(&IndexedNameUser{ID: 1})
	require.NoError(t, err)

	ntx := tx.(*node)
	require.Empty(t, ntx.txIndexDirty)
	require.Len(t, ntx.txIndexDeletes["IndexedNameUser"], 1)

	require.NoError(t, tx.Commit())
	require.NoError(t, db.FlushBleve(context.Background()))

	var users []IndexedNameUser
	err = db.Find("Name", "John", &users)
	require.NoError(t, err)
	require.Len(t, users, 1)
	require.Equal(t, 2, users[0].ID)

	var deleted IndexedNameUser
	require.Equal(t, ErrNotFound, db.One("ID", 1, &deleted))
}

func TestTransactionQueryDeleteUsesIncrementalIndexDelete(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Save(&IndexedNameUser{ID: 1, Name: "John"}))
	require.NoError(t, db.Save(&IndexedNameUser{ID: 2, Name: "John"}))
	require.NoError(t, db.Save(&IndexedNameUser{ID: 3, Name: "Jane"}))

	tx, err := db.Begin(true)
	require.NoError(t, err)

	err = tx.Select(q.Eq("Name", "John")).Delete(&IndexedNameUser{})
	require.NoError(t, err)

	ntx := tx.(*node)
	require.Empty(t, ntx.txIndexDirty)
	require.Len(t, ntx.txIndexDeletes["IndexedNameUser"], 2)

	require.NoError(t, tx.Commit())
	require.NoError(t, db.FlushBleve(context.Background()))

	var users []IndexedNameUser
	err = db.Find("Name", "John", &users)
	require.Equal(t, ErrNotFound, err)
	require.False(t, db.indexer.isDirty("IndexedNameUser"))

	require.NoError(t, db.Find("Name", "Jane", &users))
	require.Len(t, users, 1)
	require.Equal(t, 3, users[0].ID)
}

func TestTransactionMixedSaveUpdateDeleteIndexes(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Save(&IndexedNameUser{ID: 1, Name: "DeleteMe"}))
	require.NoError(t, db.Save(&IndexedNameUser{ID: 2, Name: "UpdateMe"}))

	tx, err := db.Begin(true)
	require.NoError(t, err)
	require.NoError(t, tx.Save(&IndexedNameUser{ID: 3, Name: "Created"}))
	require.NoError(t, tx.UpdateField(&IndexedNameUser{ID: 2}, "Name", "Updated"))
	require.NoError(t, tx.DeleteStruct(&IndexedNameUser{ID: 1}))
	require.NoError(t, tx.Commit())

	var users []IndexedNameUser
	require.NoError(t, db.Find("Name", "Created", &users))
	require.Len(t, users, 1)
	require.Equal(t, 3, users[0].ID)

	require.NoError(t, db.Find("Name", "Updated", &users))
	require.Len(t, users, 1)
	require.Equal(t, 2, users[0].ID)

	err = db.Find("Name", "DeleteMe", &users)
	require.Equal(t, ErrNotFound, err)
}

func TestTransactionMixedSaveQueryDeleteIndexes(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Save(&IndexedNameUser{ID: 1, Name: "Original"}))

	tx, err := db.Begin(true)
	require.NoError(t, err)
	require.NoError(t, tx.Select(q.Eq("Name", "Original")).Delete(&IndexedNameUser{}))
	require.NoError(t, tx.Save(&IndexedNameUser{ID: 1, Name: "Restored"}))
	require.NoError(t, tx.Save(&IndexedNameUser{ID: 2, Name: "Transient"}))
	require.NoError(t, tx.Select(q.Eq("Name", "Transient")).Delete(&IndexedNameUser{}))
	require.NoError(t, tx.Commit())

	var users []IndexedNameUser
	require.NoError(t, db.Find("Name", "Restored", &users))
	require.Len(t, users, 1)
	require.Equal(t, 1, users[0].ID)

	err = db.Find("Name", "Original", &users)
	require.Equal(t, ErrNotFound, err)

	err = db.Find("Name", "Transient", &users)
	require.Equal(t, ErrNotFound, err)
}

func TestTransactionDeleteSaveOrderIndexesFinalState(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Save(&IndexedNameUser{ID: 1, Name: "Original"}))

	tx, err := db.Begin(true)
	require.NoError(t, err)
	require.NoError(t, tx.DeleteStruct(&IndexedNameUser{ID: 1}))
	require.NoError(t, tx.Save(&IndexedNameUser{ID: 1, Name: "Restored"}))
	require.NoError(t, tx.Commit())

	var users []IndexedNameUser
	require.NoError(t, db.Find("Name", "Restored", &users))
	require.Len(t, users, 1)
	require.Equal(t, 1, users[0].ID)

	err = db.Find("Name", "Original", &users)
	require.Equal(t, ErrNotFound, err)

	tx, err = db.Begin(true)
	require.NoError(t, err)
	require.NoError(t, tx.Save(&IndexedNameUser{ID: 2, Name: "Transient"}))
	require.NoError(t, tx.DeleteStruct(&IndexedNameUser{ID: 2}))
	require.NoError(t, tx.Commit())

	err = db.Find("Name", "Transient", &users)
	require.Equal(t, ErrNotFound, err)
}

func TestTransactionNotWritable(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	err := db.Save(&User{ID: 10, Name: "John"})
	require.NoError(t, err)

	tx, err := db.Begin(false)
	require.NoError(t, err)

	err = tx.Save(&User{ID: 20, Name: "John"})
	require.Error(t, err)

	var user User
	err = tx.One("ID", 10, &user)
	require.NoError(t, err)

	err = tx.Rollback()
	require.NoError(t, err)
}
