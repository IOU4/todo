package db

import (
	"context"
	"database/sql"
	"time"
)

// PushSubscription contains the browser's opaque delivery endpoint and keys.
// These values are credentials: never include them in logs or HTML.
type PushSubscription struct {
	ID       int64    `json:"-"`
	Endpoint string   `json:"endpoint"`
	Keys     PushKeys `json:"keys"`
}

type PushKeys struct {
	P256dh string `json:"p256dh"`
	Auth   string `json:"auth"`
}

type Reminder struct {
	ItemID int64
	Text   string
	DueAt  string
}

// reminderAt uses the due date's UTC calendar day, not the browser's day or
// a rolling 24-hour interval. A due date first set after its cutoff is skipped.
func reminderAt(listType, raw string, now time.Time) string {
	if listType != ListTodo {
		return ""
	}
	due, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return ""
	}
	due = due.UTC()
	cutoff := time.Date(due.Year(), due.Month(), due.Day(), 23, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	if cutoff.Year() < 1 || now.After(cutoff) {
		return ""
	}
	return cutoff.Format(time.RFC3339)
}

func initializePushSchema(ctx context.Context, conn *sql.DB) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS push_subscriptions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			endpoint TEXT NOT NULL UNIQUE,
			p256dh TEXT NOT NULL,
			auth TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS push_deliveries (
			item_id INTEGER NOT NULL,
			due_at TEXT NOT NULL,
			subscription_id INTEGER NOT NULL,
			claimed_until TEXT NOT NULL,
			sent INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (item_id, due_at, subscription_id)
		)`,
		`CREATE INDEX IF NOT EXISTS items_reminder_at ON items (reminder_at)
			WHERE list_type = 'todo' AND done = 0 AND reminder_at != ''`,
		`CREATE TRIGGER IF NOT EXISTS items_delete_push_deliveries AFTER DELETE ON items
			BEGIN DELETE FROM push_deliveries WHERE item_id = OLD.id; END`,
		`CREATE TRIGGER IF NOT EXISTS subscriptions_delete_push_deliveries AFTER DELETE ON push_subscriptions
			BEGIN DELETE FROM push_deliveries WHERE subscription_id = OLD.id; END`,
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SavePushSubscription(ctx context.Context, sub PushSubscription) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO push_subscriptions (endpoint, p256dh, auth) VALUES (?, ?, ?)
		ON CONFLICT(endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth`,
		sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth)
	return err
}

func (s *Store) DeletePushSubscription(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint)
	return err
}

func (s *Store) PushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, endpoint, p256dh, auth FROM push_subscriptions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []PushSubscription
	for rows.Next() {
		var sub PushSubscription
		if err := rows.Scan(&sub.ID, &sub.Endpoint, &sub.Keys.P256dh, &sub.Keys.Auth); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

func (s *Store) PendingReminders(ctx context.Context, now time.Time) ([]Reminder, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, item_text, due_at FROM items
		WHERE list_type = 'todo' AND done = 0 AND reminder_at != '' AND reminder_at <= ? AND due_at > ?
		AND EXISTS (
			SELECT 1 FROM push_subscriptions AS subscription
			LEFT JOIN push_deliveries AS delivery ON delivery.subscription_id = subscription.id
				AND delivery.item_id = items.id AND delivery.due_at = items.due_at
			WHERE delivery.item_id IS NULL OR (delivery.sent = 0 AND delivery.claimed_until < ?)
		)
		ORDER BY id`, now.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reminders []Reminder
	for rows.Next() {
		var reminder Reminder
		if err := rows.Scan(&reminder.ItemID, &reminder.Text, &reminder.DueAt); err != nil {
			return nil, err
		}
		reminders = append(reminders, reminder)
	}
	return reminders, rows.Err()
}

// ClaimPushDelivery rechecks the item and takes a short lease atomically. This
// avoids sends for deleted/completed/rescheduled items and duplicate sends from
// concurrent workers. A crash before sending is retried after the lease expires.
func (s *Store) ClaimPushDelivery(ctx context.Context, reminder Reminder, subID int64, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO push_deliveries (item_id, due_at, subscription_id, claimed_until)
		SELECT id, due_at, ?, ? FROM items
		WHERE id = ? AND due_at = ? AND list_type = 'todo' AND done = 0
		AND reminder_at != '' AND reminder_at <= ? AND due_at > ?
		AND EXISTS (SELECT 1 FROM push_subscriptions WHERE id = ?)
		ON CONFLICT(item_id, due_at, subscription_id) DO UPDATE SET claimed_until = excluded.claimed_until
		WHERE push_deliveries.sent = 0 AND push_deliveries.claimed_until < ?`,
		subID, now.Add(2*time.Minute).UTC().Format(time.RFC3339), reminder.ItemID, reminder.DueAt,
		now.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339), subID, now.UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

func (s *Store) FinishPushDelivery(ctx context.Context, reminder Reminder, subID int64, sent bool) error {
	query := `DELETE FROM push_deliveries WHERE item_id = ? AND due_at = ? AND subscription_id = ? AND sent = 0`
	if sent {
		query = `UPDATE push_deliveries SET sent = 1 WHERE item_id = ? AND due_at = ? AND subscription_id = ?`
	}
	_, err := s.db.ExecContext(ctx, query, reminder.ItemID, reminder.DueAt, subID)
	return err
}
