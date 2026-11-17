package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestBuildStoreRealSQLite(t *testing.T) {
	ctx := context.Background()
	conn := realSQLite(t)
	if err := initializeSchema(ctx, conn); err != nil {
		t.Fatal(err)
	}
	store := NewStore(conn)
	if err := store.Create(ctx, ListShopping, "Hardware", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{StatusIdea, StatusPlanned, StatusBuilding, StatusFinished} {
		if !ValidBuildStatus(status) {
			t.Errorf("valid build status rejected: %s", status)
		}
		if err := store.Create(ctx, ListBuild, "Build "+status, "", status); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.List(ctx, ListBuild)
	if err != nil || len(items) != 4 {
		t.Fatalf("build list = %#v, %v", items, err)
	}
	for i, item := range items {
		if item.ID != int64(i+1) || item.ListType != ListBuild || item.Text != "Build "+item.Status || item.Done || item.DueAt != "" || item.CreatedAt == "" {
			t.Errorf("unexpected build item: %#v", item)
		}
		got, err := store.Get(ctx, ListBuild, item.ID)
		if err != nil || got != item {
			t.Errorf("get build = %#v, %v; want %#v", got, err, item)
		}
	}
	if err := store.Update(ctx, ListBuild, 1, "Workbench & tools", "", StatusBuilding); err != nil {
		t.Fatal(err)
	}
	item, err := store.Get(ctx, ListBuild, 1)
	if err != nil || item.Text != "Workbench & tools" || item.Status != StatusBuilding {
		t.Fatalf("updated build = %#v, %v", item, err)
	}
	if err := store.SetDone(ctx, ListBuild, 1, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("build checkbox mutation = %v, want not found", err)
	}
	if err := store.Delete(ctx, ListBuild, 1); err != nil {
		t.Fatal(err)
	}
	shopping, err := store.Get(ctx, ListShopping, 1)
	if err != nil || shopping.Text != "Hardware" || shopping.Status != "" || shopping.Done {
		t.Fatalf("build mutations affected shopping item with same ID: %#v, %v", shopping, err)
	}
	for operation, err := range map[string]error{
		"get deleted":    func() error { _, err := store.Get(ctx, ListBuild, 1); return err }(),
		"get wrong list": func() error { _, err := store.Get(ctx, ListTodo, 2); return err }(),
		"update missing": store.Update(ctx, ListBuild, 99, "Missing", "", StatusIdea),
		"delete missing": store.Delete(ctx, ListBuild, 99),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s error=%v, want not found", operation, err)
		}
	}
	for _, status := range []string{"", "done", "Idea", "<script>"} {
		if ValidBuildStatus(status) {
			t.Errorf("invalid build status accepted: %q", status)
		}
		if err := store.Create(ctx, ListBuild, "Invalid", "", status); err == nil {
			t.Errorf("database accepted status %q", status)
		}
		if err := store.Update(ctx, ListBuild, 2, "Invalid", "", status); err == nil {
			t.Errorf("database accepted status update %q", status)
		}
	}
	for _, text := range []string{"", strings.Repeat("x", MaxItemRunes+1)} {
		if err := store.Create(ctx, ListBuild, text, "", StatusIdea); err == nil {
			t.Error("database accepted invalid build name")
		}
	}
}

func TestBuildSchemaUpgradePreservesExistingItemsAndReminders(t *testing.T) {
	ctx := context.Background()
	conn := realSQLite(t)
	// Reproduce a database deployed before the Build tab existed.
	execSQL(t, conn, fmt.Sprintf(schema, MaxItemRunes))
	if err := initializePushSchema(ctx, conn); err != nil {
		t.Fatal(err)
	}
	execSQL(t, conn, `INSERT INTO items(id, list_type, item_text, done, due_at, reminder_at, created_at)
		VALUES(1, 'todo', 'Existing todo', 1, '2030-06-10T12:00:00Z', '2030-06-09T23:00:00Z', '2029-01-01 10:00:00')`)
	execSQL(t, conn, `INSERT INTO push_deliveries(item_id, due_at, subscription_id, claimed_until, sent)
		VALUES(1, '2030-06-10T12:00:00Z', 1, '2030-06-09T23:03:00Z', 1)`)
	store := NewStore(conn)
	before, err := store.Get(ctx, ListTodo, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := initializeSchema(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, ListBuild, "Existing build", "", StatusPlanned); err != nil {
		t.Fatal(err)
	}
	if err := initializeSchema(ctx, conn); err != nil {
		t.Fatal(err)
	}
	after, err := store.Get(ctx, ListTodo, 1)
	if err != nil || after != before {
		t.Fatalf("schema upgrade changed existing todo: %#v -> %#v, %v", before, after, err)
	}
	build, err := store.Get(ctx, ListBuild, 1)
	if err != nil || build.Status != StatusPlanned || build.Text != "Existing build" {
		t.Fatalf("repeated startup changed build: %#v, %v", build, err)
	}
	if err := store.Delete(ctx, ListBuild, 1); err != nil {
		t.Fatal(err)
	}
	var reminder string
	if err := conn.QueryRowContext(ctx, `SELECT reminder_at FROM items WHERE id = 1`).Scan(&reminder); err != nil || reminder != "2030-06-09T23:00:00Z" {
		t.Fatalf("schema upgrade changed reminder: %q, %v", reminder, err)
	}
	var sent int
	if err := conn.QueryRowContext(ctx, `SELECT sent FROM push_deliveries WHERE item_id = 1`).Scan(&sent); err != nil || sent != 1 {
		t.Fatalf("upgrade/build deletion changed delivery receipt: sent=%d, %v", sent, err)
	}
	if err := store.Delete(ctx, ListTodo, 1); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM push_deliveries`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("existing item cleanup trigger no longer works: count=%d, %v", count, err)
	}
}
