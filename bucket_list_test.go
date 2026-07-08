package storm

import (
	"reflect"
	"testing"

	"github.com/clakeboy/storm-rev/codec"
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

type EditableLegacyTable struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Age  int    `json:"age"`
}

type EditableMetadataTable struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
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

func TestSetTableMetadataAddsMetadataToLegacyTable(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	createTableWithoutMetadata(t, db, "", "EditableLegacyTable", &EditableLegacyTable{
		ID:   1,
		Name: "Ada",
		Age:  36,
	})

	columns, err := db.ListColumns("", "EditableLegacyTable")
	require.ErrorIs(t, err, ErrNotFound)
	require.Nil(t, columns)

	metadata := []ColumnInfo{
		{Name: "ID", JSON: "id", Type: "int"},
		{Name: "Name", JSON: "name", Type: "string", Index: tagIdx},
		{Name: "Age", JSON: "age", Type: "int"},
	}
	err = db.SetTableMetadata("", "EditableLegacyTable", metadata)
	require.NoError(t, err)

	columns, err = db.ListColumns("", "EditableLegacyTable")
	require.NoError(t, err)
	require.Equal(t, []string{"ID", "Name", "Age"}, columnNames(columns))
	require.True(t, requireColumn(t, columns, "ID").ID)
	require.Equal(t, tagUniqueIdx, requireColumn(t, columns, "ID").Index)
	require.True(t, requireColumn(t, columns, "Age").Integer)

	sql, err := db.SQL()
	require.NoError(t, err)

	var rows []map[string]any
	err = sql.Project("SELECT id, age FROM EditableLegacyTable WHERE name = ?", &rows, "Ada")
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"id": 1, "age": 36}}, rows)

	cfg := metadataConfig(t, "EditableLegacyTable", metadata)
	ids, err := db.indexer.searchExact(cfg, "Name", "Ada")
	require.NoError(t, err)
	require.Empty(t, ids)

	require.NoError(t, db.ReIndex(nil))

	ids, err = db.indexer.searchExact(cfg, "Name", "Ada")
	require.NoError(t, err)
	require.Len(t, ids, 1)
}

func TestSetTableMetadataEditsExistingMetadata(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Save(&EditableMetadataTable{ID: 1, Name: "Ada"}))

	metadata := []ColumnInfo{
		{Name: "ID", JSON: "id", Type: "int"},
		{Name: "Name", JSON: "name", Type: "string", Index: tagIdx},
		{Name: "Score", JSON: "score", Type: "int"},
	}
	err := db.SetTableMetadata("", "EditableMetadataTable", metadata)
	require.NoError(t, err)

	columns, err := db.ListColumns("", "EditableMetadataTable")
	require.NoError(t, err)
	require.Equal(t, []string{"ID", "Name", "Score"}, columnNames(columns))
	require.Equal(t, tagIdx, requireColumn(t, columns, "Name").Index)

	cfg := metadataConfig(t, "EditableMetadataTable", metadata)
	ids, err := db.indexer.searchExact(cfg, "Name", "Ada")
	require.NoError(t, err)
	require.Empty(t, ids)

	require.NoError(t, db.ReIndex(nil))

	ids, err = db.indexer.searchExact(cfg, "Name", "Ada")
	require.NoError(t, err)
	require.Len(t, ids, 1)

	sql, err := db.SQL()
	require.NoError(t, err)

	result, err := sql.Exec(
		"INSERT INTO EditableMetadataTable (id, name, score) VALUES (?, ?, ?)",
		2,
		"Grace",
		41,
	)
	require.NoError(t, err)
	require.Equal(t, 1, result.RowsAffected)

	var rows []map[string]any
	err = sql.Project("SELECT name, score FROM EditableMetadataTable WHERE name = ?", &rows, "Grace")
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"name": "Grace", "score": 41}}, rows)
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

// createTableWithoutMetadata writes table records without creating Storm schema metadata.
func createTableWithoutMetadata(t *testing.T, db *DB, from, table string, records ...any) {
	t.Helper()

	err := db.Bolt.Update(func(tx *bolt.Tx) error {
		n := db.Node.(*node)
		bucket, err := legacyTableBucket(tx, n, from, table)
		if err != nil {
			return err
		}

		for _, record := range records {
			id, err := legacyRecordID(record, db.Codec())
			if err != nil {
				return err
			}
			raw, err := db.Codec().Marshal(record)
			if err != nil {
				return err
			}
			if err := bucket.Put(id, raw); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
}

func legacyTableBucket(tx *bolt.Tx, n *node, from, table string) (*bolt.Bucket, error) {
	if from == "" {
		return n.CreateBucketIfNotExists(tx, table)
	}

	fromBucket, err := n.CreateBucketIfNotExists(tx, from)
	if err != nil {
		return nil, err
	}
	return fromBucket.CreateBucketIfNotExists([]byte(table))
}

func legacyRecordID(record any, codec codec.MarshalUnmarshaler) ([]byte, error) {
	ref := reflect.Indirect(reflect.ValueOf(record))
	return toBytes(ref.FieldByName("ID").Interface(), codec)
}

func metadataConfig(t *testing.T, table string, columns []ColumnInfo) *structConfig {
	t.Helper()

	schema, err := storedSchemaFromColumns(table, columns)
	require.NoError(t, err)

	return structConfigFromStoredSchema(schema)
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
