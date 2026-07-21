package storm

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/clakeboy/storm-rev/v2/codec/gob"
	"github.com/clakeboy/storm-rev/v2/codec/json"
	"github.com/clakeboy/storm-rev/v2/q"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

func TestInit(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	var u IndexedNameUser
	err := db.One("Name", "John", &u)
	require.Equal(t, ErrNotFound, err)

	err = db.Init(&u)
	require.NoError(t, err)

	err = db.One("Name", "John", &u)
	require.Error(t, err)
	require.Equal(t, ErrNotFound, err)

	err = db.Init(&ClassicBadTags{})
	require.Error(t, err)
	require.Equal(t, ErrUnknownTag, err)

	err = db.Init(10)
	require.Error(t, err)
	require.Equal(t, ErrBadType, err)

	err = db.Init(&ClassicNoTags{})
	require.Error(t, err)
	require.Equal(t, ErrNoID, err)

	err = db.Init(&struct{ ID string }{})
	require.Error(t, err)
	require.Equal(t, ErrNoName, err)
}

func TestInitMetadata(t *testing.T) {
	db, cleanup := createDB(t, Batch())
	defer cleanup()

	err := db.Init(new(User))
	require.NoError(t, err)
	n := db.WithCodec(gob.Codec)
	err = n.Init(new(User))
	require.Equal(t, ErrDifferentCodec, err)
}

func TestReIndex(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	for i := 1; i < 10; i++ {
		type User struct {
			ID   int
			Age  int    `storm:"index"`
			Name string `storm:"unique"`
		}

		u := User{
			ID:   i,
			Age:  i % 2,
			Name: fmt.Sprintf("John%d", i),
		}
		err := db.Save(&u)
		require.NoError(t, err)
	}

	db.Bolt.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("User"))
		require.NotNil(t, bucket)
		require.Nil(t, bucket.Bucket([]byte(indexPrefix+"Name")))
		require.Nil(t, bucket.Bucket([]byte(indexPrefix+"Age")))
		return nil
	})
	require.DirExists(t, filepath.Join(indexRootDir(db.Bolt.Path()), safeIndexName("User")+bleveIndexSuffix))

	type User struct {
		ID    int
		Age   int
		Name  string `storm:"index"`
		Group string `storm:"unique"`
	}

	require.NoError(t, db.ReIndex(new(User)))

	db.Bolt.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("User"))
		require.NotNil(t, bucket)
		require.Nil(t, bucket.Bucket([]byte(indexPrefix+"Age")))
		require.Nil(t, bucket.Bucket([]byte(indexPrefix+"Name")))
		require.Nil(t, bucket.Bucket([]byte(indexPrefix+"Group")))
		return nil
	})
	require.DirExists(t, filepath.Join(indexRootDir(db.Bolt.Path()), safeIndexName("User")+bleveIndexSuffix))
}

func TestReIndexUpdatesStoredMetadataFromStruct(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	{
		type User struct {
			ID   int
			Name string
		}

		require.NoError(t, db.Save(&User{ID: 1, Name: "John"}))
	}

	{
		type User struct {
			ID    int
			Name  string `storm:"index"`
			Group string `storm:"unique"`
		}

		require.NoError(t, db.ReIndex(&User{}))

		var users []User
		require.NoError(t, db.Find("Name", "John", &users))
		require.Len(t, users, 1)
	}

	columns, err := db.ListColumns("", "User")
	require.NoError(t, err)
	require.Equal(t, tagIdx, requireColumn(t, columns, "Name").Index)
	require.Equal(t, tagUniqueIdx, requireColumn(t, columns, "Group").Index)
}

func TestReIndexNilUsesStoredMetadata(t *testing.T) {
	db, cleanup := createDB(t, BleveSyncWrites())
	defer cleanup()

	type reindexMetadataUser struct {
		ID   int
		Name string `storm:"index"`
	}

	require.NoError(t, db.Save(&reindexMetadataUser{ID: 1, Name: "John"}))
	require.NoError(t, db.Save(&reindexMetadataUser{ID: 2, Name: "Jane"}))
	require.NoError(t, db.indexer.dropTable("reindexMetadataUser"))

	var users []reindexMetadataUser
	require.Equal(t, ErrNotFound, db.Find("Name", "John", &users))

	require.NoError(t, db.ReIndex(nil))
	require.NoError(t, db.Find("Name", "John", &users))
	require.Len(t, users, 1)
}

