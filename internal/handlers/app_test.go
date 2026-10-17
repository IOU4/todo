package handlers

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func (m *memoryStore) Create(ctx context.Context, listName, text string) error {
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
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	return nil
}

func (m *memoryStore) UpdateText(ctx context.Context, listName string, id int64, text string) error {
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

func formRequest(method, path string, values url.Values, htmx bool) *http.Request {
	request := httptest.NewRequest(method, "http://lists.test"+path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://lists.test")
	if htmx {
		request.Header.Set("HX-Request", "true")
	}
	return request
}

func perform(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestCreateEditCompleteDeleteAndEscapeItems(t *testing.T) {
	store := newMemoryStore()
	handler, _ := newTestHandler(t, store)

	response := perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/shopping", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET list status = %d, want 200", response.Code)
	}
	if !strings.Contains(response.Body.String(), "Nothing here yet.") || !strings.Contains(response.Body.String(), "Add to Shopping") {
		t.Fatalf("empty page is missing useful content: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `href="/lists/shopping" aria-current="page"`) || !strings.Contains(response.Body.String(), `href="/lists/todo"`) {
		t.Fatal("page does not render the two actual list navigation links")
	}

	for _, invalid := range []string{"   ", "\t \n"} {
		response = perform(handler, formRequest(http.MethodPost, "/lists/shopping/items", url.Values{"text": {invalid}}, true))
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("blank item status = %d, want 422", response.Code)
		}
		if !strings.Contains(response.Body.String(), "Enter an item before saving.") {
			t.Fatalf("blank-item failure is not visible: %s", response.Body.String())
		}
	}

	tooLong := strings.Repeat("x", tododb.MaxItemRunes+1)
	response = perform(handler, formRequest(http.MethodPost, "/lists/shopping/items", url.Values{"text": {tooLong}}, true))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "255 Unicode code points or fewer") {
		t.Fatalf("overlong item was not rejected visibly: status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `value="`+tooLong+`"`) {
		t.Fatal("failed creation did not preserve the submitted text")
	}

	unsafeText := `  <script>alert("x")</script>  `
	response = perform(handler, formRequest(http.MethodPost, "/lists/shopping/items", url.Values{"text": {unsafeText}}, false))
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/lists/shopping" {
		t.Fatalf("create response = %d, Location %q; want redirect to selected list", response.Code, response.Header().Get("Location"))
	}
	item, ok := store.item(1)
	if !ok || item.Text != `<script>alert("x")</script>` || item.ListType != tododb.ListShopping {
		t.Fatalf("created item = %#v, exists=%v", item, ok)
	}

	response = perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/shopping", nil))
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;`) || strings.Contains(body, `<script>alert("x")`) {
		t.Fatalf("item text was not safely escaped: status=%d body=%s", response.Code, body)
	}

	editRequest := httptest.NewRequest(http.MethodGet, "http://lists.test/lists/shopping/items/1/edit", nil)
	editRequest.Header.Set("HX-Request", "true")
	response = perform(handler, editRequest)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `id="edit-1"`) || !strings.Contains(response.Body.String(), `value="&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;"`) {
		t.Fatalf("edit form did not render escaped text: status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `value=""`) {
		t.Fatal("editing unexpectedly filled the separate create form")
	}

	response = perform(handler, formRequest(http.MethodPost, "/lists/shopping/items/1/edit", url.Values{"text": {"   "}}, true))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), `value="   "`) || !strings.Contains(response.Body.String(), `aria-invalid="true"`) {
		t.Fatalf("failed edit did not preserve and identify its input: status=%d body=%s", response.Code, response.Body.String())
	}

	response = perform(handler, formRequest(http.MethodPost, "/lists/shopping/items/1/edit", url.Values{"text": {"Fresh beans & tea"}}, true))
	item, _ = store.item(1)
	if response.Code != http.StatusOK || item.Text != "Fresh beans & tea" || !strings.Contains(response.Body.String(), "Fresh beans &amp; tea") {
		t.Fatalf("edit failed: status=%d item=%#v body=%s", response.Code, item, response.Body.String())
	}

	response = perform(handler, formRequest(http.MethodPost, "/lists/shopping/items/1/toggle", url.Values{"done": {"true"}}, true))
	item, _ = store.item(1)
	if response.Code != http.StatusOK || !item.Done || !strings.Contains(response.Body.String(), `aria-pressed="true"`) || !strings.Contains(response.Body.String(), "incomplete") {
		t.Fatalf("mark complete failed: status=%d item=%#v body=%s", response.Code, item, response.Body.String())
	}

	response = perform(handler, formRequest(http.MethodPost, "/lists/shopping/items/1/toggle", url.Values{"done": {"false"}}, true))
	item, _ = store.item(1)
	if response.Code != http.StatusOK || item.Done {
		t.Fatalf("mark incomplete failed: status=%d item=%#v", response.Code, item)
	}

	response = perform(handler, formRequest(http.MethodPost, "/lists/shopping/items/1/delete", url.Values{}, true))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Nothing here yet.") {
		t.Fatalf("delete failed to refresh empty state: status=%d body=%s", response.Code, response.Body.String())
	}
	if _, ok := store.item(1); ok {
		t.Fatal("delete left the item in storage")
	}
}

func TestListIsolationValidationNotFoundAndNoGETMutations(t *testing.T) {
	store := newMemoryStore()
	if err := store.Create(context.Background(), tododb.ListShopping, "Apples"); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), tododb.ListTodo, "Call home"); err != nil {
		t.Fatal(err)
	}
	handler, _ := newTestHandler(t, store)

	shopping := perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/shopping", nil))
	todo := perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/todo", nil))
	if !strings.Contains(shopping.Body.String(), "Apples") || strings.Contains(shopping.Body.String(), "Call home") {
		t.Fatal("shopping view leaked an item from the todo list")
	}
	if !strings.Contains(todo.Body.String(), "Call home") || strings.Contains(todo.Body.String(), "Apples") {
		t.Fatal("todo view leaked an item from the shopping list")
	}

	response := perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/other", nil))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "Unknown list") {
		t.Fatalf("invalid list response = %d, body=%s", response.Code, response.Body.String())
	}
	for _, id := range []string{"0", "-1", "not-an-id"} {
		response = perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/shopping/items/"+id+"/edit", nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("invalid item ID %q status = %d, want 400", id, response.Code)
		}
	}

	response = perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/shopping/items/2/edit", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("editing an item from another list status = %d, want 404", response.Code)
	}
	response = perform(handler, formRequest(http.MethodPost, "/lists/shopping/items/2/delete", url.Values{}, true))
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

	request := formRequest(http.MethodPost, "/lists/shopping/items", url.Values{"text": {"no origin"}}, true)
	request.Header.Del("Origin")
	response := perform(handler, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing Origin/Referer status = %d, want 403", response.Code)
	}

	request = formRequest(http.MethodPost, "/lists/shopping/items", url.Values{"text": {"cross-site"}}, true)
	request.Header.Set("Origin", "http://attacker.test")
	response = perform(handler, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin mutation status = %d, want 403", response.Code)
	}

	request = formRequest(http.MethodPost, "/lists/shopping/items", url.Values{"text": {"same-origin referer"}}, false)
	request.Header.Del("Origin")
	request.Header.Set("Referer", "http://lists.test/lists/shopping")
	response = perform(handler, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("same-origin Referer was not accepted: %d", response.Code)
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

	response := perform(handler, formRequest(http.MethodPost, "/lists/shopping/items", url.Values{"text": {"keep this text"}}, true))
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
	response = perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/todo", nil))
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
	response := perform(handler, formRequest(http.MethodPost, "/lists/shopping/items", tooLarge, true))
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
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Shopping — Lists") || !strings.Contains(page.Body.String(), "/static/style.css") {
		t.Fatalf("embedded template failed outside the repository: status=%d body=%s", page.Code, page.Body.String())
	}

	for path, want := range map[string]string{
		"/static/style.css":    "--paper: #f6f3eb",
		"/static/app.js":       "htmx:beforeSwap",
		"/static/htmx.min.js":  "var htmx=function()",
		"/static/HTMX-LICENSE": "Zero-Clause BSD",
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
