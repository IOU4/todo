package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
)

// A minimal SQL driver exercises the startup migration without remote credentials.
type schemaDB struct {
	existing, hasDueAt bool
	alters             int
	fail               string
}

type schemaConnector struct{ state *schemaDB }

func (c schemaConnector) Connect(context.Context) (driver.Conn, error) {
	return schemaConn{c.state}, nil
}
func (c schemaConnector) Driver() driver.Driver { return schemaDriver{} }

type schemaDriver struct{}

func (schemaDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type schemaConn struct{ state *schemaDB }

func (schemaConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (schemaConn) Close() error                        { return nil }
func (schemaConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }
func (c schemaConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	switch {
	case strings.Contains(query, "CREATE TABLE"):
		if c.state.fail == "create" {
			return nil, errors.New("create failed")
		}
		if !c.state.existing {
			c.state.existing = true
			c.state.hasDueAt = strings.Contains(query, "due_at TEXT NOT NULL DEFAULT ''")
		}
	case query == "ALTER TABLE items ADD COLUMN due_at TEXT NOT NULL DEFAULT ''":
		if c.state.fail == "alter" {
			return nil, errors.New("alter failed")
		}
		if c.state.hasDueAt {
			return nil, errors.New("duplicate column")
		}
		c.state.hasDueAt = true
		c.state.alters++
	default:
		return nil, errors.New("unexpected SQL")
	}
	return driver.RowsAffected(0), nil
}
func (c schemaConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if query != "PRAGMA table_info(items)" || c.state.fail == "query" {
		return nil, errors.New("query failed")
	}
	names := []string{"id", "list_type", "item_text", "done", "created_at"}
	if c.state.hasDueAt {
		names = append(names, "due_at")
	}
	return &schemaRows{names: names}, nil
}

type schemaRows struct {
	names []string
	index int
}

func (*schemaRows) Columns() []string {
	return []string{"cid", "name", "type", "notnull", "dflt_value", "pk"}
}
func (*schemaRows) Close() error { return nil }
func (r *schemaRows) Next(values []driver.Value) error {
	if r.index == len(r.names) {
		return io.EOF
	}
	copy(values, []driver.Value{int64(r.index), r.names[r.index], "TEXT", int64(1), nil, int64(0)})
	r.index++
	return nil
}

func TestInitializeSchemaAddsDueDateOnlyWhenMissing(t *testing.T) {
	for _, test := range []struct {
		name     string
		state    schemaDB
		wantAdds int
	}{
		{"new database", schemaDB{}, 0},
		{"existing database", schemaDB{existing: true}, 1},
		{"already upgraded", schemaDB{existing: true, hasDueAt: true}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := sql.OpenDB(schemaConnector{&test.state})
			db.SetMaxOpenConns(1)
			defer db.Close()
			// Startup must remain idempotent after an upgrade.
			for range 2 {
				if err := initializeSchema(context.Background(), db); err != nil {
					t.Fatal(err)
				}
			}
			if !test.state.hasDueAt || test.state.alters != test.wantAdds {
				t.Fatalf("schema state = %#v, want due_at and %d ALTERs", test.state, test.wantAdds)
			}
		})
	}
}

func TestInitializeSchemaReturnsMigrationErrors(t *testing.T) {
	for _, operation := range []string{"create", "query", "alter"} {
		t.Run(operation, func(t *testing.T) {
			db := sql.OpenDB(schemaConnector{&schemaDB{existing: true, fail: operation}})
			defer db.Close()
			if err := initializeSchema(context.Background(), db); err == nil {
				t.Fatal("schema initialization ignored a database error")
			}
		})
	}
}