func TestReIndexNilUsesStoredMetadataBelowCurrentNode(t *testing.T) {
	db, cleanup := createDB(t, BleveSyncWrites())
	defer cleanup()

	type reindexMetadataIssue struct {
		ID    int
		Title string `storm:"index"`
	}

	repo := db.From("repo")
	require.NoError(t, repo.Save(&reindexMetadataIssue{ID: 1, Title: "Bug"}))
	require.NoError(t, db.indexer.dropTable("reindexMetadataIssue"))

	var issues []reindexMetadataIssue
	require.Equal(t, ErrNotFound, repo.Find("Title", "Bug", &issues))

	require.NoError(t, repo.ReIndex(nil))
	require.NoError(t, repo.Find("Title", "Bug", &issues))
	require.Len(t, issues, 1)
}

func TestSave(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	err := db.Save(&SimpleUser{ID: 10, Name: "John"})
	require.NoError(t, err)

	err = db.Save(&SimpleUser{Name: "John"})
	require.Error(t, err)
	require.Equal(t, ErrZeroID, err)

	err = db.Save(&ClassicBadTags{ID: "id", PublicField: 100})
	require.Error(t, err)
	require.Equal(t, ErrUnknownTag, err)

	err = db.Save(&UserWithNoID{Name: "John"})
	require.Error(t, err)
	require.Equal(t, ErrNoID, err)

	err = db.Save(&UserWithIDField{ID: 10, Name: "John"})
	require.NoError(t, err)

	u := UserWithEmbeddedIDField{}
	u.ID = 150
	u.Name = "Pete"
	u.Age = 10
	err = db.Save(&u)
	require.NoError(t, err)

	v := UserWithIDField{ID: 10, Name: "John"}
	err = db.Save(&v)
	require.NoError(t, err)

	w := UserWithEmbeddedField{}
	w.ID = 150
	w.Name = "John"
	err = db.Save(&w)
	require.NoError(t, err)

	db.Bolt.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("UserWithIDField"))
		require.NotNil(t, bucket)

		i, err := toBytes(10, json.Codec)
		require.NoError(t, err)

		val := bucket.Get(i)
		require.NotNil(t, val)

		content, err := db.Codec().Marshal(&v)
		require.NoError(t, err)
		require.Equal(t, content, val)
		return nil
	})
}

func TestSaveUnique(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	u1 := UniqueNameUser{ID: 10, Name: "John", Age: 10}
	err := db.Save(&u1)
	require.NoError(t, err)

	u2 := UniqueNameUser{ID: 11, Name: "John", Age: 100}
	err = db.Save(&u2)
	require.Error(t, err)
	require.True(t, ErrAlreadyExists == err)

	// same id
	u3 := UniqueNameUser{ID: 10, Name: "Jake", Age: 100}
	err = db.Save(&u3)
	require.NoError(t, err)

	var found UniqueNameUser
	require.NoError(t, db.One("Name", "Jake", &found))
	require.Equal(t, 10, found.ID)
	require.Equal(t, ErrNotFound, db.One("Name", "John", &found))
}

func TestSaveUniqueStruct(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	a := ClassicUnique{ID: "id1"}
	a.InlineStruct.A = 10.0
	a.InlineStruct.B = 12.0

	err := db.Save(&a)
	require.NoError(t, err)

	b := ClassicUnique{ID: "id2"}
	b.InlineStruct.A = 10.0
	b.InlineStruct.B = 12.0

	err = db.Save(&b)
	require.Equal(t, ErrAlreadyExists, err)

	err = db.One("InlineStruct", struct {
		A float32
		B float64
	}{A: 10.0, B: 12.0}, &b)
	require.NoError(t, err)
	require.Equal(t, a.ID, b.ID)
}

