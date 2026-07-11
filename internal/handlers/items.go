package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	tododb "todo/internal/db"
)

// Items wires DB dependency into handlers.
type Items struct {
	DB *sql.DB
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// List handles GET /api/items?list=todo|shopping
func (h *Items) List(w http.ResponseWriter, r *http.Request) {
	listType := r.URL.Query().Get("list")
	if listType != "todo" && listType != "shopping" {
		writeError(w, http.StatusBadRequest, "list must be 'todo' or 'shopping'")
		return
	}

	items, err := tododb.ListItems(h.DB, listType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, items)
}

// Add handles POST /api/items
func (h *Items) Add(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ListType string `json:"list_type"`
		Text     string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}

	body.Text = strings.TrimSpace(body.Text)
	if body.ListType != "todo" && body.ListType != "shopping" {
		writeError(w, http.StatusBadRequest, "list_type must be 'todo' or 'shopping'")
		return
	}
	if body.Text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}

	id, err := tododb.AddItem(h.DB, body.ListType, body.Text)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// Toggle handles POST /api/items/{id}/toggle
func (h *Items) Toggle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid item id")
		return
	}

	if err := tododb.ToggleItem(h.DB, id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Delete handles DELETE /api/items/{id}
func (h *Items) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid item id")
		return
	}

	if err := tododb.DeleteItem(h.DB, id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
