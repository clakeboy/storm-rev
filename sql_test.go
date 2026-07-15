package storm

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

type SQLUser struct {
	ID     int    `storm:"id,increment" json:"id"`
	Name   string `storm:"index" json:"name"`
	Age    int    `storm:"index" json:"age"`
	Team   string `storm:"index" json:"team"`
	Active bool   `json:"active"`
}

type SQLUserDTO struct {
	Username string `json:"username"`
	UserAge  int    `json:"user_age"`
}

func prepareSQLUsers(t *testing.T) (*DB, *SQL, func()) {
	db, cleanup := createDB(t)
	users := []*SQLUser{
		{Name: "Alice", Age: 31, Team: "staff", Active: true},
		{Name: "Bob", Age: 22, Team: "admin", Active: true},
		{Name: "Carol", Age: 17, Team: "guest", Active: false},
		{Name: "Dave", Age: 42, Team: "staff", Active: true},
	}
	require.NoError(t, db.SaveAll(users))

	sql, err := db.SQL(&SQLUser{})
	require.NoError(t, err)
	return db, sql, cleanup
}

func TestSQLFindProjectAndCount(t *testing.T) {
	_, sql, cleanup := prepareSQLUsers(t)
	defer cleanup()

	var users []SQLUser
	err := sql.Find("SELECT * FROM SQLUser WHERE age >= ? AND team IN (?, ?) ORDER BY age DESC LIMIT 2", &users, 18, "staff", "admin")
	require.NoError(t, err)
	require.Equal(t, []string{"Dave", "Alice"}, []string{users[0].Name, users[1].Name})

	count, err := sql.Count("SELECT COUNT(*) FROM SQLUser WHERE team IN (?, ?)", "staff", "admin")
	require.NoError(t, err)
	require.Equal(t, 3, count)

	var rows []map[string]any
	err = sql.Project("SELECT name, age AS user_age FROM SQLUser WHERE age > ? ORDER BY age ASC", &rows, 18)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{
		{"name": "Bob", "user_age": 22},
		{"name": "Alice", "user_age": 31},
		{"name": "Dave", "user_age": 42},
	}, rows)

	var dto []SQLUserDTO
	err = sql.Project("SELECT name AS username, age AS user_age FROM SQLUser WHERE team = ? ORDER BY age ASC", &dto, "staff")
	require.NoError(t, err)
	require.Equal(t, []SQLUserDTO{
		{Username: "Alice", UserAge: 31},
		{Username: "Dave", UserAge: 42},
	}, dto)

	var starRows []map[string]any
	err = sql.Project("SELECT * FROM SQLUser WHERE name = ?", &starRows, "Bob")
	require.NoError(t, err)
	require.Equal(t, "Bob", starRows[0]["name"])
	require.Equal(t, 22, starRows[0]["age"])
	require.Equal(t, "admin", starRows[0]["team"])
}

func TestSQLProjectAndCountFromMetadata(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Init(&SQLUser{}))
	require.NoError(t, db.SaveAll([]*SQLUser{
		{Name: "Alice", Age: 31, Team: "staff", Active: true},
		{Name: "Bob", Age: 22, Team: "admin", Active: true},
		{Name: "Carol", Age: 17, Team: "guest", Active: false},
	}))

	sql, err := db.SQL()
	require.NoError(t, err)

	count, err := sql.Count("SELECT COUNT(*) FROM SQLUser WHERE age >= ?", 18)
	require.NoError(t, err)
	require.Equal(t, 2, count)

	var rows []map[string]any
	err = sql.Project("SELECT name, age AS user_age FROM SQLUser WHERE age >= ? ORDER BY age DESC", &rows, 18)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{
		{"name": "Alice", "user_age": 31},
		{"name": "Bob", "user_age": 22},
	}, rows)

	var dto []SQLUserDTO
	err = sql.Project("SELECT name AS username, age AS user_age FROM SQLUser WHERE team = ?", &dto, "admin")
	require.NoError(t, err)
	require.Equal(t, []SQLUserDTO{{Username: "Bob", UserAge: 22}}, dto)

	var users []SQLUser
	err = sql.Find("SELECT * FROM SQLUser WHERE name = ?", &users, "Alice")
	require.NoError(t, err)
	require.Len(t, users, 1)
	require.Equal(t, "Alice", users[0].Name)
}

