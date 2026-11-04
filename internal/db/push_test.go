package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReminderAtUsesPreviousUTCCalendarDay(t *testing.T) {
	for _, test := range []struct{ name, due, now, want string }{
		{"afternoon", "2030-06-10T15:00:00Z", "2030-06-09T22:59:59Z", "2030-06-09T23:00:00Z"},
		{"midnight", "2030-06-10T00:00:00Z", "2030-06-09T22:00:00Z", "2030-06-09T23:00:00Z"},
		{"exact cutoff", "2030-06-10T23:59:00Z", "2030-06-09T23:00:00Z", "2030-06-09T23:00:00Z"},
		{"after cutoff", "2030-06-10T23:59:00Z", "2030-06-09T23:00:01Z", ""},
		{"year rollover", "2031-01-01T10:00:00Z", "2030-12-30T12:00:00Z", "2030-12-31T23:00:00Z"},
		{"leap day", "2032-03-01T10:00:00Z", "2032-02-28T12:00:00Z", "2032-02-29T23:00:00Z"},
		{"normalize UTC day", "2030-06-10T00:30:00+02:00", "2030-06-08T12:00:00Z", "2030-06-08T23:00:00Z"},
		{"ancient boundary", "0001-01-01T12:00:00Z", "0001-01-01T00:00:00Z", ""},
		{"no date", "", "2030-06-09T22:00:00Z", ""},
		{"invalid date", "tomorrow", "2030-06-09T22:00:00Z", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, test.now)
			if err != nil {
				t.Fatal(err)
			}
			if got := reminderAt(ListTodo, test.due, now); got != test.want {
				t.Fatalf("reminderAt = %q, want %q", got, test.want)
			}
			if got := reminderAt(ListShopping, test.due, now); got != "" {
				t.Fatalf("shopping reminder = %q", got)
			}
		})
	}
}

// Exercise the real SQLite statements without adding a database driver to the
// application's dependencies. These integration tests skip if sqlite3 is absent.
type sqliteCLIConnector struct{ path string }
type sqliteCLIConn struct {
	path string
	tx   *sqliteCLITx
}

