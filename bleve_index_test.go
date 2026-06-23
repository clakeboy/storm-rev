package storm

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

type blevePathUser struct {
	ID   int
	Name string `storm:"index"`
}

type bleveUseDBUser struct {
	ID   int
	Name string `storm:"index"`
}

type compositeUser struct {
	ID    int
	Group string `storm:"index,composite=group_age:1"`
	Age   int    `storm:"composite=group_age:2"`
}

type reindexBleveUser struct {
	ID    int
	Group string `storm:"index"`
}

type fullTextArticle struct {
	ID    int
	Title string `storm:"fulltext"`
	Body  string
}

func TestBleveIndexPath(t *testing.T) {
	dir, err := os.MkdirTemp(os.TempDir(), "storm-bleve")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	dbPath := filepath.Join(dir, "storm.db")
	db, err := Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	require.NoError(t, db.Save(&blevePathUser{ID: 1, Name: "John"}))
	require.Equal(t, filepath.Join(dir, "storm_db_index"), indexRootDir(dbPath))
	require.DirExists(t, filepath.Join(indexRootDir(dbPath), safeIndexName("blevePathUser")+bleveIndexSuffix))
}

func TestBleveIndexPathWithUseDB(t *testing.T) {
	dir, err := os.MkdirTemp(os.TempDir(), "storm-bleve-usedb")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	boltPath := filepath.Join(dir, "bolt.db")
	boltDB, err := bolt.Open(boltPath, 0o600, &bolt.Options{Timeout: time.Second})
	require.NoError(t, err)

	db, err := Open("ignored.db", UseDB(boltDB))
	require.NoError(t, err)
	defer db.Close()

	require.NoError(t, db.Save(&bleveUseDBUser{ID: 1, Name: "John"}))
	require.Equal(t, filepath.Join(dir, "bolt_db_index"), indexRootDir(boltPath))
	require.DirExists(t, filepath.Join(indexRootDir(boltPath), safeIndexName("bleveUseDBUser")+bleveIndexSuffix))
}

func TestFindByCompositeIndex(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Save(&compositeUser{ID: 1, Group: "staff", Age: 20}))
	require.NoError(t, db.Save(&compositeUser{ID: 2, Group: "staff", Age: 20}))
	require.NoError(t, db.Save(&compositeUser{ID: 3, Group: "staff", Age: 30}))
	require.NoError(t, db.Save(&compositeUser{ID: 4, Group: "admin", Age: 20}))

	var users []compositeUser
	require.NoError(t, db.FindByIndex("group_age", []any{"staff", 20}, &users))
	require.Len(t, users, 2)
	require.Equal(t, 1, users[0].ID)
	require.Equal(t, 2, users[1].ID)

	require.NoError(t, db.FindByIndex("group_age", []any{"staff", 20}, &users, Limit(1), Skip(1), Reverse()))
	require.Len(t, users, 1)
	require.Equal(t, 1, users[0].ID)

	require.Equal(t, ErrIdxNotFound, db.FindByIndex("missing", []any{"staff", 20}, &users))
	require.Equal(t, ErrIncompatibleValue, db.FindByIndex("group_age", []any{"staff"}, &users))
}

func TestFindByCompositeIndexAfterUpdateAndDelete(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Save(&compositeUser{ID: 1, Group: "staff", Age: 20}))
	require.NoError(t, db.Save(&compositeUser{ID: 2, Group: "staff", Age: 20}))
	require.NoError(t, db.UpdateField(&compositeUser{ID: 2}, "Age", 40))

	var users []compositeUser
	require.NoError(t, db.FindByIndex("group_age", []any{"staff", 20}, &users))
	require.Len(t, users, 1)
	require.Equal(t, 1, users[0].ID)

	require.NoError(t, db.FindByIndex("group_age", []any{"staff", 40}, &users))
	require.Len(t, users, 1)
	require.Equal(t, 2, users[0].ID)

	require.NoError(t, db.DeleteStruct(&compositeUser{ID: 2}))
	require.Equal(t, ErrNotFound, db.FindByIndex("group_age", []any{"staff", 40}, &users))
}

