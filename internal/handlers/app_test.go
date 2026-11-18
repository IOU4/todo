package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	tododb "github.com/IOU4/todo/internal/db"
)

type memoryStore struct {
	mu     sync.Mutex
	items  map[int64]tododb.Item
	nextID int64
	fail   map[string]error
}

func newMemoryStore() *memoryStore {
	return &memoryStore{items: make(map[int64]tododb.Item), nextID: 1, fail: make(map[string]error)}
}

func (m *memoryStore) check(ctx context.Context, operation string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return m.fail[operation]
}

func (m *memoryStore) List(ctx context.Context, listName string) ([]tododb.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(ctx, "list"); err != nil {
		return nil, err
	}
	items := make([]tododb.Item, 0)
	for _, item := range m.items {
		if item.ListType == listName {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

func (m *memoryStore) Get(ctx context.Context, listName string, id int64) (tododb.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(ctx, "get"); err != nil {
		return tododb.Item{}, err
	}
	item, ok := m.items[id]
	if !ok || item.ListType != listName {
		return tododb.Item{}, tododb.ErrNotFound
	}
	return item, nil
}

func (m *memoryStore) Create(ctx context.Context, listName, text, dueAt, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(ctx, "create"); err != nil {
		return err
	}
	id := m.nextID
	m.nextID++
	m.items[id] = tododb.Item{
		ID:        id,
		ListType:  listName,
		Text:      text,
		DueAt:     dueAt,
		Status:    status,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	return nil
}

func (m *memoryStore) Update(ctx context.Context, listName string, id int64, text, dueAt, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(ctx, "update"); err != nil {
		return err
	}
	item, ok := m.items[id]
	if !ok || item.ListType != listName {
		return tododb.ErrNotFound
	}
	item.Text = text
	item.DueAt = dueAt
	item.Status = status
	m.items[id] = item
	return nil
}

func (m *memoryStore) SetDone(ctx context.Context, listName string, id int64, done bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(ctx, "set_done"); err != nil {
		return err
	}
	item, ok := m.items[id]
	if !ok || item.ListType != listName {
		return tododb.ErrNotFound
	}
	item.Done = done
	m.items[id] = item
	return nil
}

func (m *memoryStore) Delete(ctx context.Context, listName string, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(ctx, "delete"); err != nil {
		return err
	}
	item, ok := m.items[id]
	if !ok || item.ListType != listName {
		return tododb.ErrNotFound
	}
	delete(m.items, id)
	return nil
}

func (m *memoryStore) item(id int64) (tododb.Item, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[id]
	return item, ok
}

func newTestHandler(t *testing.T, store Repository) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	app, err := New(store, log.New(&logs, "", 0))
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	return app.Handler(), &logs
}

func formRequest(method, path string, values url.Values) *http.Request {
	request := httptest.NewRequest(method, "http://lists.test"+path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://lists.test")
	return request
}

func perform(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func performPage(t *testing.T, handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	response := perform(handler, request)
	for _, want := range []string{"<!doctype html>", `<html lang="en">`, "<head>", "<body>", `<nav class="list-nav"`, `id="workspace"`, "</html>"} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("expected full page containing %q: status=%d body=%s", want, response.Code, response.Body.String())
		}
	}
	if response.Header().Get("Content-Type") != "text/html; charset=utf-8" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected page headers: %v", response.Header())
	}
	if strings.Contains(strings.ToLower(response.Header().Get("Vary")), "hx-request") || response.Header().Get("X-Todo-Swap-Error") != "" {
		t.Fatalf("page emitted obsolete swap headers: %v", response.Header())
	}
	return response
}

func followRedirect(t *testing.T, handler http.Handler, response *httptest.ResponseRecorder, location string) *httptest.ResponseRecorder {
	t.Helper()
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != location {
		t.Fatalf("mutation response = %d, Location %q; want 303 to %s", response.Code, response.Header().Get("Location"), location)
	}
	page := performPage(t, handler, httptest.NewRequest(http.MethodGet, "http://lists.test"+response.Header().Get("Location"), nil))
	if page.Code != http.StatusOK {
		t.Fatalf("redirected GET status = %d, want 200", page.Code)
	}
	return page
}

func TestCreateEditCompleteDeleteAndEscapeItems(t *testing.T) {
	store := newMemoryStore()
	handler, _ := newTestHandler(t, store)

	response := performPage(t, handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/shopping", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET list status = %d, want 200", response.Code)
	}
	if !strings.Contains(response.Body.String(), "Nothing here yet.") || !strings.Contains(response.Body.String(), "Add to Shopping") {
		t.Fatalf("empty page is missing useful content: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `href="/lists/shopping" aria-current="page"`) || !strings.Contains(response.Body.String(), `href="/lists/todo"`) || !strings.Contains(response.Body.String(), `href="/lists/build"`) {
		t.Fatal("page does not render all three list navigation links")
	}

	for _, invalid := range []string{"   ", "\t \n"} {
		response = performPage(t, handler, formRequest(http.MethodPost, "/lists/shopping/items", url.Values{"text": {invalid}}))
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("blank item status = %d, want 422", response.Code)
		}
		if !strings.Contains(response.Body.String(), "Enter an item before saving.") {
			t.Fatalf("blank-item failure is not visible: %s", response.Body.String())
		}
	}

	tooLong := strings.Repeat("x", tododb.MaxItemRunes+1)
	response = performPage(t, handler, formRequest(http.MethodPost, "/lists/shopping/items", url.Values{"text": {tooLong}}))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "255 Unicode code points or fewer") {
		t.Fatalf("overlong item was not rejected visibly: status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `value="`+tooLong+`"`) {
		t.Fatal("failed creation did not preserve the submitted text")
	}

	unsafeText := `  <script>alert("x")</script>  `
	response = perform(handler, formRequest(http.MethodPost, "/lists/shopping/items", url.Values{"text": {unsafeText}}))
	response = followRedirect(t, handler, response, "/lists/shopping")
	item, ok := store.item(1)
	if !ok || item.Text != `<script>alert("x")</script>` || item.ListType != tododb.ListShopping {
		t.Fatalf("created item = %#v, exists=%v", item, ok)
	}

	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;`) || strings.Contains(body, `<script>alert("x")`) {
		t.Fatalf("item text was not safely escaped: status=%d body=%s", response.Code, body)
	}

	response = performPage(t, handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/shopping/items/1/edit", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `id="edit-1"`) || !strings.Contains(response.Body.String(), `value="&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;"`) {
		t.Fatalf("edit form did not render escaped text: status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `value=""`) {
		t.Fatal("editing unexpectedly filled the separate create form")
	}

	response = performPage(t, handler, formRequest(http.MethodPost, "/lists/shopping/items/1/edit", url.Values{"text": {"   "}}))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "Enter an item before saving.") || !strings.Contains(response.Body.String(), `value="   "`) || !strings.Contains(response.Body.String(), `aria-invalid="true"`) {
		t.Fatalf("failed edit did not preserve and identify its input: status=%d body=%s", response.Code, response.Body.String())
	}

	if !strings.Contains(response.Body.String(), `href="/lists/shopping">Cancel</a>`) {
		t.Fatal("edit form is missing a native cancel link")
	}
	response = performPage(t, handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/shopping", nil))
	if strings.Contains(response.Body.String(), `id="edit-1"`) || !strings.Contains(response.Body.String(), `&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;`) {
		t.Fatal("cancel did not return to the unchanged list")
	}

	response = perform(handler, formRequest(http.MethodPost, "/lists/shopping/items/1/edit", url.Values{"text": {"Fresh beans & tea"}}))
	response = followRedirect(t, handler, response, "/lists/shopping")
	item, _ = store.item(1)
	if response.Code != http.StatusOK || item.Text != "Fresh beans & tea" || !strings.Contains(response.Body.String(), "Fresh beans &amp; tea") {
		t.Fatalf("edit failed: status=%d item=%#v body=%s", response.Code, item, response.Body.String())
	}

	response = perform(handler, formRequest(http.MethodPost, "/lists/shopping/items/1/toggle", url.Values{"done": {"true"}}))
	response = followRedirect(t, handler, response, "/lists/shopping")
	item, _ = store.item(1)
	if response.Code != http.StatusOK || !item.Done || !strings.Contains(response.Body.String(), `aria-pressed="true"`) || !strings.Contains(response.Body.String(), "incomplete") {
		t.Fatalf("mark complete failed: status=%d item=%#v body=%s", response.Code, item, response.Body.String())
	}

	response = perform(handler, formRequest(http.MethodPost, "/lists/shopping/items/1/toggle", url.Values{"done": {"false"}}))
	response = followRedirect(t, handler, response, "/lists/shopping")
	item, _ = store.item(1)
	if response.Code != http.StatusOK || item.Done || !strings.Contains(response.Body.String(), `aria-pressed="false"`) || !strings.Contains(response.Body.String(), "Fresh beans &amp; tea") {
		t.Fatalf("mark incomplete failed: status=%d item=%#v", response.Code, item)
	}

	response = perform(handler, formRequest(http.MethodPost, "/lists/shopping/items/1/delete", url.Values{}))
	response = followRedirect(t, handler, response, "/lists/shopping")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Nothing here yet.") {
		t.Fatalf("delete failed to refresh empty state: status=%d body=%s", response.Code, response.Body.String())
	}
	if _, ok := store.item(1); ok {
		t.Fatal("delete left the item in storage")
	}
}

func TestListIsolationValidationNotFoundAndNoGETMutations(t *testing.T) {
	store := newMemoryStore()
	if err := store.Create(context.Background(), tododb.ListShopping, "Apples", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), tododb.ListTodo, "Call home", "", ""); err != nil {
		t.Fatal(err)
	}
	handler, _ := newTestHandler(t, store)

	shopping := performPage(t, handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/shopping", nil))
	todo := performPage(t, handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/todo", nil))
	if !strings.Contains(shopping.Body.String(), "Apples") || strings.Contains(shopping.Body.String(), "Call home") {
		t.Fatal("shopping view leaked an item from the todo list")
	}
	if !strings.Contains(todo.Body.String(), "Call home") || strings.Contains(todo.Body.String(), "Apples") {
		t.Fatal("todo view leaked an item from the shopping list")
	}

	response := performPage(t, handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/other", nil))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "Unknown list") {
		t.Fatalf("invalid list response = %d, body=%s", response.Code, response.Body.String())
	}
	for _, id := range []string{"0", "-1", "not-an-id"} {
		response = performPage(t, handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/shopping/items/"+id+"/edit", nil))
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "Enter a valid item ID.") {
			t.Errorf("invalid item ID %q status = %d, want 400", id, response.Code)
		}
	}

	response = performPage(t, handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/shopping/items/2/edit", nil))
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "not found in this list") {
		t.Fatalf("editing an item from another list status = %d, want 404", response.Code)
	}
	response = performPage(t, handler, formRequest(http.MethodPost, "/lists/shopping/items/2/delete", url.Values{}))
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "not found in this list") {
		t.Fatalf("cross-list delete response = %d body=%s", response.Code, response.Body.String())
	}

	response = perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/shopping/items/1/delete", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET mutation endpoint status = %d, want 405", response.Code)
	}
}

func TestMutationsRequireSameOrigin(t *testing.T) {
	store := newMemoryStore()
	handler, _ := newTestHandler(t, store)

	request := formRequest(http.MethodPost, "/lists/shopping/items", url.Values{"text": {"no origin"}})
	request.Header.Del("Origin")
	response := performPage(t, handler, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "This request was rejected.") {
		t.Fatalf("missing Origin/Referer status = %d, want 403", response.Code)
	}

	request = formRequest(http.MethodPost, "/lists/shopping/items", url.Values{"text": {"cross-site"}})
	request.Header.Set("Origin", "http://attacker.test")
	response = performPage(t, handler, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "This request was rejected.") {
		t.Fatalf("cross-origin mutation status = %d, want 403", response.Code)
	}

	request = formRequest(http.MethodPost, "/lists/shopping/items", url.Values{"text": {"same-origin referer"}})
	request.Header.Del("Origin")
	request.Header.Set("Referer", "http://lists.test/lists/shopping")
	response = perform(handler, request)
	response = followRedirect(t, handler, response, "/lists/shopping")
	if !strings.Contains(response.Body.String(), "same-origin referer") {
		t.Fatal("same-origin submission did not render the created item")
	}
	item, ok := store.item(1)
	if !ok || item.Text != "same-origin referer" {
		t.Fatalf("same-origin request did not create item: %#v", item)
	}
}

func TestDatabaseFailuresAreVisiblePreserveDraftAndDoNotLeakErrors(t *testing.T) {
	store := newMemoryStore()
	store.fail["create"] = errors.New("remote error contained authToken=very-secret")
	handler, logs := newTestHandler(t, store)

	response := performPage(t, handler, formRequest(http.MethodPost, "/lists/shopping/items", url.Values{"text": {"keep this text"}}))
	body := response.Body.String()
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(body, "The item could not be saved") || !strings.Contains(body, `value="keep this text"`) {
		t.Fatalf("failed submission was not shown/preserved: status=%d body=%s", response.Code, body)
	}
	if strings.Contains(body, "very-secret") || strings.Contains(logs.String(), "very-secret") {
		t.Fatal("credential-bearing database error was leaked")
	}
	if !strings.Contains(logs.String(), "database create failed") {
		t.Fatalf("server-side operation failure was not logged: %q", logs.String())
	}

	store.fail["list"] = errors.New("backend detail: another-secret")
	response = performPage(t, handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/todo", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "The list could not be loaded") {
		t.Fatalf("list failure was not visible: status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "another-secret") || strings.Contains(logs.String(), "another-secret") {
		t.Fatal("raw database error was exposed in a response or log")
	}
}

func TestRequestBodyLimit(t *testing.T) {
	store := newMemoryStore()
	handler, _ := newTestHandler(t, store)
	tooLarge := url.Values{"text": {strings.Repeat("x", maxRequestBodyBytes+1)}}
	response := performPage(t, handler, formRequest(http.MethodPost, "/lists/shopping/items", tooLarge))
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "form submission is too large") {
		t.Fatalf("large request response = %d body=%s", response.Code, response.Body.String())
	}
	if _, ok := store.item(1); ok {
		t.Fatal("oversized request created an item")
	}
}

func TestEmbeddedAssetsAndTemplatesWorkOutsideRepositoryDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	handler, _ := newTestHandler(t, newMemoryStore())

	page := perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/shopping", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Shopping — Lists") || !regexp.MustCompile(`/static/[0-9a-f]{16}/style\.css`).MatchString(page.Body.String()) {
		t.Fatalf("embedded template failed outside the repository: status=%d body=%s", page.Code, page.Body.String())
	}

	for path, want := range map[string]string{
		"/static/style.css": "--paper: #f6f3eb",
		"/static/app.js":    "initializeDueDates();",
	} {
		response := perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test"+path, nil))
		assetBody := response.Body.String()
		if len(assetBody) > 100 {
			assetBody = assetBody[:100]
		}
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), want) {
			t.Errorf("embedded asset %s: status=%d body prefix=%q", path, response.Code, assetBody)
		}
	}
}

func TestVersionedAssetsBypassStaleDeploymentCaches(t *testing.T) {
	handler, _ := newTestHandler(t, newMemoryStore())
	page := perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/todo", nil))
	paths := regexp.MustCompile(`/static/[0-9a-f]{16}/(?:style\.css|app\.js)`).FindAllString(page.Body.String(), -1)
	if len(paths) != 2 {
		t.Fatalf("expected versioned CSS and scripts, got %v", paths)
	}
	for _, path := range paths {
		versioned := perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test"+path, nil))
		name := path[strings.LastIndex(path, "/")+1:]
		unversioned := perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/static/"+name, nil))
		if versioned.Code != http.StatusOK || versioned.Body.String() != unversioned.Body.String() {
			t.Errorf("versioned asset %s didn't serve the embedded file: status=%d", path, versioned.Code)
		}
		if unversioned.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("unversioned asset %s doesn't require cache revalidation", name)
		}
	}
	if strings.Contains(page.Body.String(), `for="new-due"`) {
		t.Fatal("the new due input should stay label-free")
	}
	if !strings.Contains(page.Body.String(), `aria-label="Due date and time (UTC, optional)"`) {
		t.Fatal("the label-free date input needs an accessible name")
	}
}

func TestNativeFormsAndLinksDoNotUseHTMX(t *testing.T) {
	store := newMemoryStore()
	handler, _ := newTestHandler(t, store)
	for _, list := range []string{tododb.ListShopping, tododb.ListTodo, tododb.ListBuild} {
		if err := store.Create(context.Background(), list, "Native item", "", tododb.StatusIdea); err != nil {
			t.Fatal(err)
		}
		base := "/lists/" + list
		itemPath := base + "/items/" + fmt.Sprint(store.nextID-1)
		for _, path := range []string{base, itemPath + "/edit"} {
			page := performPage(t, handler, httptest.NewRequest(http.MethodGet, "http://lists.test"+path, nil))
			body := page.Body.String()
			if strings.Contains(strings.ToLower(body), "htmx") || regexp.MustCompile(`(?i)\s(?:data-)?hx-[\w-]+\s*=`).MatchString(body) {
				t.Fatalf("page %s contains HTMX markup: %s", path, body)
			}
			controls := []string{`action="` + base + `/items" method="post"`}
			if path == base {
				controls = append(controls, `href="`+itemPath+`/edit"`, `action="`+itemPath+`/delete" method="post"`)
				if list != tododb.ListBuild {
					controls = append(controls, `action="`+itemPath+`/toggle" method="post"`)
				}
			} else {
				controls = append(controls, `action="`+itemPath+`/edit" method="post"`, `href="`+base+`">Cancel</a>`)
			}
			for _, control := range controls {
				if !strings.Contains(body, control) {
					t.Errorf("page %s missing native control %s", path, control)
				}
			}
		}
	}
	for _, name := range []string{"templates/page.html", "static/app.js", "static/style.css"} {
		content, err := embeddedAssets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(content)), "htmx") {
			t.Errorf("%s contains an HTMX runtime reference", name)
		}
	}
	for _, name := range []string{"htmx.min.js", "HTMX-LICENSE"} {
		if _, err := embeddedAssets.ReadFile("static/" + name); err == nil {
			t.Errorf("removed asset %s is still embedded", name)
		}
		response := perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/static/"+name, nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("removed asset %s status=%d, want 404", name, response.Code)
		}
	}
}

func TestSecurityHeadersAreSet(t *testing.T) {
	handler, _ := newTestHandler(t, newMemoryStore())
	response := perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/shopping", nil))
	if response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("security headers missing: %#v", response.Header())
	}
	if !strings.Contains(response.Header().Get("Content-Security-Policy"), "form-action 'self'") {
		t.Fatalf("unexpected CSP: %q", response.Header().Get("Content-Security-Policy"))
	}
}

var _ Repository = (*memoryStore)(nil)