func TestSaveIndex(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	u1 := IndexedNameUser{ID: 10, Name: "John", age: 10}
	err := db.Save(&u1)
	require.NoError(t, err)

	u1 = IndexedNameUser{ID: 10, Name: "John", age: 10}
	err = db.Save(&u1)
	require.NoError(t, err)

	u2 := IndexedNameUser{ID: 11, Name: "John", age: 100}
	err = db.Save(&u2)
	require.NoError(t, err)

	name1 := "Jake"
	name2 := "Jane"
	name3 := "James"

	for i := 0; i < 100; i++ {
		u := IndexedNameUser{ID: i + 1}

		if i%2 == 0 {
			u.Name = name1
		} else {
			u.Name = name2
		}

		db.Save(&u)
	}

	var users []IndexedNameUser
	err = db.Find("Name", name1, &users)
	require.NoError(t, err)
	require.Len(t, users, 50)

	err = db.Find("Name", name2, &users)
	require.NoError(t, err)
	require.Len(t, users, 50)

	err = db.Find("Name", name3, &users)
	require.Error(t, err)
	require.Equal(t, ErrNotFound, err)

	err = db.Save(nil)
	require.Error(t, err)
	require.Equal(t, ErrStructPtrNeeded, err)
}

func TestSaveEmptyValues(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	u := User{
		ID: 10,
	}
	err := db.Save(&u)
	require.NoError(t, err)

	var v User
	err = db.One("ID", 10, &v)
	require.NoError(t, err)
	require.Equal(t, 10, v.ID)

	u.Name = "John"
	u.Slug = "john"
	err = db.Save(&u)
	require.NoError(t, err)

	err = db.One("Name", "John", &v)
	require.NoError(t, err)
	require.Equal(t, "John", v.Name)
	require.Equal(t, "john", v.Slug)
	err = db.One("Slug", "john", &v)
	require.NoError(t, err)
	require.Equal(t, "John", v.Name)
	require.Equal(t, "john", v.Slug)

	u.Name = ""
	u.Slug = ""
	err = db.Save(&u)
	require.NoError(t, err)

	err = db.One("Name", "John", &v)
	require.Error(t, err)
	err = db.One("Slug", "john", &v)
	require.Error(t, err)
}

func TestSaveIncrement(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	type User struct {
		Identifier int    `storm:"id,increment"`
		Name       string `storm:"index,increment"`
		Age        int    `storm:"unique,increment=18"`
	}

	for i := 1; i < 10; i++ {
		s1 := User{Name: fmt.Sprintf("John%d", i)}
		err := db.Save(&s1)
		require.NoError(t, err)
		require.Equal(t, i, s1.Identifier)
		require.Equal(t, i-1+18, s1.Age)
		require.Equal(t, fmt.Sprintf("John%d", i), s1.Name)

		var s2 User
		err = db.One("Identifier", i, &s2)
		require.NoError(t, err)
		require.Equal(t, s1, s2)

		var list []User
		err = db.Find("Age", i-1+18, &list)
		require.NoError(t, err)
		require.Len(t, list, 1)
		require.Equal(t, s1, list[0])
	}
}

func TestSaveAll(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	users := []SimpleUser{
		{ID: 1, Name: "John"},
		{ID: 2, Name: "Jane"},
	}
	require.NoError(t, db.SaveAll(users))

	moreUsers := []*SimpleUser{
		{ID: 3, Name: "Jake"},
		{ID: 4, Name: "Jill"},
	}
	require.NoError(t, db.SaveAll(moreUsers))
	require.NoError(t, db.SaveAll([]SimpleUser{}))

	var all []SimpleUser
	require.NoError(t, db.All(&all))
	require.Len(t, all, 4)

	var found SimpleUser
	require.NoError(t, db.One("ID", 3, &found))
	require.Equal(t, "Jake", found.Name)

	require.Equal(t, ErrSlicePtrNeeded, db.SaveAll(nil))
	require.Equal(t, ErrSlicePtrNeeded, db.SaveAll(10))

	var nilUsers []SimpleUser
	require.Equal(t, ErrSlicePtrNeeded, db.SaveAll(nilUsers))
	require.Equal(t, ErrStructPtrNeeded, db.SaveAll([]int{1}))
	require.Equal(t, ErrStructPtrNeeded, db.SaveAll([]*SimpleUser{nil}))
	require.Equal(t, ErrZeroID, db.SaveAll([]SimpleUser{{Name: "missing id"}}))
}

