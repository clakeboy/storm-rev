package storm

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

type durableOutboxUser struct {
	ID   int    `storm:"id"`
	Name string `storm:"index"`
	Note string
}

func TestUpdateNonIndexedFieldSkipsBleveOutbox(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Save(&durableOutboxUser{ID: 1, Name: "indexed", Note: "before"}))

	var mu sync.Mutex
	batches := 0
	db.indexer.batchObserver = func(_ string, _ int, _ uint64) {
		mu.Lock()
		batches++
		mu.Unlock()
	}

	require.NoError(t, db.UpdateField(&durableOutboxUser{ID: 1}, "Note", "after"))
	mu.Lock()
	require.Zero(t, batches)
	mu.Unlock()
	empty, err := durableOutboxEmpty(db.Bolt)
	require.NoError(t, err)
	require.True(t, empty)

	var found []durableOutboxUser
	require.NoError(t, db.Find("Name", "indexed", &found))
	require.Len(t, found, 1)
	require.Equal(t, "after", found[0].Note)
}

func TestBleveWritesAreAsyncByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "async.db")
	db, err := Open(path, BleveBatchDelay(time.Millisecond))
	require.NoError(t, err)
	defer db.Close()

	require.NoError(t, db.Save(&durableOutboxUser{ID: 1, Name: "before"}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, db.FlushBleve(ctx))

	batchStarted := make(chan struct{})
	releaseBatch := make(chan struct{})
	defer func() {
		select {
		case <-releaseBatch:
		default:
			close(releaseBatch)
		}
	}()
	var once sync.Once
	db.indexer.batchObserver = func(_ string, _ int, _ uint64) {
		once.Do(func() {
			close(batchStarted)
			<-releaseBatch
		})
	}

	updateDone := make(chan error, 1)
	go func() {
		updateDone <- db.UpdateField(&durableOutboxUser{ID: 1}, "Name", "after")
	}()
	select {
	case err := <-updateDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "async update waited for Bleve persistence")
	}
	select {
	case <-batchStarted:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "Bleve batch did not start")
	}

	// Pending tables fall back to Bolt, so the committed value is immediately visible.
	var found []durableOutboxUser
	require.NoError(t, db.Find("Name", "after", &found))
	require.Len(t, found, 1)

	close(releaseBatch)
	require.NoError(t, db.FlushBleve(ctx))
}

func TestDurableBleveOutboxRecoversAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recover.db")
	db, err := Open(path, BleveBatchDelay(time.Millisecond))
	require.NoError(t, err)
	require.NoError(t, db.Save(&durableOutboxUser{ID: 1, Name: "before"}))

	updated := &durableOutboxUser{ID: 1, Name: "after"}
	cfg, err := saveConfig(updated)
	require.NoError(t, err)
	n := db.Node.(*node)
	db.indexCommitMu.Lock()
	err = db.Bolt.Update(func(tx *bolt.Tx) error {
		record, err := n.save(tx, cfg, updated, false, nil)
		if err != nil {
			return err
		}
		_, err = persistDurableIndexJob(tx, n.rootBucket, []*savedRecord{record}, nil, nil)
		return err
	})
	db.indexCommitMu.Unlock()
	require.NoError(t, err)
	empty, err := durableOutboxEmpty(db.Bolt)
	require.NoError(t, err)
	require.False(t, empty)
	require.NoError(t, db.Close())

	reopened, err := Open(path, BleveBatchDelay(time.Millisecond))
	require.NoError(t, err)
	defer reopened.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, reopened.FlushBleve(ctx))

	var found []durableOutboxUser
	require.NoError(t, reopened.Find("Name", "after", &found))
	require.Len(t, found, 1)
}

func TestRecoveredOutboxPrecedesNewWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recover-order.db")
	db, err := Open(path, BleveAsyncWrites(), BleveBatchDelay(time.Millisecond))
	require.NoError(t, err)
	require.NoError(t, db.Save(&durableOutboxUser{ID: 1, Name: "before"}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, db.FlushBleve(ctx))

	n := db.Node.(*node)
	for i := 0; i < 32; i++ {
		updated := &durableOutboxUser{ID: 1, Name: fmt.Sprintf("recovered-%d", i)}
		cfg, err := saveConfig(updated)
		require.NoError(t, err)
		db.indexCommitMu.Lock()
		err = db.Bolt.Update(func(tx *bolt.Tx) error {
			record, err := n.save(tx, cfg, updated, false, nil)
			if err != nil {
				return err
			}
			_, err = persistDurableIndexJob(tx, n.rootBucket, []*savedRecord{record}, nil, nil)
			return err
		})
		db.indexCommitMu.Unlock()
		require.NoError(t, err)
	}
	require.NoError(t, db.Close())

	reopened, err := Open(
		path,
		BleveAsyncWrites(),
		BleveBatchDelay(time.Millisecond),
		BleveBatchMaxDocs(1),
		BleveBatchQueueSize(1),
	)
	require.NoError(t, err)
	defer reopened.Close()
	require.NoError(t, reopened.UpdateField(&durableOutboxUser{ID: 1}, "Name", "latest"))
	require.NoError(t, reopened.FlushBleve(ctx))

	var found []durableOutboxUser
	require.NoError(t, reopened.Find("Name", "latest", &found))
	require.Len(t, found, 1)
}

func TestDurableBleveOutboxRetriesTransientFailure(t *testing.T) {
	db, cleanup := createDB(t, BleveSyncWrites(), BleveBatchDelay(time.Millisecond))
	defer cleanup()
	require.NoError(t, db.Save(&durableOutboxUser{ID: 1, Name: "before"}))

	transient := errors.New("transient Bleve failure")
	var once sync.Once
	db.indexer.batchError = func(_ string, _ int, _ uint64) error {
		var err error
		once.Do(func() { err = transient })
		return err
	}
	err := db.UpdateField(&durableOutboxUser{ID: 1}, "Name", "after")
	require.ErrorIs(t, err, transient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, db.FlushBleve(ctx))
	var found []durableOutboxUser
	require.NoError(t, db.Find("Name", "after", &found))
	require.Len(t, found, 1)
}

func TestFailedAsyncBleveOutboxRecoversAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failed-recovery.db")
	db, err := Open(path, BleveAsyncWrites(), BleveBatchDelay(time.Millisecond))
	require.NoError(t, err)
	require.NoError(t, db.Save(&durableOutboxUser{ID: 1, Name: "before"}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, db.FlushBleve(ctx))

	failed := make(chan struct{})
	var once sync.Once
	db.indexer.batchError = func(_ string, _ int, _ uint64) error {
		once.Do(func() { close(failed) })
		return errors.New("persistent Bleve failure")
	}
	require.NoError(t, db.UpdateField(&durableOutboxUser{ID: 1}, "Name", "after"))
	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "Bleve failure was not exercised")
	}
	require.NoError(t, db.Close())

	reopened, err := Open(path, BleveBatchDelay(time.Millisecond))
	require.NoError(t, err)
	defer reopened.Close()
	require.NoError(t, reopened.FlushBleve(ctx))
	var found []durableOutboxUser
	require.NoError(t, reopened.Find("Name", "after", &found))
	require.Len(t, found, 1)
}

func TestDurableBleveOutboxFallsBackToAutomaticReIndex(t *testing.T) {
	db, cleanup := createDB(t, BleveSyncWrites(), BleveBatchDelay(time.Millisecond))
	defer cleanup()
	require.NoError(t, db.Save(&durableOutboxUser{ID: 1, Name: "before"}))

	db.indexer.batchError = func(_ string, _ int, _ uint64) error {
		return errors.New("persistent Bleve batch failure")
	}
	err := db.UpdateField(&durableOutboxUser{ID: 1}, "Name", "after")
	require.Error(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, db.FlushBleve(ctx))
	var found []durableOutboxUser
	require.NoError(t, db.Find("Name", "after", &found))
	require.Len(t, found, 1)
	require.False(t, db.indexer.isDirty("durableOutboxUser"))
}

