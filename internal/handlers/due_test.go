package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	tododb "github.com/IOU4/todo/internal/db"
)

func TestParseDueDate(t *testing.T) {
	for _, test := range []struct {
		name, list, raw, offset, want string
		invalid                       bool
	}{
		{name: "optional", list: "todo"},
		{name: "UTC fallback", list: "todo", raw: "2030-06-10T14:30", want: "2030-06-10T14:30:00Z"},
		{name: "east of UTC", list: "todo", raw: "2030-06-10T14:30", offset: "-180", want: "2030-06-10T11:30:00Z"},
		{name: "west of UTC", list: "todo", raw: "2030-06-10T14:30", offset: "240", want: "2030-06-10T18:30:00Z"},
		{name: "fractional offset", list: "todo", raw: "2030-06-10T14:30", offset: "-345", want: "2030-06-10T08:45:00Z"},
		{name: "shopping ignores dates", list: "shopping", raw: "invalid", offset: "invalid"},
		{name: "bad date", list: "todo", raw: "2030-02-30T14:30", invalid: true},
		{name: "missing time", list: "todo", raw: "2030-06-10", invalid: true},
		{name: "bad offset", list: "todo", raw: "2030-06-10T14:30", offset: "NaN", invalid: true},
		{name: "large offset", list: "todo", raw: "2030-06-10T14:30", offset: "841", invalid: true},
		{name: "year zero", list: "todo", raw: "0000-06-10T14:30", invalid: true},
		{name: "UTC year overflow", list: "todo", raw: "9999-12-31T23:30", offset: "-60", want: "9999-12-31T22:30:00Z"},
		{name: "UTC year out of range", list: "todo", raw: "9999-12-31T23:30", offset: "60", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := formRequest(http.MethodPost, "/lists/"+test.list+"/items", url.Values{
				"due_at": {test.raw}, "due_offset": {test.offset},
			}, false)
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			got, err := parseDueDate(r, test.list)
			if (err != nil) != test.invalid || got != test.want {
				t.Fatalf("parseDueDate = %q, %v; want %q, invalid=%v", got, err, test.want, test.invalid)
			}
		})
	}
}

func TestRemainingTime(t *testing.T) {
	now := time.Date(2030, 6, 10, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		delta time.Duration
		want  string
	}{
		{(5*24+2)*time.Hour + 38*time.Minute, "5d 2h 38m"},
		{time.Hour + 2*time.Minute, "0d 1h 2m"},
		{time.Second, "0d 0h 1m"},
		{time.Minute + time.Second, "0d 0h 2m"},
		{0, "Overdue"},
		{-time.Hour, "Overdue"},
	} {
		got := remainingTime(now.Add(test.delta).Format(time.RFC3339), now)
		if got != test.want {
			t.Errorf("remainingTime(%v) = %q, want %q", test.delta, got, test.want)
		}
	}
	if got := remainingTime("not a date", now); got != "" {
		t.Errorf("invalid date countdown = %q", got)
	}
}