func (c sqliteCLIConnector) Connect(context.Context) (driver.Conn, error) {
	return &sqliteCLIConn{path: c.path}, nil
}
func (sqliteCLIConnector) Driver() driver.Driver           { return schemaDriver{} }
func (*sqliteCLIConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (*sqliteCLIConn) Close() error                        { return nil }
func (c *sqliteCLIConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *sqliteCLIConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	c.tx = &sqliteCLITx{conn: c, ctx: ctx}
	return c.tx, nil
}

type sqliteCLITx struct {
	conn    *sqliteCLIConn
	ctx     context.Context
	queries []string
	args    []driver.NamedValue
}

func (tx *sqliteCLITx) Commit() error {
	tx.conn.tx = nil
	_, err := tx.conn.run(tx.ctx, "BEGIN;"+strings.Join(tx.queries, ";")+";COMMIT;", tx.args, false)
	return err
}
func (tx *sqliteCLITx) Rollback() error { tx.conn.tx = nil; return nil }

func (c *sqliteCLIConn) run(ctx context.Context, query string, args []driver.NamedValue, header bool) ([]byte, error) {
	parts := strings.Split(query, "?")
	if len(parts) != len(args)+1 {
		return nil, errors.New("wrong placeholder count")
	}
	var expanded strings.Builder
	expanded.WriteString(parts[0])
	for i, arg := range args {
		switch value := arg.Value.(type) {
		case string:
			expanded.WriteString("'" + strings.ReplaceAll(value, "'", "''") + "'")
		case int64:
			expanded.WriteString(strconv.FormatInt(value, 10))
		case bool:
			if value {
				expanded.WriteString("1")
			} else {
				expanded.WriteString("0")
			}
		default:
			return nil, fmt.Errorf("unsupported SQL argument %T", value)
		}
		expanded.WriteString(parts[i+1])
	}
	flags := []string{"-csv"}
	if header {
		flags = append(flags, "-header")
	}
	flags = append(flags, c.path, expanded.String())
	output, err := exec.CommandContext(ctx, "sqlite3", flags...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("sqlite3: %w: %s", err, output)
	}
	return output, nil
}

func (c *sqliteCLIConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.tx != nil {
		c.tx.queries = append(c.tx.queries, query)
		c.tx.args = append(c.tx.args, args...)
		return driver.RowsAffected(0), nil
	}
	output, err := c.run(ctx, query+"; SELECT changes();", args, false)
	if err != nil {
		return nil, err
	}
	count, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	return driver.RowsAffected(count), err
}

func (c *sqliteCLIConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	output, err := c.run(ctx, query, args, true)
	if err != nil {
		return nil, err
	}
	records, err := csv.NewReader(strings.NewReader(string(output))).ReadAll()
	if err != nil {
		return nil, err
	}
	rows := &sqliteCLIRows{}
	if len(records) > 0 {
		rows.columns, rows.values = records[0], records[1:]
	}
	return rows, nil
}

type sqliteCLIRows struct {
	columns []string
	values  [][]string
}

func (r *sqliteCLIRows) Columns() []string { return r.columns }
func (*sqliteCLIRows) Close() error        { return nil }
func (r *sqliteCLIRows) Next(values []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	for i, value := range r.values[0] {
		values[i] = value
	}
	r.values = r.values[1:]
	return nil
}

func realSQLite(t *testing.T) *sql.DB {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed")
	}
	conn := sql.OpenDB(sqliteCLIConnector{filepath.Join(t.TempDir(), "test.db")})
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func execSQL(t *testing.T, conn *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestPushStoreRealSQLite(t *testing.T) {
	ctx := context.Background()
	conn := realSQLite(t)
	if err := initializeSchema(ctx, conn); err != nil {
		t.Fatal(err)
	}
	store := NewStore(conn)
	now := time.Date(2030, 6, 9, 23, 0, 0, 0, time.UTC)
	const due = "2030-06-10T15:00:00Z"
	const cutoff = "2030-06-09T23:00:00Z"
	for _, row := range []struct {
		list, text, due, reminder string
		done                      bool
	}{
		{ListTodo, "eligible", due, cutoff, false},
		{ListTodo, "late", due, "", false},
		{ListTodo, "complete", due, cutoff, true},
		{ListShopping, "shopping", due, cutoff, false},
		{ListTodo, "undated", "", "", false},
		{ListTodo, "overdue", "2030-06-09T22:00:00Z", "2030-06-08T23:00:00Z", false},
	} {
		execSQL(t, conn, `INSERT INTO items(list_type, item_text, due_at, reminder_at, done) VALUES(?, ?, ?, ?, ?)`,
			row.list, row.text, row.due, row.reminder, row.done)
	}
	sub := PushSubscription{Endpoint: "https://fcm.googleapis.com/test", Keys: PushKeys{P256dh: "public", Auth: "auth"}}
	if err := store.SavePushSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	sub.Keys.Auth = "new-auth"
	if err := store.SavePushSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	subs, err := store.PushSubscriptions(ctx)
	if err != nil || len(subs) != 1 || subs[0].Keys.Auth != "new-auth" {
		t.Fatalf("subscriptions = %#v, %v", subs, err)
	}
	for _, test := range []struct {
		now   time.Time
		count int
	}{{now.Add(-time.Second), 0}, {now, 1}} {
		items, err := store.PendingReminders(ctx, test.now)
		if err != nil || len(items) != test.count {
			t.Fatalf("pending = %#v, %v; want %d", items, err, test.count)
		}
	}
	subID := subs[0].ID
	reminder := Reminder{ItemID: 1, DueAt: due}
	claim := func(at time.Time, want bool) {
		t.Helper()
		got, err := store.ClaimPushDelivery(ctx, reminder, subID, at)
		if err != nil || got != want {
			t.Fatalf("claim = %v, %v; want %v", got, err, want)
		}
	}
	claim(now.Add(-time.Second), false)
	claim(now, true)
	claim(now, false)
	claim(now.Add(3*time.Minute), true) // Recover an interrupted worker's lease.
	if err := store.FinishPushDelivery(ctx, reminder, subID, false); err != nil {
		t.Fatal(err)
	}
	claim(now, true) // A failed send is retryable.
	if err := store.FinishPushDelivery(ctx, reminder, subID, true); err != nil {
		t.Fatal(err)
	}
	claim(now.Add(5*time.Minute), false) // A successful send survives restarts.
	if items, err := store.PendingReminders(ctx, now); err != nil || len(items) != 0 {
		t.Fatalf("already delivered items still pending: %v, %v", items, err)
	}
	second := sub
	second.Endpoint += "-second-browser"
	if err := store.SavePushSubscription(ctx, second); err != nil {
		t.Fatal(err)
	}
	if items, err := store.PendingReminders(ctx, now); err != nil || len(items) != 1 {
		t.Fatalf("second browser has no pending reminder: %v, %v", items, err)
	}
	if err := store.DeletePushSubscription(ctx, second.Endpoint); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePushSubscription(ctx, sub.Endpoint); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM push_deliveries`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("subscription deletion left %d receipts: %v", count, err)
	}
	if err := store.SavePushSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	subs, _ = store.PushSubscriptions(ctx)
	subID = subs[0].ID
	execSQL(t, conn, `UPDATE items SET done = 1 WHERE id = 1`)
	claim(now, false)
	execSQL(t, conn, `UPDATE items SET done = 0, due_at = '2030-06-11T12:00:00Z' WHERE id = 1`)
	claim(now, false)
	if err := store.Delete(ctx, ListTodo, 1); err != nil {
		t.Fatal(err)
	}
	claim(now, false)
}

func TestItemSavesPreserveOrRescheduleReminder(t *testing.T) {
	ctx := context.Background()
	conn := realSQLite(t)
	if err := initializeSchema(ctx, conn); err != nil {
		t.Fatal(err)
	}
	store := NewStore(conn)
	future := time.Now().UTC().AddDate(0, 0, 3).Format(time.RFC3339)
	late := time.Now().UTC().Format(time.RFC3339)
	if err := store.Create(ctx, ListTodo, "don't forget", future); err != nil {
		t.Fatal(err)
	}
	getReminder := func() string {
		t.Helper()
		var value string
		if err := conn.QueryRowContext(ctx, `SELECT reminder_at FROM items WHERE id = 1`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if got := getReminder(); got != reminderAt(ListTodo, future, time.Now()) {
		t.Fatalf("new reminder = %q", got)
	}
	// Simulate editing only the text after an eligible item's cutoff.
	execSQL(t, conn, `UPDATE items SET due_at = ?, reminder_at = '2000-01-01T23:00:00Z' WHERE id = 1`, late)
	if err := store.Update(ctx, ListTodo, 1, "new text", late); err != nil {
		t.Fatal(err)
	}
	if got := getReminder(); got != "2000-01-01T23:00:00Z" {
		t.Fatalf("text edit erased reminder: %q", got)
	}
	if err := store.Update(ctx, ListTodo, 1, "future", future); err != nil {
		t.Fatal(err)
	}
	if got := getReminder(); got == "" {
		t.Fatal("future due date did not reschedule")
	}
	if err := store.Update(ctx, ListTodo, 1, "late", late); err != nil {
		t.Fatal(err)
	}
	if got := getReminder(); got != "" {
		t.Fatalf("late due date scheduled: %q", got)
	}
	if err := store.Create(ctx, ListTodo, "late new item", late); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := conn.QueryRowContext(ctx, `SELECT reminder_at FROM items WHERE id = 2`).Scan(&value); err != nil || value != "" {
		t.Fatalf("late new item's reminder = %q, %v", value, err)
	}
}

func TestRealSQLiteUpgradeBackfillsOnlyEligibleItems(t *testing.T) {
	conn := realSQLite(t)
	execSQL(t, conn, `CREATE TABLE items (
		id INTEGER PRIMARY KEY, list_type TEXT NOT NULL, item_text TEXT NOT NULL,
		done INTEGER NOT NULL DEFAULT 0, due_at TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL)`)
	for _, row := range []struct {
		id           int64
		created, due string
	}{
		{1, "2030-06-09 22:59:59", "2030-06-10T15:00:00Z"},
		{2, "2030-06-09 23:00:01", "2030-06-10T15:00:00Z"},
		{3, "2030-06-09 23:00:00", "2030-06-10T15:00:00Z"},
		{4, "2030-06-09 22:00:00", ""},
	} {
		execSQL(t, conn, `INSERT INTO items(id, list_type, item_text, due_at, created_at) VALUES(?, 'todo', 'preserved', ?, ?)`, row.id, row.due, row.created)
	}
	for range 2 {
		if err := initializeSchema(context.Background(), conn); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[int64]string{1: "2030-06-09T23:00:00Z", 2: "", 3: "2030-06-09T23:00:00Z", 4: ""} {
		var got string
		if err := conn.QueryRow(`SELECT reminder_at FROM items WHERE id = ?`, id).Scan(&got); err != nil || got != want {
			t.Fatalf("item %d reminder = %q, %v; want %q", id, got, err, want)
		}
	}
}
