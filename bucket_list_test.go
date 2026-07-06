package storm

import (
	"testing"

	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

type listBuild struct {
	ID   int `storm:"id"`
	Name string
}

type listIssue struct {
	ID    int `storm:"id"`
	Title string
}

type listRootRecord struct {
	ID   int `storm:"id"`
	Name string
}

type listColumnRecord struct {
	ID          int    `storm:"id,increment"`
	DisplayName string `json:"display_name" storm:"index"`
	Slug        string `storm:"unique"`
	Count       int    `json:"count"`
}

func TestListFroms(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Save(&listRootRecord{ID: 1, Name: "root table"}))
	require.NoError(t, db.Set("sessions", "session-id", "value"))
	require.NoError(t, db.From("empty").Set("cache", "key", "value"))
	require.NoError(t, db.From("repo").Save(&listIssue{ID: 1, Title: "repo issue"}))
	require.NoError(t, db.From("alpha").Save(&listIssue{ID: 1, Title: "alpha issue"}))

	froms, err := db.ListFroms()
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "repo"}, froms)
}

func TestListTables(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	repo := db.From("repo")
	require.NoError(t, repo.Save(&listIssue{ID: 1, Title: "issue"}))
	require.NoError(t, repo.Save(&listBuild{ID: 1, Name: "build"}))
	require.NoError(t, repo.Set("cache", "key", "value"))
	require.NoError(t, db.From("onlyKV").Set("cache", "key", "value"))

	tables, err := db.ListTables("repo")
	require.NoError(t, err)
	require.Equal(t, []string{"listBuild", "listIssue"}, tables)

	tables, err = db.ListTables("onlyKV")
	require.NoError(t, err)
	require.Empty(t, tables)

	tables, err = db.ListTables("missing")
	require.ErrorIs(t, err, ErrNotFound)
	require.Nil(t, tables)

	tables, err = db.ListTables(dbinfo)
	require.ErrorIs(t, err, ErrNotFound)
	require.Nil(t, tables)
}

func TestListColumns(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.From("repo").Save(&listColumnRecord{
		DisplayName: "Build",
		Slug:        "build",
		Count:       3,
	}))
	require.NoError(t, db.From("repo").Set("cache", "key", "value"))
	require.NoError(t, db.Save(&listRootRecord{ID: 1, Name: "root"}))

	columns, err := db.ListColumns("repo", "listColumnRecord")
	require.NoError(t, err)
	require.Equal(t, []string{"Count", "DisplayName", "ID", "Slug"}, columnNames(columns))

	count := requireColumn(t, columns, "Count")
	require.Equal(t, "count", count.JSON)
	require.Equal(t, "int", count.Type)
	require.True(t, count.Integer)

	displayName := requireColumn(t, columns, "DisplayName")
	require.Equal(t, "display_name", displayName.JSON)
	require.Equal(t, "string", displayName.Type)
	require.Equal(t, tagIdx, displayName.Index)

	id := requireColumn(t, columns, "ID")
	require.Equal(t, "ID", id.JSON)
	require.Equal(t, "int", id.Type)
	require.Equal(t, tagUniqueIdx, id.Index)
	require.True(t, id.ID)
	require.True(t, id.Increment)
	require.Equal(t, int64(1), id.IncrementStart)
	require.True(t, id.Integer)

	slug := requireColumn(t, columns, "Slug")
	require.Equal(t, tagUniqueIdx, slug.Index)

	rootColumns, err := db.ListColumns("", "listRootRecord")
	require.NoError(t, err)
	require.Equal(t, []string{"ID", "Name"}, columnNames(rootColumns))

	columns, err = db.ListColumns("repo", "cache")
	require.ErrorIs(t, err, ErrNotFound)
	require.Nil(t, columns)

	columns, err = db.ListColumns("repo", "missing")
	require.ErrorIs(t, err, ErrNotFound)
	require.Nil(t, columns)

	columns, err = db.ListColumns(dbinfo, "listColumnRecord")
	require.ErrorIs(t, err, ErrNotFound)
	require.Nil(t, columns)
}

func TestListFromsAndTablesWithRoot(t *testing.T) {
	db, cleanup := createDB(t, Root("tenant"))
	defer cleanup()

	require.NoError(t, db.From("repo").Save(&listIssue{ID: 1, Title: "rooted issue"}))
	require.NoError(t, createTopLevelTableMetadata(db, "outside", "listIssue"))

	froms, err := db.ListFroms()
	require.NoError(t, err)
	require.Equal(t, []string{"repo"}, froms)

	tables, err := db.ListTables("repo")
	require.NoError(t, err)
	require.Equal(t, []string{"listIssue"}, tables)
}

// createTopLevelTableMetadata creates a table-shaped bucket outside Root for root-scope tests.
func createTopLevelTableMetadata(db *DB, from, table string) error {
	return db.Bolt.Update(func(tx *bolt.Tx) error {
		fromBucket, err := tx.CreateBucketIfNotExists([]byte(from))
		if err != nil {
			return err
		}

		tableBucket, err := fromBucket.CreateBucketIfNotExists([]byte(table))
		if err != nil {
			return err
		}

		metaBucket, err := tableBucket.CreateBucketIfNotExists([]byte(metadataBucket))
		if err != nil {
			return err
		}

		return metaBucket.Put([]byte(metaSchema), []byte(`{"table":"`+table+`"}`))
	})
}

func columnNames(columns []ColumnInfo) []string {
	names := make([]string, 0, len(columns))
	for _, column := range columns {
		names = append(names, column.Name)
	}
	return names
}

func requireColumn(t *testing.T, columns []ColumnInfo, name string) ColumnInfo {
	t.Helper()

	for _, column := range columns {
		if column.Name == name {
			return column
		}
	}

	require.FailNowf(t, "missing column", "column %s was not returned", name)
	return ColumnInfo{}
}