func TestAutomaticReIndexPreservesFromRoot(t *testing.T) {
	db, cleanup := createDB(t, BleveAsyncWrites(), BleveBatchDelay(time.Millisecond))
	defer cleanup()
	repo := db.From("tenant-a")
	require.NoError(t, repo.Save(&durableOutboxUser{ID: 1, Name: "before"}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, db.FlushBleve(ctx))

	db.indexer.batchError = func(_ string, _ int, _ uint64) error {
		return errors.New("persistent nested Bleve failure")
	}
	require.NoError(t, repo.UpdateField(&durableOutboxUser{ID: 1}, "Name", "after"))
	require.NoError(t, db.FlushBleve(ctx))

	var found []durableOutboxUser
	require.NoError(t, repo.Find("Name", "after", &found))
	require.Len(t, found, 1)
}

func TestReIndexDoesNotHoldBoltWriter(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()
	require.NoError(t, db.SaveAll([]durableOutboxUser{
		{ID: 1, Name: "one"},
		{ID: 2, Name: "two"},
	}))

	rebuildStarted := make(chan struct{})
	releaseRebuild := make(chan struct{})
	defer func() {
		select {
		case <-releaseRebuild:
		default:
			close(releaseRebuild)
		}
	}()
	var once sync.Once
	db.indexer.rebuildObserver = func(_ string) {
		once.Do(func() {
			close(rebuildStarted)
			<-releaseRebuild
		})
	}
	reindexDone := make(chan error, 1)
	go func() { reindexDone <- db.ReIndex(&durableOutboxUser{}) }()
	select {
	case <-rebuildStarted:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "ReIndex did not enter snapshot build")
	}
	require.NoError(t, db.Bolt.View(func(tx *bolt.Tx) error {
		state, err := durableIndexTableState(tx, "durableOutboxUser")
		require.NoError(t, err)
		require.True(t, state.Dirty)
		return nil
	}))

	writeDone := make(chan error, 1)
	go func() { writeDone <- db.Set("probe", "key", "value") }()
	select {
	case err := <-writeDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "ReIndex held the Bolt writer")
	}

	close(releaseRebuild)
	require.NoError(t, <-reindexDone)
	require.NoError(t, db.Bolt.View(func(tx *bolt.Tx) error {
		state, err := durableIndexTableState(tx, "durableOutboxUser")
		require.NoError(t, err)
		require.False(t, state.Dirty)
		return nil
	}))
}

func TestReIndexReplaysConcurrentIndexedUpdate(t *testing.T) {
	db, cleanup := createDB(t, BleveAsyncWrites(), BleveBatchDelay(time.Millisecond))
	defer cleanup()
	require.NoError(t, db.Save(&durableOutboxUser{ID: 1, Name: "before"}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, db.FlushBleve(ctx))

	rebuildStarted := make(chan struct{})
	releaseRebuild := make(chan struct{})
	defer func() {
		select {
		case <-releaseRebuild:
		default:
			close(releaseRebuild)
		}
	}()
	var once sync.Once
	db.indexer.rebuildObserver = func(_ string) {
		once.Do(func() {
			close(rebuildStarted)
			<-releaseRebuild
		})
	}

	reindexDone := make(chan error, 1)
	go func() { reindexDone <- db.ReIndex(&durableOutboxUser{}) }()
	select {
	case <-rebuildStarted:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "ReIndex did not enter snapshot build")
	}

	require.NoError(t, db.UpdateField(&durableOutboxUser{ID: 1}, "Name", "after"))
	close(releaseRebuild)
	require.NoError(t, <-reindexDone)
	require.NoError(t, db.FlushBleve(ctx))

	var found []durableOutboxUser
	require.NoError(t, db.Find("Name", "after", &found))
	require.Len(t, found, 1)
}

