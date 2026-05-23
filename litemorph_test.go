package litemorph

import (
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// freshDB returns an isolated in-memory database for a single test.
func freshDB(t *testing.T) *sql.DB {
	t.Helper()
	rn := uint32(0)
	binary.Read(rand.Reader, binary.LittleEndian, &rn)
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:test%d?mode=memory&cache=shared", rn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxIdleConns(0x7fffffff)
	t.Cleanup(func() { db.Close() })
	return db
}

func mustExec(t *testing.T, db *sql.DB, queries ...string) {
	t.Helper()
	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
}

func tableColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query("SELECT * FROM " + table + " WHERE 0 = 1")
	if err != nil {
		t.Fatalf("describe %s: %v", table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns %s: %v", table, err)
	}
	return cols
}

// TestMigrateResidueColumnKept verifies that a column present in the live table
// but absent from the declared schema (a "residue" column) is retained along
// with its data, rather than being dropped.
func TestMigrateResidueColumnKept(t *testing.T) {
	db := freshDB(t)
	mustExec(t, db,
		`CREATE TABLE t(i INT, txt TEXT, b BLOB, r REAL)`,
		`INSERT INTO t (i, txt, b, r) VALUES (1, 'hello', x'0000', 1.22), (2, 'hello2', x'00', 1.2)`,
	)

	// Declared schema omits column r; litemorph keeps it as residue.
	if err := NewMigrator(db).Migrate([]string{`CREATE TABLE t(i INT, txt TEXT, b BLOB)`}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if got := tableColumns(t, db, "t"); len(got) != 4 {
		t.Fatalf("expected residue column r to be kept (4 columns), got %v", got)
	}

	var i int
	var txt string
	var r float64
	if err := db.QueryRow(`SELECT i, txt, r FROM t WHERE i = 1`).Scan(&i, &txt, &r); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if i != 1 || txt != "hello" || r != 1.22 {
		t.Fatalf("data not preserved: i=%d txt=%q r=%v", i, txt, r)
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 rows preserved, got %d", n)
	}
}

// TestMigrateAddColumn adds a column and verifies existing rows survive.
func TestMigrateAddColumn(t *testing.T) {
	db := freshDB(t)
	mustExec(t, db,
		`CREATE TABLE t(i INT, txt TEXT)`,
		`INSERT INTO t (i, txt) VALUES (1, 'a'), (2, 'b')`,
	)

	if err := NewMigrator(db).Migrate([]string{`CREATE TABLE t(i INT, txt TEXT, extra INT)`}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if got := tableColumns(t, db, "t"); len(got) != 3 {
		t.Fatalf("expected 3 columns after add, got %v", got)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 rows preserved, got %d", n)
	}
}

// TestMigrateCreateTable creates a table that does not yet exist.
func TestMigrateCreateTable(t *testing.T) {
	db := freshDB(t)

	if err := NewMigrator(db).Migrate([]string{`CREATE TABLE fresh(id INTEGER PRIMARY KEY, name TEXT)`}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := tableColumns(t, db, "fresh"); len(got) != 2 {
		t.Fatalf("expected 2 columns, got %v", got)
	}
}

// TestMigrateUpToDate reports UpToDateDecision when nothing changes.
func TestMigrateUpToDate(t *testing.T) {
	db := freshDB(t)
	mustExec(t, db, `CREATE TABLE t(i INT, txt TEXT)`)

	var decided Decision = -1
	cb := func(_ *Migrator, d Decision, subj string) {
		if subj == "t" {
			decided = d
		}
	}
	if err := NewMigrator(db, WithDecisionCallback(cb)).Migrate([]string{`CREATE TABLE t(i INT, txt TEXT)`}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if decided != UpToDateDecision {
		t.Fatalf("expected UpToDateDecision, got %v", decided)
	}
}

// TestMigrateConcurrent runs many migrations at once to ensure the per-call
// in-memory database names (atomic counter) don't collide across goroutines.
func TestMigrateConcurrent(t *testing.T) {
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for k := 0; k < n; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			db := freshDB(t)
			if _, err := db.Exec(`CREATE TABLE t(i INT, txt TEXT)`); err != nil {
				errs[k] = err
				return
			}
			if _, err := db.Exec(`INSERT INTO t (i, txt) VALUES (1, 'x')`); err != nil {
				errs[k] = err
				return
			}
			errs[k] = NewMigrator(db).Migrate([]string{`CREATE TABLE t(i INT, txt TEXT, added INT)`})
		}(k)
	}
	wg.Wait()
	for k, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", k, err)
		}
	}
}
