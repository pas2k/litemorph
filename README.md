# litemorph

Declarative schema migrations for SQLite.

Instead of writing versioned up/down migration scripts, you **declare the schema
you want** as a set of `CREATE` statements. litemorph loads that target schema
into an in-memory SQLite database, compares it against the live database's
`sqlite_master`, and builds and executes a plan that morphs the live schema into
the declared one — following SQLite's recommended
[create-temp → copy → drop → rename](https://www.sqlite.org/lang_altertable.html#otheralter)
procedure for table changes, all inside a single transaction.

litemorph never parses SQL itself; it lets SQLite do the parsing by loading
schemas into in-memory databases and inspecting the result. It is therefore
SQLite-specific by design and depends on
[`github.com/mattn/go-sqlite3`](https://github.com/mattn/go-sqlite3).

## Install

```sh
go get github.com/pas2k/litemorph
```

## Usage

```go
db, _ := sql.Open("sqlite3", "app.db")

err := litemorph.NewMigrator(db).Migrate([]string{
    `CREATE TABLE users(id INTEGER PRIMARY KEY, name TEXT, email TEXT)`,
    `CREATE INDEX users_email ON users(email)`,
})
```

Tables that already match the declared schema are left untouched. Tables that
differ are rebuilt, preserving the data of columns common to both schemas.

### Decision callback

Observe what litemorph decides for each table:

```go
m := litemorph.NewMigrator(db, litemorph.WithDecisionCallback(
    func(_ *litemorph.Migrator, d litemorph.Decision, table string) {
        log.Printf("%s: %v", table, d)
    }))
```

### Quirks

Inspect or rewrite the generated plan before it runs. The plan lives in an
in-memory table `plan(step_nr, action, tbl_name, temp_name, entry_name, sql)`:

```go
m := litemorph.NewMigrator(db, litemorph.WithQuirks([]string{
    `DELETE FROM plan WHERE action = 'copy_residue' AND entry_name = 'legacy_col'`,
}))
```

Or use `WithQuirksFunc` for arbitrary Go logic against the plan database.

## Behavior notes

- **Residue columns are kept, by design — migrating never loses data.** A column
  in the live table but omitted from the declared schema is retained with its
  data, not dropped.
- **litemorph does not manage indexes.** Explicit (`CREATE INDEX`) indexes are
  dropped when a table is rebuilt and are not recreated. Run
  `CREATE INDEX IF NOT EXISTS ...` yourself after migrating. (Implicit
  `sqlite_autoindex_*` indexes for `PRIMARY KEY`/`UNIQUE` are kept automatically.)