func TestBleveReIndexRebuildsDroppedIndex(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Save(&reindexBleveUser{ID: 1, Group: "staff"}))
	require.NoError(t, db.Save(&reindexBleveUser{ID: 2, Group: "staff"}))
	require.NoError(t, db.indexer.dropTable("reindexBleveUser"))

	var users []reindexBleveUser
	require.Equal(t, ErrNotFound, db.Find("Group", "staff", &users))

	require.NoError(t, db.ReIndex(&reindexBleveUser{}))
	require.NoError(t, db.Find("Group", "staff", &users))
	require.Len(t, users, 2)
}

func TestCompositeIndexTagErrors(t *testing.T) {
	type duplicateOrder struct {
		ID int
		A  string `storm:"composite=dup:1"`
		B  string `storm:"composite=dup:1"`
	}
	type gapOrder struct {
		ID int
		A  string `storm:"composite=gap:1"`
		B  string `storm:"composite=gap:3"`
	}

	db, cleanup := createDB(t)
	defer cleanup()

	require.Error(t, db.Init(&duplicateOrder{}))
	require.Error(t, db.Init(&gapOrder{}))
}

func TestFullTextIndexSearch(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Save(&fullTextArticle{ID: 1, Title: "Storm brings fast search", Body: "one"}))
	require.NoError(t, db.Save(&fullTextArticle{ID: 2, Title: "Bleve supports full text search", Body: "two"}))
	require.NoError(t, db.Save(&fullTextArticle{ID: 3, Title: "Exact indexes stay separate", Body: "three"}))

	var articles []fullTextArticle
	require.NoError(t, db.Search("Title", "search", &articles))
	require.Len(t, articles, 2)
	require.ElementsMatch(t, []int{1, 2}, []int{articles[0].ID, articles[1].ID})

	require.NoError(t, db.Search("Title", "bleve", &articles))
	require.Len(t, articles, 1)
	require.Equal(t, 2, articles[0].ID)

	require.NoError(t, db.Search("Title", "search", &articles, Limit(1)))
	require.Len(t, articles, 1)

	require.Equal(t, ErrIdxNotFound, db.Search("Body", "one", &articles))
}

func TestFullTextIndexAfterUpdateDeleteAndReIndex(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Save(&fullTextArticle{ID: 1, Title: "Bleve supports full text search"}))
	require.NoError(t, db.Save(&fullTextArticle{ID: 2, Title: "Storm keeps exact indexes"}))

	var articles []fullTextArticle
	require.NoError(t, db.Search("Title", "bleve", &articles))
	require.Len(t, articles, 1)
	require.Equal(t, 1, articles[0].ID)

	require.NoError(t, db.UpdateField(&fullTextArticle{ID: 1}, "Title", "No matching token"))
	require.Equal(t, ErrNotFound, db.Search("Title", "bleve", &articles))

	require.NoError(t, db.UpdateField(&fullTextArticle{ID: 2}, "Title", "Bleve rebuilt search"))
	require.NoError(t, db.Search("Title", "bleve", &articles))
	require.Len(t, articles, 1)
	require.Equal(t, 2, articles[0].ID)

	require.NoError(t, db.DeleteStruct(&fullTextArticle{ID: 2}))
	require.Equal(t, ErrNotFound, db.Search("Title", "bleve", &articles))

	require.NoError(t, db.Save(&fullTextArticle{ID: 3, Title: "Bleve can rebuild"}))
	require.NoError(t, db.indexer.dropTable("fullTextArticle"))
	require.Equal(t, ErrNotFound, db.Search("Title", "bleve", &articles))
	require.NoError(t, db.ReIndex(&fullTextArticle{}))
	require.NoError(t, db.Search("Title", "bleve", &articles))
	require.Len(t, articles, 1)
	require.Equal(t, 3, articles[0].ID)
}
