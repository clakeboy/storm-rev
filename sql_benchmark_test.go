package storm

import (
	"fmt"
	"testing"

	"github.com/clakeboy/storm-rev/q"
)

type SQLBenchUser struct {
	ID     int    `storm:"id,increment" json:"id"`
	Name   string `storm:"index" json:"name"`
	Age    int    `storm:"index" json:"age"`
	Team   string `storm:"index" json:"team"`
	Active bool   `json:"active"`
}

var (
	sqlBenchUsersSink []SQLBenchUser
	sqlBenchRowsSink  []map[string]any
	sqlBenchCountSink int
)

func prepareSQLBenchmarkDB(b *testing.B, total int) (*DB, *SQL, func()) {
	b.Helper()

	db, cleanup := createDB(b)
	users := make([]*SQLBenchUser, 0, total)
	for i := 0; i < total; i++ {
		team := "admin"
		if i%2 == 0 {
			team = "staff"
		}
		users = append(users, &SQLBenchUser{
			Name:   fmt.Sprintf("user-%04d", i),
			Age:    18 + i%50,
			Team:   team,
			Active: i%3 != 0,
		})
	}
	if err := db.SaveAll(users); err != nil {
		b.Fatal(err)
	}

	sql, err := db.SQL(&SQLBenchUser{})
	if err != nil {
		b.Fatal(err)
	}
	return db, sql, cleanup
}

func BenchmarkSQLTranslationFind(b *testing.B) {
	db, sql, cleanup := prepareSQLBenchmarkDB(b, 1000)
	defer cleanup()

	b.Run("OriginalSelect", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var users []SQLBenchUser
			err := db.Select(
				q.Eq("Team", "staff"),
				q.Gte("Age", 30),
			).OrderBy("Age").Limit(25).Find(&users)
			if err != nil {
				b.Fatal(err)
			}
			sqlBenchUsersSink = users
		}
	})

	b.Run("SQLFind", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var users []SQLBenchUser
			err := sql.Find(
				"SELECT * FROM SQLBenchUser WHERE team = ? AND age >= ? ORDER BY age ASC LIMIT 25",
				&users,
				"staff",
				30,
			)
			if err != nil {
				b.Fatal(err)
			}
			sqlBenchUsersSink = users
		}
	})
}

func BenchmarkSQLTranslationCount(b *testing.B) {
	db, sql, cleanup := prepareSQLBenchmarkDB(b, 1000)
	defer cleanup()

	b.Run("OriginalSelectCount", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			count, err := db.Select(q.Eq("Team", "staff")).Count(new(SQLBenchUser))
			if err != nil {
				b.Fatal(err)
			}
			sqlBenchCountSink = count
		}
	})

	b.Run("SQLCount", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			count, err := sql.Count("SELECT COUNT(*) FROM SQLBenchUser WHERE team = ?", "staff")
			if err != nil {
				b.Fatal(err)
			}
			sqlBenchCountSink = count
		}
	})
}

func BenchmarkSQLTranslationProject(b *testing.B) {
	db, sql, cleanup := prepareSQLBenchmarkDB(b, 1000)
	defer cleanup()

	b.Run("OriginalFindAndProject", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var users []SQLBenchUser
			err := db.Select(q.Eq("Team", "staff")).OrderBy("Age").Limit(25).Find(&users)
			if err != nil {
				b.Fatal(err)
			}
			rows := make([]map[string]any, len(users))
			for i := range users {
				rows[i] = map[string]any{
					"name":     users[i].Name,
					"user_age": users[i].Age,
				}
			}
			sqlBenchRowsSink = rows
		}
	})

	b.Run("SQLProjectMap", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var rows []map[string]any
			err := sql.Project(
				"SELECT name, age AS user_age FROM SQLBenchUser WHERE team = ? ORDER BY age ASC LIMIT 25",
				&rows,
				"staff",
			)
			if err != nil {
				b.Fatal(err)
			}
			sqlBenchRowsSink = rows
		}
	})
}
