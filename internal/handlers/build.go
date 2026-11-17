package handlers

import (
	"errors"
	"net/http"

	tododb "github.com/IOU4/todo/internal/db"
)

const invalidStatusMessage = "Choose a valid build status."

type buildStatusOption struct {
	Value string
	Label string
}

var buildStatuses = []buildStatusOption{
	{tododb.StatusIdea, "Idea"},
	{tododb.StatusPlanned, "Planned"},
	{tododb.StatusBuilding, "Building"},
	{tododb.StatusFinished, "Finished"},
}

func buildStatusLabel(status string) string {
	for _, option := range buildStatuses {
		if option.Value == status {
			return option.Label
		}
	}
	return ""
}

func parseBuildStatus(r *http.Request, listName string) (string, error) {
	if listName != tododb.ListBuild {
		return "", nil
	}
	status := r.PostForm.Get("status")
	if status == "" {
		status = tododb.StatusIdea
	}
	if !tododb.ValidBuildStatus(status) {
		return "", errors.New(invalidStatusMessage)
	}
	return status, nil
}
