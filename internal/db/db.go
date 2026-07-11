package db

import (
	"database/sql"
	"fmt"

	_ "github.com/go-sql-driver/mysql"
)

// Item represents a single todo/shopping list entry.
type Item struct {
	ID        int64  `json:"id"`
	ListType  string `json:"list_type"`
	Text      string `json:"text"`
	Done      bool   `json:"done"`
	CreatedAt string `json:"created_at"`
}

// Connect opens a MySQL connection pool and verifies it, and ensures schema exists.
func Connect(dsn string) (*sql.DB, error) {
	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	if err := ensureSchema(conn); err != nil {
		return nil, fmt.Errorf("ensure schema: %w", err)
	}

	return conn, nil
}

func ensureSchema(conn *sql.DB) error {
	_, err := conn.Exec(`
		CREATE TABLE IF NOT EXISTS items (
			id INT AUTO_INCREMENT PRIMARY KEY,
			list_type ENUM('todo','shopping') NOT NULL,
			text VARCHAR(255) NOT NULL,
			done BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)
	`)
	return err
}

// ListItems returns all items for a given list_type ordered by creation.
func ListItems(conn *sql.DB, listType string) ([]Item, error) {
	rows, err := conn.Query(
		`SELECT id, list_type, text, done, created_at FROM items WHERE list_type = ? ORDER BY id ASC`,
		listType,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []Item{}
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.ListType, &it.Text, &it.Done, &it.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// AddItem inserts a new item and returns its generated ID.
func AddItem(conn *sql.DB, listType, text string) (int64, error) {
	res, err := conn.Exec(
		`INSERT INTO items (list_type, text, done) VALUES (?, ?, FALSE)`,
		listType, text,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ToggleItem flips the done flag for an item.
func ToggleItem(conn *sql.DB, id int64) error {
	_, err := conn.Exec(`UPDATE items SET done = NOT done WHERE id = ?`, id)
	return err
}

// DeleteItem removes an item by id.
func DeleteItem(conn *sql.DB, id int64) error {
	_, err := conn.Exec(`DELETE FROM items WHERE id = ?`, id)
	return err
}