func TestSaveAllConcurrentWithoutBatch(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	type ConcurrentSaveAllUser struct {
		ID     int    `storm:"id"`
		Worker int    `storm:"index"`
		Name   string `storm:"index"`
	}

	const (
		workers   = 8
		perWorker = 25
	)

	var wg sync.WaitGroup
	errs := make(chan error, workers)

	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()

			// Give each goroutine its own slice and globally unique IDs so the
			// test only exercises concurrent SaveAll behavior.
			users := make([]ConcurrentSaveAllUser, perWorker)
			for i := 0; i < perWorker; i++ {
				id := worker*perWorker + i + 1
				workerID := worker + 1
				users[i] = ConcurrentSaveAllUser{
					ID:     id,
					Worker: workerID,
					Name:   fmt.Sprintf("worker-%d-user-%d", worker, i),
				}
			}

			errs <- db.SaveAll(users)
		}(worker)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	var all []ConcurrentSaveAllUser
	require.NoError(t, db.All(&all))
	require.Len(t, all, workers*perWorker)

	for worker := 0; worker < workers; worker++ {
		workerID := worker + 1
		var found []ConcurrentSaveAllUser
		require.NoError(t, db.Find("Worker", workerID, &found))
		require.Len(t, found, perWorker)

		seen := make(map[int]bool, perWorker)
		for _, user := range found {
			seen[user.ID] = true
		}
		for i := 0; i < perWorker; i++ {
			id := worker*perWorker + i + 1
			require.True(t, seen[id], "missing ID %d for worker %d", id, workerID)
		}
	}
}

func TestUpdateReleasesBoltWriterBeforeBleveBatch(t *testing.T) {
	db, cleanup := createDB(t, BleveBatchDelay(time.Millisecond))
	defer cleanup()

	type CoordinatedUpdateUser struct {
		ID   int    `storm:"id"`
		Name string `storm:"index"`
	}

	require.NoError(t, db.Save(&CoordinatedUpdateUser{ID: 1, Name: "before"}))

	batchStarted := make(chan struct{})
	releaseBatch := make(chan struct{})
	var observerOnce sync.Once
	db.indexer.batchObserver = func(_ string, _ int, _ uint64) {
		observerOnce.Do(func() {
			close(batchStarted)
			<-releaseBatch
		})
	}
	defer func() {
		select {
		case <-releaseBatch:
		default:
			close(releaseBatch)
		}
	}()

	updateDone := make(chan error, 1)
	go func() {
		updateDone <- db.UpdateField(&CoordinatedUpdateUser{ID: 1}, "Name", "after")
	}()

	select {
	case <-batchStarted:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "Bleve batch did not start")
	}

	// The external index is deliberately blocked above. A raw Bolt write must
	// still complete because Update released Bolt's writer lock before enqueueing.
	boltWriteDone := make(chan error, 1)
	go func() {
		boltWriteDone <- db.Set("probe", "key", "value")
	}()
	select {
	case err := <-boltWriteDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "Bolt writer remained blocked by Bleve")
	}

	close(releaseBatch)
	require.NoError(t, <-updateDone)
}