func TestSQLMetadataRawIDCursorOrderLimit(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Init(&SQLUser{}))
	users := make([]*SQLUser, 0, 120)
	for i := 1; i <= 120; i++ {
		users = append(users, &SQLUser{
			Name:   fmt.Sprintf("User%03d", i),
			Age:    i,
			Team:   "staff",
			Active: true,
		})
	}
	require.NoError(t, db.SaveAll(users))

	sql, err := db.SQL()
	require.NoError(t, err)

	var descRows []map[string]any
	err = sql.Project("SELECT id, name FROM SQLUser ORDER BY id DESC LIMIT 5", &descRows)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{
		{"id": 120, "name": "User120"},
		{"id": 119, "name": "User119"},
		{"id": 118, "name": "User118"},
		{"id": 117, "name": "User117"},
		{"id": 116, "name": "User116"},
	}, descRows)

	var ascRows []map[string]any
	err = sql.Project("SELECT id, name FROM SQLUser ORDER BY ID ASC LIMIT 5 OFFSET 10", &ascRows)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{
		{"id": 11, "name": "User011"},
		{"id": 12, "name": "User012"},
		{"id": 13, "name": "User013"},
		{"id": 14, "name": "User014"},
		{"id": 15, "name": "User015"},
	}, ascRows)

	count, err := sql.Count("SELECT COUNT(*) FROM SQLUser ORDER BY id DESC LIMIT 5")
	require.NoError(t, err)
	require.Equal(t, 5, count)
}

func TestSQLMetadataRawIDCursorOrderEligibility(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Init(&SQLUser{}))

	sql, err := db.SQL()
	require.NoError(t, err)

	eligible := buildSQLSelectPlanForTest(t, sql, "SELECT id FROM SQLUser ORDER BY id DESC LIMIT 5")
	require.True(t, eligible.canUseRawIDCursorOrder())

	withWhere := buildSQLSelectPlanForTest(t, sql, "SELECT id FROM SQLUser WHERE age >= ? ORDER BY id DESC LIMIT 5", 18)
	require.False(t, withWhere.canUseRawIDCursorOrder())

	byNonID := buildSQLSelectPlanForTest(t, sql, "SELECT id FROM SQLUser ORDER BY age DESC LIMIT 5")
	require.False(t, byNonID.canUseRawIDCursorOrder())

	multiOrder := buildSQLSelectPlanForTest(t, sql, "SELECT id FROM SQLUser ORDER BY id DESC, age DESC LIMIT 5")
	require.False(t, multiOrder.canUseRawIDCursorOrder())

	withoutLimit := buildSQLSelectPlanForTest(t, sql, "SELECT id FROM SQLUser ORDER BY id DESC")
	require.False(t, withoutLimit.canUseRawIDCursorOrder())
}

func TestSQLMetadataRawIndexedOrderLimit(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Init(&SQLUser{}))
	users := make([]*SQLUser, 0, 120)
	for i := 1; i <= 120; i++ {
		users = append(users, &SQLUser{
			Name:   fmt.Sprintf("User%03d", i),
			Age:    i,
			Team:   "staff",
			Active: true,
		})
	}
	require.NoError(t, db.SaveAll(users))

	sql, err := db.SQL()
	require.NoError(t, err)

	descPlan := buildSQLSelectPlanForTest(t, sql, "SELECT id, age FROM SQLUser ORDER BY age DESC LIMIT 5")
	_, ok := descPlan.rawIndexedOrderField()
	require.True(t, ok)
	_, used, err := sql.selectRawRecordsByIndexedOrder(descPlan)
	require.NoError(t, err)
	require.True(t, used)

	var descRows []map[string]any
	err = sql.Project("SELECT id, age FROM SQLUser ORDER BY age DESC LIMIT 5", &descRows)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{
		{"id": 120, "age": 120},
		{"id": 119, "age": 119},
		{"id": 118, "age": 118},
		{"id": 117, "age": 117},
		{"id": 116, "age": 116},
	}, descRows)

	var ascRows []map[string]any
	err = sql.Project("SELECT id, age FROM SQLUser ORDER BY age ASC LIMIT 5 OFFSET 10", &ascRows)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{
		{"id": 11, "age": 11},
		{"id": 12, "age": 12},
		{"id": 13, "age": 13},
		{"id": 14, "age": 14},
		{"id": 15, "age": 15},
	}, ascRows)

	count, err := sql.Count("SELECT COUNT(*) FROM SQLUser ORDER BY age DESC LIMIT 5")
	require.NoError(t, err)
	require.Equal(t, 5, count)
}

