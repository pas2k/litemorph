// Package litemorph performs declarative schema migrations for SQLite.
//
// Instead of writing versioned up/down migration scripts, you declare the
// schema you want as a set of CREATE statements. litemorph loads that target
// schema into an in-memory SQLite database, compares it against the live
// database's sqlite_master, and builds and executes a plan that morphs the
// live schema into the declared one (following SQLite's recommended
// create-temp / copy / drop / rename procedure for table changes).
//
// litemorph never parses SQL itself; it lets SQLite do the parsing by loading
// schemas into in-memory databases and inspecting the result. It is therefore
// SQLite-specific by design and depends on github.com/mattn/go-sqlite3.
package litemorph

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"

	_ "github.com/mattn/go-sqlite3"
)

// Decision identifies what litemorph concluded about a table while planning a
// migration. It is reported through the callback registered with
// WithDecisionCallback.
type Decision int

const (
	// UpToDateDecision means the live table already matches the target schema.
	UpToDateDecision Decision = iota
	// NeedsMigrationDecision means the live table differs and will be rebuilt.
	NeedsMigrationDecision
	// MigrationReadyDecision means a plan for the table has been produced.
	MigrationReadyDecision
)

const (
	// AllColumns is the entry_name used for plan steps that operate on every
	// column of a table rather than a single named column.
	AllColumns = "<all_columns>"
)

// Option configures a Migrator created with NewMigrator.
type Option func(m *Migrator)

// QuirksFunc is a hook that can inspect or rewrite the migration plan before it
// is executed. It receives the live database (exDb) and the in-memory plan
// database (planDb), whose "plan" table holds the ordered migration steps.
type QuirksFunc func(exDb, planDb *sql.DB) error

// WithDecisionCallback registers a callback invoked for each table as litemorph
// decides how (or whether) to migrate it.
func WithDecisionCallback(dc func(m *Migrator, d Decision, subj string)) Option {
	return func(m *Migrator) {
		m.callback = dc
	}
}

// WithQuirksFunc registers a QuirksFunc that runs against the plan database
// before the plan is executed.
func WithQuirksFunc(qf QuirksFunc) Option {
	return func(m *Migrator) {
		m.quirksFunc = qf
	}
}

// WithQuirks registers a set of SQL statements to execute against the plan
// database before the plan is executed, e.g. to delete or adjust steps.
func WithQuirks(qs []string) Option {
	return WithQuirksFunc(func(exDb, planDb *sql.DB) error {
		for _, q := range qs {
			if _, err := planDb.Exec(q); err != nil {
				return &ActionError{Context: "planDb(quirk)", Query: q, Err: err}
			}
		}
		return nil
	})
}

// Migrator migrates the schema of a SQLite database toward a declared target.
type Migrator struct {
	callback   func(m *Migrator, d Decision, subj string)
	quirksFunc QuirksFunc
	// DB is the live database whose schema will be migrated.
	DB *sql.DB
}

// NewMigrator returns a Migrator for the given live database, configured with
// the supplied options.
func NewMigrator(db *sql.DB, opts ...Option) *Migrator {
	ret := &Migrator{DB: db}
	for _, o := range opts {
		o(ret)
	}
	return ret
}

// ActionError describes a failure while executing a SQL statement, including
// the statement and the context in which it ran.
type ActionError struct {
	Context string
	Query   string
	Err     error
}

func (ae *ActionError) Error() string {
	return fmt.Sprintf("While executing %v on %v: %v", ae.Query, ae.Context, ae.Err)
}

// Unwrap returns the underlying error so ActionError works with errors.Is/As.
func (ae *ActionError) Unwrap() error {
	return ae.Err
}

// SchemaEntry mirrors a row of sqlite_master.
type SchemaEntry struct {
	Type      string
	Name      string
	TableName string
	SQL       string
}

// SchemaEntries is a list of sqlite_master rows.
type SchemaEntries []SchemaEntry