func TestConcurrentUpdatesUseBleveGroupCommit(t *testing.T) {
	db, cleanup := createDB(t, BleveBatchDelay(30*time.Millisecond))
	defer cleanup()

	type GroupCommitUser struct {
		ID   int    `storm:"id"`
		Name string `storm:"index"`
	}

	const updates = 32
	users := make([]GroupCommitUser, updates)
	for i := range users {
		users[i] = GroupCommitUser{ID: i + 1, Name: fmt.Sprintf("before-%d", i+1)}
	}
	require.NoError(t, db.SaveAll(users))
	require.NoError(t, db.FlushBleve(context.Background()))

	var sizesMu sync.Mutex
	var batchSizes []int
	db.indexer.batchObserver = func(_ string, docs int, _ uint64) {
		sizesMu.Lock()
		batchSizes = append(batchSizes, docs)
		sizesMu.Unlock()
	}

	start := make(chan struct{})
	errs := make(chan error, updates)
	var wg sync.WaitGroup
	for i := 0; i < updates; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			errs <- db.UpdateField(&GroupCommitUser{ID: id}, "Name", fmt.Sprintf("after-%d", id))
		}(i + 1)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.NoError(t, db.FlushBleve(context.Background()))

	sizesMu.Lock()
	observed := append([]int(nil), batchSizes...)
	sizesMu.Unlock()
	require.NotEmpty(t, observed)
	require.Less(t, len(observed), updates, "concurrent updates were not coalesced")
	total := 0
	for _, size := range observed {
		total += size
	}
	require.Equal(t, updates, total)

	for id := 1; id <= updates; id++ {
		var found []GroupCommitUser
		require.NoError(t, db.Find("Name", fmt.Sprintf("after-%d", id), &found))
		require.Len(t, found, 1)
		require.Equal(t, id, found[0].ID)
	}
}