func TestSQLMetadataRawIndexedOrderCoverageLifecycle(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Init(&SQLUser{}))
	require.NoError(t, db.SaveAll([]*SQLUser{
		{Name: "Alice", Age: 10, Team: "staff", Active: true},
		{Name: "Bob", Age: 20, Team: "staff", Active: true},
		{Name: "Carol", Age: 30, Team: "staff", Active: true},
	}))

	sql, err := db.SQL()
	require.NoError(t, err)
	plan := buildSQLSelectPlanForTest(t, sql, "SELECT id, age FROM SQLUser ORDER BY age ASC LIMIT 5")
	requireIndexedRawOrderUsed(t, sql, plan, true)

	_, err = sql.Exec("UPDATE SQLUser SET age = ? WHERE id = ?", 0, 1)
	require.NoError(t, err)
	requireIndexedRawOrderUsed(t, sql, plan, false)

	var rows []map[string]any
	err = sql.Project("SELECT id, age FROM SQLUser ORDER BY age ASC LIMIT 1", &rows)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"id": 1, "age": 0}}, rows)

	_, err = sql.Exec("UPDATE SQLUser SET age = ? WHERE id = ?", 40, 1)
	require.NoError(t, err)
	requireIndexedRawOrderUsed(t, sql, plan, true)

	_, err = sql.Exec("DELETE FROM SQLUser WHERE id = ?", 3)
	require.NoError(t, err)
	requireIndexedRawOrderUsed(t, sql, plan, true)

	raw, err := db.GetBytes("SQLUser", 1)
	require.NoError(t, err)
	require.NoError(t, db.SetBytes("SQLUser", 1, raw))
	requireIndexedRawOrderUsed(t, sql, plan, false)

	require.NoError(t, db.ReIndex(nil))
	requireIndexedRawOrderUsed(t, sql, plan, true)

	columns, err := db.ListColumns("", "SQLUser")
	require.NoError(t, err)
	for i := range columns {
		if columns[i].Name == "Age" {
			columns[i].Index = tagUniqueIdx
		}
	}
	require.NoError(t, db.SetTableMetadata("", "SQLUser", columns))
	requireIndexedRawOrderUsed(t, sql, plan, false)

	require.NoError(t, db.ReIndex(nil))
	requireIndexedRawOrderUsed(t, sql, plan, true)
}

func TestSQLMetadataRawIndexedOrderEligibility(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Init(&SQLUser{}))
	sql, err := db.SQL()
	require.NoError(t, err)

	eligible := buildSQLSelectPlanForTest(t, sql, "SELECT id FROM SQLUser ORDER BY age DESC LIMIT 5")
	_, ok := eligible.rawIndexedOrderField()
	require.True(t, ok)

	withWhere := buildSQLSelectPlanForTest(t, sql, "SELECT id FROM SQLUser WHERE age >= ? ORDER BY age DESC LIMIT 5", 18)
	_, ok = withWhere.rawIndexedOrderField()
	require.False(t, ok)

	byNonIndex := buildSQLSelectPlanForTest(t, sql, "SELECT id FROM SQLUser ORDER BY active DESC LIMIT 5")
	_, ok = byNonIndex.rawIndexedOrderField()
	require.False(t, ok)

	multiOrder := buildSQLSelectPlanForTest(t, sql, "SELECT id FROM SQLUser ORDER BY age DESC, name DESC LIMIT 5")
	_, ok = multiOrder.rawIndexedOrderField()
	require.False(t, ok)

	withoutLimit := buildSQLSelectPlanForTest(t, sql, "SELECT id FROM SQLUser ORDER BY age DESC")
	_, ok = withoutLimit.rawIndexedOrderField()
	require.False(t, ok)
}

func requireIndexedRawOrderUsed(t *testing.T, sql *SQL, plan *sqlSelectPlan, expected bool) {
	t.Helper()
	_, used, err := sql.selectRawRecordsByIndexedOrder(plan)
	require.NoError(t, err)
	require.Equal(t, expected, used)
}

