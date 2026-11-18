package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	tododb "github.com/IOU4/todo/internal/db"
)

func TestBuildLifecycle(t *testing.T) {
	store := newMemoryStore()
	handler, _ := newTestHandler(t, store)
	response := performPage(t, handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/build", nil))
	body := response.Body.String()
	for _, want := range []string{"Build — Lists", `href="/lists/build" aria-current="page"`, "Nothing here yet.", `id="new-status"`} {
		if response.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("build page missing %q: status=%d body=%s", want, response.Code, body)
		}
	}
	for _, option := range buildStatuses {
		if !strings.Contains(body, `value="`+option.Value+`"`) {
			t.Errorf("missing status option %s", option.Value)
		}
	}

	response = perform(handler, formRequest(http.MethodPost, "/lists/build/items", url.Values{
		"text": {"  Bookcase <oak>  "}, "due_at": {"invalid"}, "done": {"true"},
	}))
	response = followRedirect(t, handler, response, "/lists/build")
	item, ok := store.item(1)
	if !ok ||
		item.Text != "Bookcase <oak>" || item.Status != tododb.StatusIdea || item.Done || item.DueAt != "" {
		t.Fatalf("create build: status=%d item=%#v", response.Code, item)
	}

	body = response.Body.String()
	for _, want := range []string{"Bookcase &lt;oak&gt;", `class="status-badge status-idea"`, `aria-label="Status: Idea"`, ">Edit</a>", ">Delete</button>"} {
		if !strings.Contains(body, want) {
			t.Errorf("build row missing %q: %s", want, body)
		}
	}
	for _, unwanted := range []string{"completion-form", "check-button", "/toggle", `name="due_at"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("build page contains %q", unwanted)
		}
	}

	for _, option := range buildStatuses {
		response = perform(handler, formRequest(http.MethodPost, "/lists/build/items/1/edit", url.Values{
			"text": {"Personal app"}, "status": {option.Value},
		}))
		response = followRedirect(t, handler, response, "/lists/build")
		item, _ = store.item(1)
		if response.Code != http.StatusOK || item.Text != "Personal app" || item.Status != option.Value || item.Done ||
			!strings.Contains(response.Body.String(), `aria-label="Status: `+option.Label+`"`) || strings.Contains(response.Body.String(), "is-done") {
			t.Fatalf("edit status %s: status=%d item=%#v body=%s", option.Value, response.Code, item, response.Body.String())
		}

		response = performPage(t, handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/build/items/1/edit", nil))
		body = response.Body.String()
		selectMatch := regexp.MustCompile(`(?s)<select id="edit-status-1".*?</select>`).FindString(body)
		if response.Code != http.StatusOK || !strings.Contains(selectMatch, `value="`+option.Value+`" selected`) ||
			!strings.Contains(body, `value="Personal app"`) {
			t.Fatalf("edit didn't load status %s: status=%d body=%s", option.Value, response.Code, body)
		}
		newSelect := regexp.MustCompile(`(?s)<select id="new-status".*?</select>`).FindString(body)
		if !strings.Contains(newSelect, `value="idea" selected`) {
			t.Fatal("editing must not change the separate add form's default status")
		}
	}

	response = performPage(t, handler, formRequest(http.MethodPost, "/lists/build/items/1/toggle", url.Values{"done": {"true"}}))
	item, _ = store.item(1)
	if response.Code != http.StatusUnprocessableEntity || item.Done || item.Status != tododb.StatusFinished || !strings.Contains(response.Body.String(), "Build items use a status instead of a completion checkbox.") {
		t.Fatal("build items must not support checkbox completion")
	}

	response = perform(handler, formRequest(http.MethodPost, "/lists/build/items/1/delete", url.Values{}))
	response = followRedirect(t, handler, response, "/lists/build")
	if _, ok := store.item(1); response.Code != http.StatusOK || ok || !strings.Contains(response.Body.String(), "Nothing here yet.") {
		t.Fatalf("delete build: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestBuildValidationAndFailuresPreserveDrafts(t *testing.T) {
	store := newMemoryStore()
	handler, logs := newTestHandler(t, store)
	values := url.Values{"text": {"Robot"}, "status": {"unknown"}}
	response := performPage(t, handler, formRequest(http.MethodPost, "/lists/build/items", values))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), invalidStatusMessage) ||
		!strings.Contains(response.Body.String(), `value="Robot"`) || !strings.Contains(response.Body.String(), `aria-invalid="true"`) {
		t.Fatalf("invalid status not rejected visibly: status=%d body=%s", response.Code, response.Body.String())
	}
	if _, ok := store.item(1); ok {
		t.Fatal("invalid status created an item")
	}

	values.Set("text", " ")
	values.Set("status", tododb.StatusBuilding)
	response = performPage(t, handler, formRequest(http.MethodPost, "/lists/build/items", values))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "Enter an item before saving.") || !strings.Contains(response.Body.String(), `value=" "`) || !strings.Contains(response.Body.String(), `value="building" selected`) {
		t.Fatal("name validation discarded the status draft")
	}

	values.Set("text", "Robot")
	store.fail["create"] = errors.New("authToken=secret")
	response = performPage(t, handler, formRequest(http.MethodPost, "/lists/build/items", values))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "The item could not be saved.") || !strings.Contains(response.Body.String(), `value="building" selected`) ||
		!strings.Contains(response.Body.String(), `value="Robot"`) || strings.Contains(logs.String(), "secret") {
		t.Fatal("database failure discarded or leaked the build draft")
	}
	delete(store.fail, "create")
	response = perform(handler, formRequest(http.MethodPost, "/lists/build/items", values))
	response = followRedirect(t, handler, response, "/lists/build")
	if !strings.Contains(response.Body.String(), "Robot") || !strings.Contains(response.Body.String(), `aria-label="Status: Building"`) {
		t.Fatalf("create failed: %d", response.Code)
	}

	values.Set("status", "invalid")
	response = performPage(t, handler, formRequest(http.MethodPost, "/lists/build/items/1/edit", values))
	item, _ := store.item(1)
	if response.Code != http.StatusUnprocessableEntity || item.Status != tododb.StatusBuilding || !strings.Contains(response.Body.String(), invalidStatusMessage) ||
		!strings.Contains(response.Body.String(), `value="Robot"`) || !strings.Contains(response.Body.String(), `aria-invalid="true"`) {
		t.Fatal("invalid update changed the saved status")
	}

	values.Set("text", " ")
	values.Set("status", tododb.StatusPlanned)
	response = performPage(t, handler, formRequest(http.MethodPost, "/lists/build/items/1/edit", values))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "Enter an item before saving.") || !strings.Contains(response.Body.String(), `value=" "`) || !strings.Contains(response.Body.String(), `value="planned" selected`) {
		t.Fatal("failed edit discarded the selected status")
	}

	values.Set("text", "Workbench")
	store.fail["update"] = errors.New("unavailable")
	response = performPage(t, handler, formRequest(http.MethodPost, "/lists/build/items/1/edit", values))
	item, _ = store.item(1)
	if response.Code != http.StatusServiceUnavailable || item.Text != "Robot" || item.Status != tododb.StatusBuilding || !strings.Contains(response.Body.String(), "The item could not be saved.") ||
		!strings.Contains(response.Body.String(), `value="planned" selected`) || !strings.Contains(response.Body.String(), `value="Workbench"`) {
		t.Fatal("failed save changed the item or discarded its draft")
	}
}

func TestBuildIsolationAndSameOrigin(t *testing.T) {
	store := newMemoryStore()
	for _, list := range []string{tododb.ListShopping, tododb.ListTodo, tododb.ListBuild} {
		status := ""
		if list == tododb.ListBuild {
			status = tododb.StatusIdea
		}
		if err := store.Create(context.Background(), list, "Only in "+list, "", status); err != nil {
			t.Fatal(err)
		}
	}
	handler, _ := newTestHandler(t, store)
	for _, list := range []string{tododb.ListShopping, tododb.ListTodo, tododb.ListBuild} {
		response := performPage(t, handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/"+list, nil))
		body := response.Body.String()
		if !strings.Contains(body, "Only in "+list) {
			t.Errorf("missing own item in %s", list)
		}
		for _, other := range []string{tododb.ListShopping, tododb.ListTodo, tododb.ListBuild} {
			if other != list && strings.Contains(body, "Only in "+other) {
				t.Errorf("%s leaked an item from %s", list, other)
			}
		}
		if list != tododb.ListBuild && strings.Contains(body, `name="status"`) {
			t.Errorf("%s unexpectedly has build fields", list)
		}
	}

	for _, action := range []string{"edit", "delete"} {
		response := performPage(t, handler, formRequest(http.MethodPost, "/lists/build/items/1/"+action,
			url.Values{"text": {"Wrong list"}, "status": {tododb.StatusIdea}}))
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "not found in this list") {
			t.Errorf("cross-list %s status=%d, want 404", action, response.Code)
		}
	}
	for _, path := range []string{"", "/3/edit", "/3/delete", "/3/toggle"} {
		request := formRequest(http.MethodPost, "/lists/build/items"+path,
			url.Values{"text": {"Unsafe"}, "status": {tododb.StatusBuilding}, "done": {"true"}})
		request.Header.Set("Origin", "http://attacker.test")
		if response := performPage(t, handler, request); response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "This request was rejected.") {
			t.Errorf("cross-origin POST %s status=%d", path, response.Code)
		}
	}
}
