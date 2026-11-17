package db

const (
	StatusIdea     = "idea"
	StatusPlanned  = "planned"
	StatusBuilding = "building"
	StatusFinished = "finished"
)

const buildSchema = `
	CREATE TABLE IF NOT EXISTS builds (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		item_text TEXT NOT NULL CHECK (length(item_text) BETWEEN 1 AND %d),
		status TEXT NOT NULL DEFAULT 'idea' CHECK (status IN ('idea', 'planned', 'building', 'finished')),
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`

func ValidBuildStatus(status string) bool {
	switch status {
	case StatusIdea, StatusPlanned, StatusBuilding, StatusFinished:
		return true
	default:
		return false
	}
}