func TestSQLMetadataExecMaintainsIndexes(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	require.NoError(t, db.Init(&SQLUser{}))
	sql, err := db.SQL()
	require.NoError(t, err)

	result, err := sql.Exec(
		"INSERT INTO SQLUser (name, age, team, active) VALUES (?, ?, ?, ?), (?, ?, ?, ?)",
		"Eve", 28, "staff", true,
		"Frank", 35, "admin", false,
	)
	require.NoError(t, err)
	require.Equal(t, 2, result.RowsAffected)
	require.Equal(t, 2, result.LastInsertID)

	var users []SQLUser
	require.NoError(t, db.Find("Name", "Eve", &users))
	require.Len(t, users, 1)
	require.Equal(t, 28, users[0].Age)

	result, err = sql.Exec("UPDATE SQLUser SET team = ?, age = ? WHERE name = ?", "ops", 29, "Eve")
	require.NoError(t, err)
	require.Equal(t, 1, result.RowsAffected)

	users = nil
	require.NoError(t, db.Find("Team", "ops", &users))
	require.Len(t, users, 1)
	require.Equal(t, "Eve", users[0].Name)
	require.Equal(t, 29, users[0].Age)

	result, err = sql.Exec("DELETE FROM SQLUser WHERE team = ?", "admin")
	require.NoError(t, err)
	require.Equal(t, 1, result.RowsAffected)

	users = nil
	err = db.Find("Name", "Frank", &users)
	require.ErrorIs(t, err, ErrNotFound)

	count, err := sql.Count("SELECT COUNT(*) FROM SQLUser")
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestSQLExecInsertUpdateDelete(t *testing.T) {
	db, cleanup := createDB(t)
	defer cleanup()

	sql, err := db.SQL(&SQLUser{})
	require.NoError(t, err)

	result, err := sql.Exec(
		"INSERT INTO SQLUser (name, age, team, active) VALUES (?, ?, ?, ?), (?, ?, ?, ?)",
		"Eve", 28, "staff", true,
		"Frank", 35, "admin", false,
	)
	require.NoError(t, err)
	require.Equal(t, 2, result.RowsAffected)
	require.Equal(t, 2, result.LastInsertID)

	_, err = sql.Exec("UPDATE SQLUser SET team = ?", "blocked")
	require.ErrorIs(t, err, ErrSQLUnsafeWrite)

	result, err = sql.Exec("UPDATE SQLUser SET team = ?, age = ? WHERE name = ?", "staff", 29, "Eve")
	require.NoError(t, err)
	require.Equal(t, 1, result.RowsAffected)

	var users []SQLUser
	err = sql.Find("SELECT * FROM SQLUser WHERE team = ? ORDER BY age ASC", &users, "staff")
	require.NoError(t, err)
	require.Len(t, users, 1)
	require.Equal(t, "Eve", users[0].Name)
	require.Equal(t, 29, users[0].Age)

	_, err = sql.Exec("DELETE FROM SQLUser")
	require.ErrorIs(t, err, ErrSQLUnsafeWrite)

	result, err = sql.Exec("DELETE FROM SQLUser WHERE active = ?", false)
	require.NoError(t, err)
	require.Equal(t, 1, result.RowsAffected)

	count, err := sql.Count("SELECT COUNT(*) FROM SQLUser")
	require.NoError(t, err)
	require.Equal(t, 1, count)

	result, err = sql.WithAllowFullTableWrite(true).Exec("DELETE FROM SQLUser")
	require.NoError(t, err)
	require.Equal(t, 1, result.RowsAffected)

	count, err = sql.Count("SELECT COUNT(*) FROM SQLUser")
	require.NoError(t, err)
	require.Equal(t, 0, count)
}

func TestSQLErrors(t *testing.T) {
	_, sql, cleanup := prepareSQLUsers(t)
	defer cleanup()

	var users []SQLUser
	err := sql.Find("SELECT name FROM SQLUser", &users)
	require.ErrorIs(t, err, ErrUnsupportedSQL)

	_, err = sql.Count("SELECT COUNT(*) FROM MissingUser")
	require.ErrorIs(t, err, ErrSQLTableNotRegistered)

	_, err = sql.Count("SELECT COUNT(*) FROM SQLUser WHERE missing = ?", 1)
	require.ErrorIs(t, err, ErrSQLUnknownField)

	err = sql.Project("SELECT name FROM SQLUser WHERE age > ?", &[]map[string]any{})
	require.ErrorIs(t, err, ErrSQLArguments)
}

func buildSQLSelectPlanForTest(t *testing.T, sql *SQL, query string, args ...any) *sqlSelectPlan {
	t.Helper()

	stmt, reader, err := sql.parseSelect(query, args)
	require.NoError(t, err)
	plan, err := sql.buildSelectPlan(stmt, reader)
	require.NoError(t, err)
	require.NoError(t, reader.done())
	return plan
}