// OfType returns the entries whose Type equals t (e.g. "table", "index").
func (se SchemaEntries) OfType(t string) SchemaEntries {
	ret := SchemaEntries{}
	for _, v := range se {
		if v.Type == t {
			ret = append(ret, v)
		}
	}
	return ret
}

// Column is a column name/type pair.
type Column struct {
	Name string
	Type string
}

var safeIndent = regexp.MustCompile("[a-zA-Z_][a-zA-Z0-9_]*")

var parseSpace = regexp.MustCompile(`\s+`)

var significantParsers = []*regexp.Regexp{
	regexp.MustCompile("^[a-zA-Z0-9_]+"),
	regexp.MustCompile(`^[\(\),;:\.\+\-\*/&\^$%!=]`),
	regexp.MustCompile(`^"[^"]*"`),
	regexp.MustCompile(`^\[[^\]]*\]`),
	regexp.MustCompile(`^'[^']*'`),
	regexp.MustCompile("^`[^`]*`"),
}

func splitSql(s string) []string {
	b := []byte(s)
	ret := []string{}
topLoop:
	for s != "" {
		for _, sp := range significantParsers {
			bs := sp.FindSubmatch(b)
			if bs == nil {
				continue
			}
			ret = append(ret, string(bs[0]))
			b = b[len(bs[0]):]
			continue topLoop
		}
		bs := parseSpace.FindSubmatch(b)
		if bs == nil {
			if len(b) != 0 {
				ret = append(ret, string(b))
			}
			break
		}
		b = b[len(bs[0]):]
	}
	return ret
}

func isQuoted(s string) bool {
	if len(s) < 2 {
		return false
	}
	switch s[0] {
	case '"':
		fallthrough
	case '\'':
		fallthrough
	case '`':
		return s[len(s)-1] == s[0]
	case '[':
		return s[len(s)-1] == ']'
	}
	return false
}

func matchSql(s1, s2 string) bool {
	sq1 := splitSql(s1)
	sq2 := splitSql(s2)
	if len(sq1) != len(sq2) {
		return false
	}
	for i, s1 := range sq1 {
		s2 = sq2[i]
		hasQuoted := false
		if isQuoted(s1) {
			s1 = s1[1 : len(s1)-1]
			hasQuoted = true
		}
		if isQuoted(s2) {
			s2 = s2[1 : len(s2)-1]
			hasQuoted = true
		}
		if !hasQuoted {
			// Case-insensitive for unquoted identifiers/keywords.
			if !strings.EqualFold(s2, s1) {
				return false
			}
		} else {
			if s1 != s2 {
				return false
			}
		}
	}
	return true
}

func normalizeIdent(i string) string {
	i = strings.TrimSpace(i)
	if safeIndent.Match([]byte(i)) {
		return i
	}
	i = strings.ReplaceAll(i, "\"", "")
	i = strings.TrimSpace(i)
	return "\"" + i + "\""
}

func getColumns(db *sql.DB, table string) (cols []*sql.ColumnType, err error) {
	table = normalizeIdent(table)
	rows, err := db.Query("SELECT * FROM " + table + " WHERE 0 = 1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return rows.ColumnTypes()
}

func getSqliteSchema(db *sql.DB) (se SchemaEntries, err error) {
	ret := SchemaEntries{}
	rows, err := db.Query("SELECT type, name, tbl_name, sql FROM sqlite_master")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e := SchemaEntry{}
		if err := rows.Scan(&e.Type, &e.Name, &e.TableName, &e.SQL); err != nil {
			return nil, err
		}
		ret = append(ret, e)
	}
	return ret, rows.Err()
}

// memDbCounter makes the in-memory database names unique per Migrate call.
var memDbCounter uint64