func TestConcurrentUpdatesPreserveCommitOrderForSameDocument(t *testing.T) {
	db, cleanup := createDB(t, BleveBatchDelay(30*time.Millisecond))
	defer cleanup()

	type OrderedUpdateUser struct {
		ID   int    `storm:"id"`
		Name string `storm:"index"`
	}

	require.NoError(t, db.Save(&OrderedUpdateUser{ID: 1, Name: "before"}))
	require.NoError(t, db.FlushBleve(context.Background()))

	const updates = 32
	start := make(chan struct{})
	errs := make(chan error, updates)
	var wg sync.WaitGroup
	for i := 0; i < updates; i++ {
		wg.Add(1)
		go func(value int) {
			defer wg.Done()
			<-start
			errs <- db.UpdateField(
				&OrderedUpdateUser{ID: 1},
				"Name",
				fmt.Sprintf("after-%d", value),
			)
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.NoError(t, db.FlushBleve(context.Background()))

	// The final Bolt value must be the value indexed by Bleve. This catches an
	// enqueue-order reversal between two already-committed updates of one ID.
	var current OrderedUpdateUser
	require.NoError(t, db.One("ID", 1, &current))
	require.NotEqual(t, "before", current.Name)

	var indexed []OrderedUpdateUser
	require.NoError(t, db.Find("Name", current.Name, &indexed))
	require.Len(t, indexed, 1)
	require.Equal(t, current, indexed[0])
}

func TestBleveGroupCommitRespectsDocumentLimit(t *testing.T) {
	db, cleanup := createDB(t, BleveBatchDelay(time.Millisecond), BleveBatchMaxDocs(3))
	defer cleanup()

	type BoundedBatchUser struct {
		ID   int    `storm:"id"`
		Name string `storm:"index"`
	}

	var sizesMu sync.Mutex
	var batchSizes []int
	db.indexer.batchObserver = func(_ string, docs int, _ uint64) {
		sizesMu.Lock()
		batchSizes = append(batchSizes, docs)
		sizesMu.Unlock()
	}

	users := make([]BoundedBatchUser, 8)
	for i := range users {
		users[i] = BoundedBatchUser{ID: i + 1, Name: fmt.Sprintf("user-%d", i+1)}
	}
	require.NoError(t, db.SaveAll(users))
	require.NoError(t, db.FlushBleve(context.Background()))

	sizesMu.Lock()
	observed := append([]int(nil), batchSizes...)
	sizesMu.Unlock()
	require.Equal(t, []int{3, 3, 2}, observed)
}

func TestSaveAllIncrement(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	type User struct {
		ID   int    `storm:"id,increment"`
		Name string `storm:"index"`
		Age  int    `storm:"index,increment"`
	}

	users := []User{
		{Name: "John"},
		{Name: "Jane"},
	}
	require.NoError(t, db.SaveAll(users))
	require.Equal(t, 1, users[0].ID)
	require.Equal(t, 2, users[1].ID)
	require.Equal(t, 1, users[0].Age)
	require.Equal(t, 2, users[1].Age)

	var byAge []User
	require.NoError(t, db.Find("Age", 2, &byAge))
	require.Len(t, byAge, 1)
	require.Equal(t, users[1], byAge[0])
}

func TestSaveAllUnique(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Save(&UniqueNameUser{ID: 1, Name: "taken"}))
	require.Equal(t, ErrAlreadyExists, db.SaveAll([]UniqueNameUser{{ID: 2, Name: "taken"}}))

	var missing UniqueNameUser
	require.Equal(t, ErrNotFound, db.One("ID", 2, &missing))

	err := db.SaveAll([]UniqueNameUser{
		{ID: 2, Name: "same"},
		{ID: 3, Name: "same"},
	})
	require.Equal(t, ErrAlreadyExists, err)
	require.Equal(t, ErrNotFound, db.One("ID", 2, &missing))

	err = db.SaveAll([]UniqueNameUser{
		{ID: 1, Name: "free"},
		{ID: 2, Name: "taken"},
	})
	require.NoError(t, err)

	var found UniqueNameUser
	require.NoError(t, db.One("Name", "free", &found))
	require.Equal(t, 1, found.ID)
	require.NoError(t, db.One("Name", "taken", &found))
	require.Equal(t, 2, found.ID)
}

func TestSaveAllIndexes(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	type Article struct {
		ID    int    `storm:"id"`
		Group string `storm:"index,composite=group_age:1"`
		Age   int    `storm:"index,composite=group_age:2"`
		Slug  string `storm:"unique"`
		Body  string `storm:"fulltext"`
	}

	articles := []Article{
		{ID: 1, Group: "staff", Age: 20, Slug: "one", Body: "storm supports batch save"},
		{ID: 2, Group: "staff", Age: 30, Slug: "two", Body: "bleve indexes batch records"},
		{ID: 3, Group: "admin", Age: 20, Slug: "three", Body: "exact indexes stay available"},
	}
	require.NoError(t, db.SaveAll(articles))

	var byGroup []Article
	require.NoError(t, db.Find("Group", "staff", &byGroup))
	require.Len(t, byGroup, 2)

	var byComposite []Article
	require.NoError(t, db.FindByIndex("group_age", []any{"staff", 20}, &byComposite))
	require.Len(t, byComposite, 1)
	require.Equal(t, 1, byComposite[0].ID)

	var bySlug Article
	require.NoError(t, db.One("Slug", "two", &bySlug))
	require.Equal(t, 2, bySlug.ID)

	var byText []Article
	require.NoError(t, db.Search("Body", "batch", &byText))
	require.Len(t, byText, 2)
}

func TestSaveDifferentBucketRoot(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.Len(t, db.Node.(*node).rootBucket, 0)

	dbSub := db.From("sub").(*node)

	require.NotEqual(t, dbSub, db)
	require.Len(t, dbSub.rootBucket, 1)

	err := db.Save(&User{ID: 10, Name: "John"})
	require.NoError(t, err)
	err = dbSub.Save(&User{ID: 11, Name: "Paul"})
	require.NoError(t, err)

	var (
		john User
		paul User
	)

	err = db.One("Name", "John", &john)
	require.NoError(t, err)
	err = db.One("Name", "Paul", &paul)
	require.Error(t, err)

	err = dbSub.One("Name", "Paul", &paul)
	require.NoError(t, err)
	err = dbSub.One("Name", "John", &john)
	require.Error(t, err)
}

func TestSaveEmbedded(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	type Base struct {
		ID int `storm:"id,increment"`
	}

	type User struct {
		Base      `storm:"inline"`
		Group     string `storm:"index"`
		Email     string `storm:"unique"`
		Name      string
		Age       int
		CreatedAt time.Time `storm:"index"`
	}

	user := User{
		Group:     "staff",
		Email:     "john@provider.com",
		Name:      "John",
		Age:       21,
		CreatedAt: time.Now(),
	}

	err := db.Save(&user)
	require.NoError(t, err)
	require.Equal(t, 1, user.ID)
}

func TestSaveByValue(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	w := User{Name: "John"}
	err := db.Save(w)
	require.Error(t, err)
	require.Equal(t, ErrStructPtrNeeded, err)
}

func TestSaveWithBatch(t *testing.T) {
	db, cleanup := createDB(t, Batch())
	defer cleanup()

	var wg sync.WaitGroup

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := db.Save(&User{ID: i + 1, Name: "John"})
			require.NoError(t, err)
		}(i)
	}

	wg.Wait()
}

