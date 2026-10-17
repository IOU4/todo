package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	turso "turso.tech/database/tursogo-serverless"
)

const (
	ListShopping = "shopping"
	ListTodo     = "todo"

	// MaxItemRunes is the maximum item length, measured in Unicode code points.
	MaxItemRunes = 255
)

var ErrNotFound = errors.New("item not found")

const schema = `
	CREATE TABLE IF NOT EXISTS items (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		list_type TEXT NOT NULL CHECK (list_type IN ('shopping', 'todo')),
		item_text TEXT NOT NULL CHECK (length(item_text) BETWEEN 1 AND %d),
		done INTEGER NOT NULL DEFAULT 0 CHECK (done IN (0, 1)),
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`

// Item is a shopping or todo list entry.
type Item struct {
	ID        int64
	ListType  string
	Text      string
	Done      bool
	CreatedAt string
}

// Store contains the database operations used by the HTTP handlers.
type Store struct {
	db *sql.DB
}

// DatabaseConfig is the credential-free endpoint and its separately parsed token.
type DatabaseConfig struct {
	URL       string
	AuthToken string
}

func ValidListName(name string) bool {
	return name == ListShopping || name == ListTodo
}

// ParseDatabaseURL accepts libsql://HOST?authToken=TOKEN. Query decoding is
// handled by net/url so reserved token characters must be URL-encoded.
func ParseDatabaseURL(raw string) (DatabaseConfig, error) {
	const invalidURL = "invalid DATABASE_URL: expected libsql://HOST?authToken=TOKEN"

	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return DatabaseConfig{}, errors.New(invalidURL)
	}
	if !strings.EqualFold(u.Scheme, "libsql") || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return DatabaseConfig{}, errors.New(invalidURL)
	}
	if u.Path != "" && u.Path != "/" {
		return DatabaseConfig{}, errors.New(invalidURL)
	}

	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return DatabaseConfig{}, errors.New(invalidURL)
	}

	camelToken, hasCamelToken := query["authToken"]
	snakeToken, hasSnakeToken := query["auth_token"]
	if hasCamelToken == hasSnakeToken { // neither spelling, or both spellings
		return DatabaseConfig{}, errors.New(invalidURL)
	}
	var tokenValues []string
	if hasCamelToken {
		tokenValues = camelToken
	} else {
		tokenValues = snakeToken
	}
	if len(tokenValues) != 1 || strings.TrimSpace(tokenValues[0]) == "" {
		return DatabaseConfig{}, errors.New(invalidURL)
	}
	for key := range query {
		if key != "authToken" && key != "auth_token" {
			return DatabaseConfig{}, errors.New(invalidURL)
		}
	}

	u.Scheme = "libsql"
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.ForceQuery = false
	return DatabaseConfig{URL: u.String(), AuthToken: tokenValues[0]}, nil
}

// Connect opens the pure-Go remote Turso driver, verifies connectivity, and
// creates the schema. Error messages intentionally omit credential-bearing URLs
// and driver error text.
func Connect(ctx context.Context, databaseURL string) (*sql.DB, error) {
	config, err := ParseDatabaseURL(databaseURL)
	if err != nil {
		return nil, err
	}

	db := sql.OpenDB(turso.NewConnector(config.URL, config.AuthToken))
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(10 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancelPing := context.WithTimeout(ctx, 10*time.Second)
	err = db.PingContext(pingCtx)
	cancelPing()
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("database connectivity check failed (%T)", err)
	}

	schemaCtx, cancelSchema := context.WithTimeout(ctx, 10*time.Second)
	_, err = db.ExecContext(schemaCtx, fmt.Sprintf(schema, MaxItemRunes))
	cancelSchema()
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("database schema initialization failed (%T)", err)
	}

	return db, nil
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// List returns items in stable creation/id order.
func (s *Store) List(ctx context.Context, listType string) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, list_type, item_text, done, created_at
		 FROM items WHERE list_type = ? ORDER BY id ASC`,
		listType,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Item, 0)
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.ID, &item.ListType, &item.Text, &item.Done, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) Get(ctx context.Context, listType string, id int64) (Item, error) {
	var item Item
	err := s.db.QueryRowContext(ctx,
		`SELECT id, list_type, item_text, done, created_at
		 FROM items WHERE list_type = ? AND id = ?`,
		listType, id,
	).Scan(&item.ID, &item.ListType, &item.Text, &item.Done, &item.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, err
	}
	return item, nil
}

func (s *Store) Create(ctx context.Context, listType, text string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO items (list_type, item_text, done) VALUES (?, ?, 0)`,
		listType, text,
	)
	return err
}

func (s *Store) UpdateText(ctx context.Context, listType string, id int64, text string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE items SET item_text = ? WHERE list_type = ? AND id = ?`,
		text, listType, id,
	)
	if err != nil {
		return err
	}
	return requireAffectedRow(result)
}

func (s *Store) SetDone(ctx context.Context, listType string, id int64, done bool) error {
	value := 0
	if done {
		value = 1
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE items SET done = ? WHERE list_type = ? AND id = ?`,
		value, listType, id,
	)
	if err != nil {
		return err
	}
	return requireAffectedRow(result)
}

func (s *Store) Delete(ctx context.Context, listType string, id int64) error {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM items WHERE list_type = ? AND id = ?`,
		listType, id,
	)
	if err != nil {
		return err
	}
	return requireAffectedRow(result)
}

func requireAffectedRow(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

// ValidItemText validates and trims an item exactly once before persistence.
func ValidItemText(raw string) (string, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", errors.New("item text must not be blank")
	}
	if !utf8.ValidString(text) {
		return "", errors.New("item text must be valid UTF-8")
	}
	if utf8.RuneCountInString(text) > MaxItemRunes {
		return "", errors.New("item text exceeds the character limit")
	}
	return text, nil
}
