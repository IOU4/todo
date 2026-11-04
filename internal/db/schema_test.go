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
	existing, hasDueAt, hasReminderAt bool
	alters, reminderAlters, backfills int
	fail                              string
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
func (c schemaConn) Begin() (driver.Tx, error) {
	return &schemaTx{state: c.state, before: *c.state}, nil
}

type schemaTx struct {
	state  *schemaDB
	before schemaDB
}

func (*schemaTx) Commit() error      { return nil }
func (tx *schemaTx) Rollback() error { *tx.state = tx.before; return nil }
func (c schemaConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	switch {
	case strings.Contains(query, "CREATE TABLE IF NOT EXISTS items"):
		if c.state.fail == "create" {
			return nil, errors.New("create failed")
		}
		if !c.state.existing {
			c.state.existing = true
			c.state.hasDueAt = strings.Contains(query, "due_at TEXT NOT NULL DEFAULT ''")
			c.state.hasReminderAt = strings.Contains(query, "reminder_at TEXT NOT NULL DEFAULT ''")
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
	case query == "ALTER TABLE items ADD COLUMN reminder_at TEXT NOT NULL DEFAULT ''":
		if c.state.fail == "reminder" || c.state.hasReminderAt {
			return nil, errors.New("reminder column failed")
		}
		c.state.hasReminderAt = true
		c.state.reminderAlters++
	case strings.Contains(query, "SET reminder_at = strftime"):
		if c.state.fail == "backfill" {
			return nil, errors.New("backfill failed")
		}
		c.state.backfills++
	case strings.Contains(query, "CREATE TABLE"), strings.Contains(query, "CREATE INDEX"), strings.Contains(query, "CREATE TRIGGER"):
		if c.state.fail == "push" {
			return nil, errors.New("push schema failed")
		}
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
	if c.state.hasReminderAt {
		names = append(names, "reminder_at")
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
		name             string
		state            schemaDB
		wantAdds         int
		wantReminderAdds int
	}{
		{"new database", schemaDB{}, 0, 0},
		{"existing database", schemaDB{existing: true}, 1, 1},
		{"due date schema", schemaDB{existing: true, hasDueAt: true}, 0, 1},
		{"already upgraded", schemaDB{existing: true, hasDueAt: true, hasReminderAt: true}, 0, 0},
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
			if !test.state.hasDueAt || test.state.alters != test.wantAdds ||
				!test.state.hasReminderAt || test.state.reminderAlters != test.wantReminderAdds || test.state.backfills != test.wantReminderAdds {
				t.Fatalf("schema state = %#v, want %d due_at and %d reminder_at ALTERs/backfills", test.state, test.wantAdds, test.wantReminderAdds)
			}
		})
	}
}

func TestFailedReminderBackfillCanBeRetried(t *testing.T) {
	state := &schemaDB{existing: true, hasDueAt: true, fail: "backfill"}
	conn := sql.OpenDB(schemaConnector{state})
	defer conn.Close()
	if err := initializeSchema(context.Background(), conn); err == nil {
		t.Fatal("backfill failure was ignored")
	}
	if state.hasReminderAt {
		t.Fatal("failed migration left the reminder column without its backfill")
	}
	state.fail = ""
	if err := initializeSchema(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	if !state.hasReminderAt || state.backfills != 1 {
		t.Fatalf("retry did not complete backfill: %#v", state)
	}
}

func TestInitializeSchemaReturnsMigrationErrors(t *testing.T) {
	for _, operation := range []string{"create", "query", "alter", "reminder", "backfill", "push"} {
		t.Run(operation, func(t *testing.T) {
			db := sql.OpenDB(schemaConnector{&schemaDB{existing: true, fail: operation}})
			defer db.Close()
			if err := initializeSchema(context.Background(), db); err == nil {
				t.Fatal("schema initialization ignored a database error")
			}
		})
	}
}