func TestTodoDueDateLifecycle(t *testing.T) {
	store := newMemoryStore()
	handler, _ := newTestHandler(t, store)

	response := perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/todo", nil))
	if !strings.Contains(response.Body.String(), `type="datetime-local"`) {
		t.Fatal("todo page is missing its due date input")
	}
	response = perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/shopping", nil))
	if strings.Contains(response.Body.String(), `name="due_at"`) {
		t.Fatal("shopping unexpectedly has a due date input")
	}

	values := url.Values{"text": {"Finish project"}, "due_at": {"2099-06-10T14:30"}, "due_offset": {"-180"}}
	response = perform(handler, formRequest(http.MethodPost, "/lists/todo/items", values, true))
	item, _ := store.item(1)
	if response.Code != http.StatusOK || item.DueAt != "2099-06-10T11:30:00Z" || !strings.Contains(response.Body.String(), `datetime="2099-06-10T11:30:00Z"`) {
		t.Fatalf("create with due date failed: status=%d item=%#v body=%s", response.Code, item, response.Body.String())
	}

	response = perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/todo/items/1/edit", nil))
	if !strings.Contains(response.Body.String(), `data-due-at="2099-06-10T11:30:00Z"`) || !strings.Contains(response.Body.String(), `value="2099-06-10T11:30"`) {
		t.Fatalf("edit didn't load the saved date: %s", response.Body.String())
	}

	values.Set("due_at", "2000-01-01T12:00")
	response = perform(handler, formRequest(http.MethodPost, "/lists/todo/items/1/edit", values, true))
	item, _ = store.item(1)
	if response.Code != http.StatusOK || item.DueAt != "2000-01-01T09:00:00Z" || !strings.Contains(response.Body.String(), ">Overdue</time>") {
		t.Fatalf("update with past date failed: status=%d item=%#v body=%s", response.Code, item, response.Body.String())
	}

	response = perform(handler, formRequest(http.MethodPost, "/lists/todo/items/1/toggle", url.Values{"done": {"true"}}, true))
	item, _ = store.item(1)
	if item.DueAt == "" || strings.Contains(response.Body.String(), "data-countdown") {
		t.Fatal("completion should retain the date but hide the countdown")
	}

	values.Set("due_at", "")
	response = perform(handler, formRequest(http.MethodPost, "/lists/todo/items/1/edit", values, true))
	item, _ = store.item(1)
	if response.Code != http.StatusOK || item.DueAt != "" {
		t.Fatalf("clearing due date failed: status=%d item=%#v", response.Code, item)
	}

	response = perform(handler, formRequest(http.MethodPost, "/lists/todo/items", url.Values{"text": {"No deadline"}}, false))
	item, _ = store.item(2)
	if response.Code != http.StatusSeeOther || item.DueAt != "" {
		t.Fatal("todo creation without a due date should still work")
	}
}

func TestDueDateFailuresPreserveDrafts(t *testing.T) {
	store := newMemoryStore()
	handler, _ := newTestHandler(t, store)
	values := url.Values{"text": {"Keep me"}, "due_at": {"2030-06-10T14:30"}, "due_offset": {"invalid"}}
	response := perform(handler, formRequest(http.MethodPost, "/lists/todo/items", values, true))
	body := response.Body.String()
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(body, invalidDueMessage) || !strings.Contains(body, `value="2030-06-10T14:30"`) || !strings.Contains(body, `value="Keep me"`) || !strings.Contains(body, `aria-invalid="true"`) {
		t.Fatalf("invalid date was not rejected with draft preserved: status=%d body=%s", response.Code, body)
	}
	if _, ok := store.item(1); ok {
		t.Fatal("invalid due date created an item")
	}

	values.Set("due_offset", "-180")
	store.fail["create"] = errors.New("unavailable")
	response = perform(handler, formRequest(http.MethodPost, "/lists/todo/items", values, true))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `value="2030-06-10T14:30"`) {
		t.Fatal("database failure discarded the due date draft")
	}
	delete(store.fail, "create")
	perform(handler, formRequest(http.MethodPost, "/lists/todo/items", values, true))

	values.Set("text", " ")
	response = perform(handler, formRequest(http.MethodPost, "/lists/todo/items/1/edit", values, true))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), `value="2030-06-10T14:30"`) {
		t.Fatal("text validation failure discarded the due date draft")
	}

	values.Set("text", "Keep me")
	values.Set("due_at", "2030-02-30T14:30")
	response = perform(handler, formRequest(http.MethodPost, "/lists/todo/items/1/edit", values, true))
	item, _ := store.item(1)
	if response.Code != http.StatusUnprocessableEntity || item.DueAt != "2030-06-10T11:30:00Z" {
		t.Fatal("invalid update changed the saved due date")
	}

	response = perform(handler, formRequest(http.MethodPost, "/lists/shopping/items", values, true))
	item, _ = store.item(2)
	if response.Code != http.StatusOK || item.ListType != tododb.ListShopping || item.DueAt != "" {
		t.Fatal("shopping should ignore due dates")
	}
}
