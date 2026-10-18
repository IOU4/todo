package handlers

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	tododb "github.com/IOU4/todo/internal/db"
)

const (
	dueInputLayout    = "2006-01-02T15:04"
	invalidDueMessage = "Enter a valid due date and time, or leave it blank."
)

// The browser supplies the selected date's UTC offset (including DST).
// Without JavaScript, the form explicitly uses UTC.
func parseDueDate(r *http.Request, listName string) (string, error) {
	raw := r.PostForm.Get("due_at")
	if listName != tododb.ListTodo || raw == "" {
		return "", nil
	}
	offset := 0
	if value := r.PostForm.Get("due_offset"); value != "" {
		var err error
		offset, err = strconv.Atoi(value)
		if err != nil || offset < -14*60 || offset > 14*60 {
			return "", errors.New(invalidDueMessage)
		}
	}
	zone := time.FixedZone("", -offset*60) // JS getTimezoneOffset is UTC minus local.
	due, err := time.ParseInLocation(dueInputLayout, raw, zone)
	if err != nil || due.Year() < 1 || due.UTC().Year() < 1 || due.UTC().Year() > 9999 {
		return "", errors.New(invalidDueMessage)
	}
	return due.UTC().Format(time.RFC3339), nil
}

func remainingTime(raw string, now time.Time) string {
	due, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return ""
	}
	if !due.After(now) {
		return "Overdue"
	}
	// Unix seconds avoid time.Duration's ~290-year limit for distant dates.
	minutes := int64(math.Ceil((float64(due.Unix()-now.Unix()) - float64(now.Nanosecond())/1e9) / 60))
	return fmt.Sprintf("%dd %dh %dm", minutes/1440, minutes%1440/60, minutes%60)
}