func newMemDb(data string) (*sql.DB, error) {
	memDb, err := sql.Open("sqlite3", "file:"+data+"?mode=memory&cache=shared")
	if err != nil {
		return nil, err
	}
	memDb.SetConnMaxIdleTime(0)
	memDb.SetMaxIdleConns(0x7fffffff)
	return memDb, nil
}

func (m *Migrator) makeDecision(d Decision, subj string) {
	if m.callback == nil {
		return
	}
	m.callback(m, d, subj)
}

func compareIndent(a, b string) bool {
	a = normalizeIdent(a)
	b = normalizeIdent(b)
	return strings.EqualFold(a, b)
}

// Migrate brings the live database's schema in line with tgtSchema, a list of
// SQL statements (typically CREATE TABLE / CREATE INDEX) describing the desired
// end state. Tables that already match are left untouched; tables that differ
// are rebuilt while preserving the data of columns common to both schemas.
func (m *Migrator) Migrate(tgtSchema []string) error {
	// Unique per call so concurrent migrations in the same process never share
	// the same in-memory databases. go-sqlite3 needs cache=shared with a stable
	// name to share one in-memory DB across the pool's connections, so a private
	// ":memory:" is not usable here; we make the shared name unique instead.
	id := atomic.AddUint64(&memDbCounter, 1)

	dbName := "memdb"
	memDb, err := newMemDb(fmt.Sprintf("mem%d", id))
	if err != nil {
		return err
	}
	defer memDb.Close()
	for _, q := range tgtSchema {
		if _, err := memDb.Exec(q); err != nil {
			return &ActionError{dbName, q, err}
		}
	}

	planDb, err := newMemDb(fmt.Sprintf("plan%d", id))
	if err != nil {
		return err
	}
	defer planDb.Close()

	// quirkDb is shared across all tables that need migration within this call;
	// it is where target tables are materialized and renamed to discover their
	// canonical SQL.
	quirkDb, err := newMemDb(fmt.Sprintf("quirk%d", id))
	if err != nil {
		return err
	}
	defer quirkDb.Close()
	{
		planSchema := "CREATE TABLE plan(step_nr INT, action TEXT, tbl_name TEXT, temp_name TEXT, entry_name TEXT, sql TEXT)"
		if _, err := planDb.Exec(planSchema); err != nil {
			return &ActionError{dbName, planSchema, err}
		}
	}

	// 1. Make new table schemas in memory (new_temp).
	// 2. Get ex-schema table columns.
	// 3. Copy intersecting old tables into memory buf (copy_ex_schema).
	// 4. Residue = difference between ex-schema columns and new table schemas.
	// 5. Add alpha-sorted residues into new_temp, don't mod copy_ex_schema, as
	//    re-ordering will trigger copies anyway.
	// 6. If copy_ex_schema:sqlite_master[table, i] == new_temp:sqlite_master[table, i], skip copying.

	memEnts, err := getSqliteSchema(memDb)
	if err != nil {
		return &ActionError{dbName, "GETSCHEMA", err}
	}
	exEnts, err := getSqliteSchema(m.DB)
	if err != nil {
		return &ActionError{"ex", "GETSCHEMA", err}
	}
	stepCounter := 0
	nextStep := func() int {
		stepCounter += 1
		return stepCounter * 100
	}
	for _, newt := range memEnts.OfType("table") {
		// Ex-table.
		ext := SchemaEntry{}
		for _, mbext := range exEnts {
			if mbext.Type != "table" {
				continue
			}
			if compareIndent(mbext.Name, newt.Name) {
				ext = mbext
				break
			}
		}
		if ext.Type == "" {
			insertStep(planDb, nextStep(), "create_new_table", newt.Name, newt.Name, newt.Name, newt.SQL)
		} else {
			// Residue = extCols - newCols.
			newCols, err := getColumns(memDb, newt.Name)
			if err != nil {
				return &ActionError{dbName, "GETCOL", err}
			}
			extCols, err := getColumns(m.DB, ext.Name)
			if err != nil {
				return &ActionError{"ext", "GETCOL", err}
			}
			residues, sortedResidues := colDifference(extCols, newCols)
			// Add residues to table template.
			columnAdditions := []string{}
			for _, sr := range sortedResidues {
				col := residues[sr]
				columnAdditions = append(columnAdditions, col.Name())
				if _, err := memDb.Exec("ALTER TABLE " + normalizeIdent(newt.Name) + " ADD COLUMN " + normalizeIdent(col.Name())); err != nil {
					return err
				}
			}
			// Compare with existing.
			newSql, err := getSqlForTable(memDb, newt.Name)
			if err != nil {
				return &ActionError{"mem", "cmpschema", err}
			}
			// If matches, skip migration.
			if matchSql(ext.SQL, newSql) {
				m.makeDecision(UpToDateDecision, newt.Name)
				continue
			}
			// Write a plan:
			//    step_nr INT, action TEXT, tbl_name TEXT, temp_name TEXT, entry_name TEXT, sql TEXT
			//
			//    M.1. Rename new_temp into random temp to get updated SQL in sqlite_master.
			//    M.2. Create new table.
			//    M.3. Add residue columns.
			//    M.4. Copy intersecting columns.
			//    M.5. Copy residue data.
			//    M.6. Drop associated indices.
			//    M.7. Drop old table.
			//    M.8. Rename new into old.
			//
			// Then run quirks against the plan, drop triggers/views, execute the
			// plan, and re-add triggers/views/missing indices from mem_db.

			m.makeDecision(NeedsMigrationDecision, newt.Name)

			if _, err := quirkDb.Exec(newt.SQL); err != nil {
				return err
			}
			tmpName := mkTempName()
			if _, err := quirkDb.Exec(fmt.Sprint("ALTER TABLE ", normalizeIdent(newt.Name), " RENAME TO ", tmpName)); err != nil {
				return err
			}
			primaryKeyName := "rowid"
			{
				countIsZero := 0
				row := quirkDb.QueryRow("SELECT count(*)==0 FROM pragma_index_info(?)", normalizeIdent(tmpName))
				if err := row.Scan(&countIsZero); err != nil {
					return err
				}
				if countIsZero != 1 {
					row := quirkDb.QueryRow("SELECT name FROM pragma_table_info(?) WHERE pk = 1", normalizeIdent(tmpName))
					if err := row.Scan(&primaryKeyName); err != nil {
						return err
					}
					primaryKeyName = normalizeIdent(primaryKeyName)
				}
			}

			tmpSql, err := getSqlForTable(quirkDb, tmpName)
			if err != nil {
				return err
			}
			insertStep(planDb, nextStep(), "create_table", newt.Name, tmpName, tmpName, tmpSql)
			for _, col := range columnAdditions {
				insertStep(planDb, nextStep(), "add_residue", newt.Name, tmpName, col,
					fmt.Sprint("ALTER TABLE ", tmpName, " ADD COLUMN ", normalizeIdent(col)))
			}
			{
				// Don't copy new columns.
				interCols, err := getColumns(quirkDb, tmpName)
				if err != nil {
					return err
				}
				colNames := ""
				for _, v := range interCols {
					found := false
					for _, ev := range extCols {
						if ev.Name() == v.Name() {
							found = true
							break
						}
					}
					if !found {
						continue
					}
					colNames += v.Name()
					colNames += ", "
				}
				if colNames != "" {
					colNames = colNames[:len(colNames)-2]
				}
				if primaryKeyName == "rowid" {
					if colNames != "" {
						colNames += ", "
					}
					colNames = colNames + "rowid"
				}
				insertStep(planDb, nextStep(), "copy_intersecting", newt.Name, tmpName, AllColumns, fmt.Sprint("INSERT INTO ", tmpName, " (", colNames, ") SELECT ", colNames, " FROM ", newt.Name))
			}
			for _, tidx := range exEnts.OfType("index") {
				if strings.HasPrefix(tidx.Name, "sqlite_autoindex_") {
					continue
				}
				if compareIndent(tidx.TableName, newt.Name) {
					insertStep(planDb, nextStep(), "drop_index", newt.Name, tmpName, tidx.Name, fmt.Sprint("DROP INDEX ", normalizeIdent(tidx.Name)))
				}
			}
			for _, col := range columnAdditions {
				insertStep(planDb, nextStep(), "copy_residue", newt.Name, tmpName, col,
					fmt.Sprint("UPDATE ", tmpName,
						" SET ", col, "=", normalizeIdent(newt.Name), ".", col,
						" FROM ", normalizeIdent(newt.Name),
						" WHERE ", tmpName, ".", primaryKeyName, " = ", normalizeIdent(newt.Name), ".", primaryKeyName))
			}
			insertStep(planDb, nextStep(), "drop_old", newt.Name, tmpName, tmpName, fmt.Sprint("DROP TABLE ", normalizeIdent(newt.Name)))
			insertStep(planDb, nextStep(), "rename_temp", newt.Name, tmpName, tmpName, fmt.Sprint("ALTER TABLE ", tmpName, " RENAME TO ", normalizeIdent(newt.Name)))
			if m.quirksFunc != nil {
				if err := m.quirksFunc(m.DB, planDb); err != nil {
					return err
				}
			}
		}

		m.makeDecision(MigrationReadyDecision, newt.Name)
	}

	ctx, cancelTx := context.WithCancel(context.Background())
	defer cancelTx()
	if _, err := m.DB.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	tx, err := m.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	{
		rows, err := planDb.Query("SELECT step_nr, action, sql FROM plan ORDER BY step_nr ASC")
		if err != nil {
			return err
		}
		var (
			step   int
			action string
			sqlStr string
		)
		for rows.Next() {
			if err := rows.Scan(&step, &action, &sqlStr); err != nil {
				rows.Close()
				return &ActionError{Context: action, Query: sqlStr, Err: err}
			}
			if _, err := tx.Exec(sqlStr); err != nil {
				rows.Close()
				return &ActionError{Context: action, Query: sqlStr, Err: err}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := m.DB.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return err
	}

	// The working databases (memDb, planDb, quirkDb) are uniquely named per call
	// and closed via defer, which releases their in-memory storage. No manual
	// table cleanup is required.
	return nil
}

func insertStep(db *sql.DB, step_nr int, action, tbl_name, temp_name, entry_name, sql string) {
	_, err := db.Exec("INSERT INTO plan (step_nr, action, tbl_name, temp_name, entry_name, sql) VALUES (?, ?, ?, ?, ?, ?)", step_nr, action, tbl_name, temp_name, entry_name, sql)
	if err != nil {
		panic(err)
	}
}

func getSqlForTable(db *sql.DB, tbl_name string) (string, error) {
	tblSql := ""
	row := db.QueryRow("SELECT sql FROM sqlite_master WHERE type = 'table' AND tbl_name = ?", tbl_name)
	err := row.Scan(&tblSql)
	if err != nil {
		return "", err
	}
	return tblSql, err
}

func mkTempName() string {
	rn := uint32(0)
	binary.Read(rand.Reader, binary.LittleEndian, &rn)
	return fmt.Sprint("temp", rn)
}

func colDifference(whole, minusPart []*sql.ColumnType) (ret map[string]*sql.ColumnType, order []string) {
	ret = map[string]*sql.ColumnType{}
	for _, ec := range whole {
		ret[strings.ToLower(ec.Name())] = ec
	}
	order = []string{}
	for _, nc := range minusPart {
		nm := strings.ToLower(nc.Name())
		if _, ok := ret[nm]; ok {
			delete(ret, nm)
			continue
		}
	}
	for k := range ret {
		order = append(order, k)
	}
	sort.Strings(order)
	return ret, order
}