func TestSaveMetadata(t *testing.T) {
	db, cleanup := createDB(t, Batch())
	defer cleanup()

	w := User{ID: 10, Name: "John"}
	err := db.Save(&w)
	require.NoError(t, err)
	n := db.WithCodec(gob.Codec)
	err = n.Save(&w)
	require.Equal(t, ErrDifferentCodec, err)
}

func TestUpdate(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	type User struct {
		ID          int       `storm:"id,increment"`
		Name        string    `storm:"index"`
		Age         uint64    `storm:"index,increment"`
		DateOfBirth time.Time `storm:"index"`
		Group       string
		Slug        string `storm:"unique"`
	}

	var u User

	err := db.Save(&User{ID: 10, Name: "John", Age: 5, Group: "Staff", Slug: "john"})
	require.NoError(t, err)

	// nil
	err = db.Update(nil)
	require.Equal(t, ErrStructPtrNeeded, err)

	// no id
	err = db.Update(&User{Name: "Jack"})
	require.Equal(t, ErrNoID, err)

	// Unknown user
	err = db.Update(&User{ID: 11, Name: "Jack"})
	require.Equal(t, ErrNotFound, err)

	// actual user
	err = db.Update(&User{ID: 10, Name: "Jack"})
	require.NoError(t, err)

	err = db.One("Name", "John", &u)
	require.Equal(t, ErrNotFound, err)

	err = db.One("Name", "Jack", &u)
	require.NoError(t, err)
	require.Equal(t, "Jack", u.Name)
	require.Equal(t, uint64(5), u.Age)

	// indexed field with zero value #170
	err = db.Update(&User{ID: 10, Group: "Staff"})
	require.NoError(t, err)

	err = db.One("Name", "Jack", &u)
	require.NoError(t, err)
	require.Equal(t, "Jack", u.Name)
	require.Equal(t, uint64(5), u.Age)
	require.Equal(t, "Staff", u.Group)
}

func TestUpdateField(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	type User struct {
		ID          int       `storm:"id,increment"`
		Name        string    `storm:"index"`
		Age         uint64    `storm:"index,increment"`
		DateOfBirth time.Time `storm:"index"`
		Group       string
		Slug        string `storm:"unique"`
	}

	var u User

	err := db.Save(&User{ID: 10, Name: "John", Age: 5, Group: "Staff", Slug: "john"})
	require.NoError(t, err)

	// nil
	err = db.UpdateField(nil, "", nil)
	require.Equal(t, ErrStructPtrNeeded, err)

	// no id
	err = db.UpdateField(&User{}, "Name", "Jack")
	require.Equal(t, ErrNoID, err)

	// Unknown user
	err = db.UpdateField(&User{ID: 11}, "Name", "Jack")
	require.Equal(t, ErrNotFound, err)

	// Unknown field
	err = db.UpdateField(&User{ID: 11}, "Address", "Jack")
	require.Equal(t, ErrNotFound, err)

	// Incompatible value
	err = db.UpdateField(&User{ID: 10}, "Name", 50)
	require.Equal(t, ErrIncompatibleValue, err)

	// actual user
	err = db.UpdateField(&User{ID: 10}, "Name", "Jack")
	require.NoError(t, err)

	err = db.One("Name", "John", &u)
	require.Equal(t, ErrNotFound, err)

	err = db.One("Name", "Jack", &u)
	require.NoError(t, err)
	require.Equal(t, "Jack", u.Name)

	// zero value
	err = db.UpdateField(&User{ID: 10}, "Name", "")
	require.NoError(t, err)

	err = db.One("Name", "Jack", &u)
	require.Equal(t, ErrNotFound, err)

	err = db.One("ID", 10, &u)
	require.NoError(t, err)
	require.Equal(t, "", u.Name)

	// zero value with int and increment
	err = db.UpdateField(&User{ID: 10}, "Age", uint64(0))
	require.NoError(t, err)

	err = db.Select(q.Eq("Age", uint64(5))).First(&u)
	require.Equal(t, ErrNotFound, err)

	err = db.Select(q.Eq("Age", uint64(0))).First(&u)
	require.NoError(t, err)
}