func TestReIndexWaitsForEarlierQueuedMutation(t *testing.T) {
	db, cleanup := createDB(t, BleveAsyncWrites(), BleveBatchDelay(time.Millisecond))
	defer cleanup()
	require.NoError(t, db.Save(&durableOutboxUser{ID: 1, Name: "before"}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, db.FlushBleve(ctx))

	requestReady := make(chan struct{})
	releaseRequest := make(chan struct{})
	defer func() {
		select {
		case <-releaseRequest:
		default:
			close(releaseRequest)
		}
	}()
	var requestOnce sync.Once
	db.indexer.requestObserver = func(requests []*bleveIndexRequest) {
		if len(requests) == 0 || len(requests[0].jobs) == 0 {
			return
		}
		requestOnce.Do(func() {
			close(requestReady)
			<-releaseRequest
		})
	}

	rebuildStarted := make(chan struct{})
	var rebuildOnce sync.Once
	db.indexer.rebuildObserver = func(_ string) {
		rebuildOnce.Do(func() { close(rebuildStarted) })
	}

	require.NoError(t, db.UpdateField(&durableOutboxUser{ID: 1}, "Name", "after"))
	select {
	case <-requestReady:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "queued mutation did not reach the coordinator")
	}

	reindexDone := make(chan error, 1)
	go func() { reindexDone <- db.ReIndex(&durableOutboxUser{}) }()
	select {
	case <-rebuildStarted:
		require.FailNow(t, "ReIndex overtook an earlier queued mutation")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseRequest)
	require.NoError(t, <-reindexDone)
	require.NoError(t, db.FlushBleve(ctx))

	var found []durableOutboxUser
	require.NoError(t, db.Find("Name", "after", &found))
	require.Len(t, found, 1)
}

func TestReIndexKeepsPostBarrierMutationsDurable(t *testing.T) {
	db, cleanup := createDB(t, BleveAsyncWrites(), BleveBatchDelay(time.Millisecond))
	defer cleanup()
	require.NoError(t, db.Save(&durableOutboxUser{ID: 1, Name: "before"}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, db.FlushBleve(ctx))

	barrierPaused := make(chan struct{})
	releaseBarrier := make(chan struct{})
	deltasPaused := make(chan struct{})
	releaseDeltas := make(chan struct{})
	defer func() {
		for _, ch := range []chan struct{}{releaseBarrier, releaseDeltas} {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	}()

	var barrierOnce sync.Once
	var deltasOnce sync.Once
	db.indexer.requestObserver = func(requests []*bleveIndexRequest) {
		if len(requests) == 1 && requests[0].rebuild != nil {
			barrierOnce.Do(func() {
				close(barrierPaused)
				<-releaseBarrier
			})
			return
		}
		if len(requests) > 0 && len(requests[0].jobs) > 0 {
			deltasOnce.Do(func() {
				close(deltasPaused)
				<-releaseDeltas
			})
		}
	}

	reindexDone := make(chan error, 1)
	go func() { reindexDone <- db.ReIndex(&durableOutboxUser{}) }()
	select {
	case <-barrierPaused:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "ReIndex barrier did not reach the coordinator")
	}

	require.NoError(t, db.UpdateField(&durableOutboxUser{ID: 1}, "Name", "middle"))
	require.NoError(t, db.UpdateField(&durableOutboxUser{ID: 1}, "Name", "latest"))
	close(releaseBarrier)
	require.NoError(t, <-reindexDone)
	select {
	case <-deltasPaused:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "post-barrier mutations did not remain queued")
	}

	empty, err := durableOutboxEmpty(db.Bolt)
	require.NoError(t, err)
	require.False(t, empty, "ReIndex acknowledged mutations queued after its barrier")

	close(releaseDeltas)
	require.NoError(t, db.FlushBleve(ctx))
	var found []durableOutboxUser
	require.NoError(t, db.Find("Name", "latest", &found))
	require.Len(t, found, 1)
}

func TestExplicitTransactionPersistsAndDrainsBleveOutbox(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	tx, err := db.Begin(true)
	require.NoError(t, err)
	require.NoError(t, tx.Save(&durableOutboxUser{ID: 1, Name: "transactional"}))
	require.NoError(t, tx.Commit())

	empty, err := durableOutboxEmpty(db.Bolt)
	require.NoError(t, err)
	require.True(t, empty)
	var found []durableOutboxUser
	require.NoError(t, db.Find("Name", "transactional", &found))
	require.Len(t, found, 1)
}