func TestDropByString(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	n := db.From("b1", "b2", "b3")
	err := n.Save(&SimpleUser{ID: 10, Name: "John"})
	require.NoError(t, err)

	err = db.From("b1").Drop("b2")
	require.NoError(t, err)

	err = db.From("b1").Drop("b2")
	require.Error(t, err)

	n.From("b4").Drop("b5")
	require.Error(t, err)

	err = db.Drop("b1")
	require.NoError(t, err)

	db.Bolt.Update(func(tx *bolt.Tx) error {
		require.Nil(t, db.From().GetBucket(tx, "b1"))
		d := db.WithTransaction(tx)
		n := d.From("a1")
		err = n.Save(&SimpleUser{ID: 10, Name: "John"})
		require.NoError(t, err)

		err = d.Drop("a1")
		require.NoError(t, err)

		return nil
	})
}

func TestDropByStruct(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	n := db.From("b1", "b2", "b3")
	err := n.Save(&SimpleUser{ID: 10, Name: "John"})
	require.NoError(t, err)

	err = n.Drop(&SimpleUser{})
	require.NoError(t, err)

	db.Bolt.Update(func(tx *bolt.Tx) error {
		require.Nil(t, n.GetBucket(tx, "SimpleUser"))
		d := db.WithTransaction(tx)
		n := d.From("a1")
		err = n.Save(&SimpleUser{ID: 10, Name: "John"})
		require.NoError(t, err)

		err = n.Drop(&SimpleUser{})
		require.NoError(t, err)

		require.Nil(t, n.GetBucket(tx, "SimpleUser"))
		return nil
	})
}

func TestDeleteStruct(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	u1 := IndexedNameUser{ID: 10, Name: "John", age: 10}
	err := db.Save(&u1)
	require.NoError(t, err)

	err = db.DeleteStruct(u1)
	require.Equal(t, ErrStructPtrNeeded, err)

	err = db.DeleteStruct(&u1)
	require.NoError(t, err)

	err = db.DeleteStruct(&u1)
	require.Equal(t, ErrNotFound, err)

	u2 := IndexedNameUser{}
	err = db.Get("IndexedNameUser", 10, &u2)
	require.True(t, ErrNotFound == err)

	err = db.DeleteStruct(nil)
	require.Equal(t, ErrStructPtrNeeded, err)

	var users []User
	for i := 0; i < 10; i++ {
		user := User{Name: "John", ID: i + 1, Slug: fmt.Sprintf("John%d", i+1), DateOfBirth: time.Now().Add(-time.Duration(i*10) * time.Minute)}
		err = db.Save(&user)
		require.NoError(t, err)
		users = append(users, user)
	}

	err = db.DeleteStruct(&users[0])
	require.NoError(t, err)
	err = db.DeleteStruct(&users[1])
	require.NoError(t, err)

	users = nil
	err = db.All(&users)
	require.NoError(t, err)
	require.Len(t, users, 8)
	require.Equal(t, 3, users[0].ID)
}
