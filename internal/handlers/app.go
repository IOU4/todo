package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tododb "github.com/IOU4/todo/internal/db"
)

const (
	databaseOperationTimeout = 5 * time.Second
	maxRequestBodyBytes      = 8 << 10
)

//go:embed templates/*.html static/*
var embeddedAssets embed.FS

// Repository is the small persistence surface used by the server-rendered app.
type Repository interface {
	List(context.Context, string) ([]tododb.Item, error)
	Get(context.Context, string, int64) (tododb.Item, error)
	Create(context.Context, string, string, string) error
	Update(context.Context, string, int64, string, string) error
	SetDone(context.Context, string, int64, bool) error
	Delete(context.Context, string, int64) error
}

type App struct {
	store     Repository
	template  *template.Template
	staticFS  fs.FS
	assetBase string
	logger    *log.Logger
}

type viewData struct {
	List         string
	ListTitle    string
	Items        []tododb.Item
	Draft        string
	DueDraft     string
	DueValue     string
	InvalidDue   bool
	EditingID    int64
	Message      string
	InvalidInput bool
	TextLimit    int
}

// New parses the embedded templates and static asset tree once at startup.
func New(store Repository, logger *log.Logger) (*App, error) {
	if store == nil {
		return nil, errors.New("a list repository is required")
	}
	if logger == nil {
		logger = log.Default()
	}

	staticFS, err := fs.Sub(embeddedAssets, "static")
	if err != nil {
		return nil, fmt.Errorf("open embedded static assets: %w", err)
	}
	// A new URL whenever embedded assets change prevents stale browser/CDN files
	// from being mixed with a newly deployed page.
	hash := sha256.New()
	for _, name := range []string{"style.css", "htmx.min.js", "app.js"} {
		content, err := fs.ReadFile(staticFS, name)
		if err != nil {
			return nil, fmt.Errorf("read embedded asset %s: %w", name, err)
		}
		_, _ = hash.Write(content)
	}
	assetBase := fmt.Sprintf("/static/%x", hash.Sum(nil)[:8])

	tmpl, err := template.New("lists").Funcs(template.FuncMap{
		"asset":     func(name string) string { return assetBase + "/" + name },
		"remaining": func(raw string) string { return remainingTime(raw, time.Now()) },
		"overdue": func(raw string) bool {
			due, err := time.Parse(time.RFC3339, raw)
			return err == nil && !due.After(time.Now())
		},
	}).ParseFS(embeddedAssets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse embedded templates: %w", err)
	}

	return &App{
		store:     store,
		template:  tmpl,
		staticFS:  staticFS,
		assetBase: assetBase,
		logger:    logger,
	}, nil
}

// Handler builds the application's routes and security/body-limit middleware.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.home)
	mux.HandleFunc("GET /lists/{list}", a.list)
	mux.HandleFunc("GET /lists/{list}/items/{id}/edit", a.editForm)
	mux.HandleFunc("POST /lists/{list}/items", a.create)
	mux.HandleFunc("POST /lists/{list}/items/{id}/edit", a.update)
	mux.HandleFunc("POST /lists/{list}/items/{id}/toggle", a.setDone)
	mux.HandleFunc("POST /lists/{list}/items/{id}/delete", a.delete)
	staticFiles := http.FileServer(http.FS(a.staticFS))
	mux.Handle("GET "+a.assetBase+"/", http.StripPrefix(a.assetBase+"/", staticFiles))
	// Keep unversioned URLs compatible, but require caches to revalidate them.
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.StripPrefix("/static/", staticFiles).ServeHTTP(w, r)
	}))

	return securityHeaders(limitRequestBody(mux))
}

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/lists/shopping", http.StatusSeeOther)
}

func (a *App) list(w http.ResponseWriter, r *http.Request) {
	listName := r.PathValue("list")
	if !tododb.ValidListName(listName) {
		a.invalidList(w, r)
		return
	}
	a.renderList(w, r, listName, http.StatusOK, "", 0, "", false)
}

func (a *App) editForm(w http.ResponseWriter, r *http.Request) {
	listName := r.PathValue("list")
	if !tododb.ValidListName(listName) {
		a.invalidList(w, r)
		return
	}
	id, ok := parseItemID(r.PathValue("id"))
	if !ok {
		a.renderList(w, r, listName, http.StatusBadRequest, "", 0, "Enter a valid item ID.", false)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), databaseOperationTimeout)
	item, err := a.store.Get(ctx, listName, id)
	cancel()
	if errors.Is(err, tododb.ErrNotFound) {
		a.renderList(w, r, listName, http.StatusNotFound, "", 0, "That item was not found in this list.", false)
		return
	}
	if err != nil {
		a.logDatabaseError("get", listName, id, err)
		a.renderList(w, r, listName, http.StatusServiceUnavailable, "", 0, "The item could not be loaded. Please try again.", false)
		return
	}

	a.renderList(w, r, listName, http.StatusOK, item.Text, item.ID, "", false)
}

func (a *App) create(w http.ResponseWriter, r *http.Request) {
	listName := r.PathValue("list")
	if !tododb.ValidListName(listName) {
		a.invalidList(w, r)
		return
	}
	if !sameOrigin(r) {
		a.renderList(w, r, listName, http.StatusForbidden, "", 0, "This request was rejected. Submit the form from this site.", false)
		return
	}
	if !a.readForm(w, r, listName, "", 0) {
		return
	}

	draft := r.PostForm.Get("text")
	text, err := tododb.ValidItemText(draft)
	if err != nil {
		message := itemTextErrorMessage(draft)
		a.renderList(w, r, listName, http.StatusUnprocessableEntity, draft, 0, message, true)
		return
	}

	dueAt, err := parseDueDate(r, listName)
	if err != nil {
		a.renderList(w, r, listName, http.StatusUnprocessableEntity, draft, 0, invalidDueMessage, false)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), databaseOperationTimeout)
	err = a.store.Create(ctx, listName, text, dueAt)
	cancel()
	if err != nil {
		a.logDatabaseError("create", listName, 0, err)
		a.renderList(w, r, listName, http.StatusServiceUnavailable, draft, 0, "The item could not be saved. Please try again.", false)
		return
	}
	a.finishMutation(w, r, listName)
}

func (a *App) update(w http.ResponseWriter, r *http.Request) {
	listName := r.PathValue("list")
	if !tododb.ValidListName(listName) {
		a.invalidList(w, r)
		return
	}
	id, ok := parseItemID(r.PathValue("id"))
	if !ok {
		a.renderList(w, r, listName, http.StatusBadRequest, "", 0, "Enter a valid item ID.", false)
		return
	}
	if !sameOrigin(r) {
		a.renderList(w, r, listName, http.StatusForbidden, "", id, "This request was rejected. Submit the form from this site.", false)
		return
	}
	if !a.readForm(w, r, listName, "", id) {
		return
	}

	draft := r.PostForm.Get("text")
	text, err := tododb.ValidItemText(draft)
	if err != nil {
		message := itemTextErrorMessage(draft)
		a.renderList(w, r, listName, http.StatusUnprocessableEntity, draft, id, message, true)
		return
	}

	dueAt, err := parseDueDate(r, listName)
	if err != nil {
		a.renderList(w, r, listName, http.StatusUnprocessableEntity, draft, id, invalidDueMessage, false)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), databaseOperationTimeout)
	err = a.store.Update(ctx, listName, id, text, dueAt)
	cancel()
	if errors.Is(err, tododb.ErrNotFound) {
		a.renderList(w, r, listName, http.StatusNotFound, "", 0, "That item was not found in this list.", false)
		return
	}
	if err != nil {
		a.logDatabaseError("update", listName, id, err)
		a.renderList(w, r, listName, http.StatusServiceUnavailable, draft, id, "The item could not be saved. Please try again.", false)
		return
	}
	a.finishMutation(w, r, listName)
}

func (a *App) setDone(w http.ResponseWriter, r *http.Request) {
	listName := r.PathValue("list")
	if !tododb.ValidListName(listName) {
		a.invalidList(w, r)
		return
	}
	id, ok := parseItemID(r.PathValue("id"))
	if !ok {
		a.renderList(w, r, listName, http.StatusBadRequest, "", 0, "Enter a valid item ID.", false)
		return
	}
	if !sameOrigin(r) {
		a.renderList(w, r, listName, http.StatusForbidden, "", 0, "This request was rejected. Submit the form from this site.", false)
		return
	}
	if !a.readForm(w, r, listName, "", 0) {
		return
	}

	var done bool
	switch r.PostForm.Get("done") {
	case "true":
		done = true
	case "false":
		done = false
	default:
		a.renderList(w, r, listName, http.StatusUnprocessableEntity, "", 0, "Choose a valid completion state.", false)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), databaseOperationTimeout)
	err := a.store.SetDone(ctx, listName, id, done)
	cancel()
	if errors.Is(err, tododb.ErrNotFound) {
		a.renderList(w, r, listName, http.StatusNotFound, "", 0, "That item was not found in this list.", false)
		return
	}
	if err != nil {
		a.logDatabaseError("set completion", listName, id, err)
		a.renderList(w, r, listName, http.StatusServiceUnavailable, "", 0, "The item could not be updated. Please try again.", false)
		return
	}
	a.finishMutation(w, r, listName)
}

func (a *App) delete(w http.ResponseWriter, r *http.Request) {
	listName := r.PathValue("list")
	if !tododb.ValidListName(listName) {
		a.invalidList(w, r)
		return
	}
	id, ok := parseItemID(r.PathValue("id"))
	if !ok {
		a.renderList(w, r, listName, http.StatusBadRequest, "", 0, "Enter a valid item ID.", false)
		return
	}
	if !sameOrigin(r) {
		a.renderList(w, r, listName, http.StatusForbidden, "", 0, "This request was rejected. Submit the form from this site.", false)
		return
	}
	if !a.readForm(w, r, listName, "", 0) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), databaseOperationTimeout)
	err := a.store.Delete(ctx, listName, id)
	cancel()
	if errors.Is(err, tododb.ErrNotFound) {
		a.renderList(w, r, listName, http.StatusNotFound, "", 0, "That item was not found in this list.", false)
		return
	}
	if err != nil {
		a.logDatabaseError("delete", listName, id, err)
		a.renderList(w, r, listName, http.StatusServiceUnavailable, "", 0, "The item could not be deleted. Please try again.", false)
		return
	}
	a.finishMutation(w, r, listName)
}

func (a *App) readForm(w http.ResponseWriter, r *http.Request, listName, draft string, editingID int64) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		a.renderList(w, r, listName, http.StatusBadRequest, draft, editingID, "The form submission was not valid.", false)
		return false
	}
	if err := r.ParseForm(); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			a.renderList(w, r, listName, http.StatusRequestEntityTooLarge, draft, editingID, "The form submission is too large.", false)
		} else {
			a.renderList(w, r, listName, http.StatusBadRequest, draft, editingID, "The form submission was not valid.", false)
		}
		return false
	}
	return true
}

func (a *App) finishMutation(w http.ResponseWriter, r *http.Request, listName string) {
	if isHTMX(r) {
		a.renderList(w, r, listName, http.StatusOK, "", 0, "", false)
		return
	}
	http.Redirect(w, r, "/lists/"+listName, http.StatusSeeOther)
}

func (a *App) renderList(w http.ResponseWriter, r *http.Request, listName string, status int, draft string, editingID int64, message string, invalidInput bool) {
	ctx, cancel := context.WithTimeout(r.Context(), databaseOperationTimeout)
	items, err := a.store.List(ctx, listName)
	cancel()
	if err != nil {
		a.logDatabaseError("list", listName, 0, err)
		status = http.StatusServiceUnavailable
		message = "The list could not be loaded. Please try again."
		invalidInput = false
		items = []tododb.Item{}
	}

	var dueDraft, dueValue string
	if listName == tododb.ListTodo {
		if r.Method == http.MethodPost && status >= 400 {
			dueDraft = r.PostForm.Get("due_at")
		} else if editingID != 0 {
			for _, item := range items {
				if item.ID == editingID {
					dueValue = item.DueAt
					if due, err := time.Parse(time.RFC3339, dueValue); err == nil {
						dueDraft = due.UTC().Format(dueInputLayout)
					}
					break
				}
			}
		}
	}

	a.render(w, r, viewData{
		List:         listName,
		ListTitle:    listTitle(listName),
		Items:        items,
		Draft:        draft,
		DueDraft:     dueDraft,
		DueValue:     dueValue,
		InvalidDue:   message == invalidDueMessage,
		EditingID:    editingID,
		Message:      message,
		InvalidInput: invalidInput,
		TextLimit:    tododb.MaxItemRunes,
	}, status)
}

func (a *App) invalidList(w http.ResponseWriter, r *http.Request) {
	data := viewData{
		List:      tododb.ListShopping,
		ListTitle: "Shopping",
		Items:     []tododb.Item{},
		Message:   "Unknown list. Choose Shopping or Todo.",
		TextLimit: tododb.MaxItemRunes,
	}
	a.render(w, r, data, http.StatusBadRequest)
}

func (a *App) render(w http.ResponseWriter, r *http.Request, data viewData, status int) {
	name := "page"
	if isHTMX(r) {
		name = "workspace"
	}

	var body bytes.Buffer
	if err := a.template.ExecuteTemplate(&body, name, data); err != nil {
		a.logger.Printf("HTML template rendering failed (error type %T)", err)
		http.Error(w, "The page could not be rendered.", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("Vary", "HX-Request")
	if status >= 400 && isHTMX(r) {
		w.Header().Set("X-Todo-Swap-Error", "true")
	}
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}

func (a *App) logDatabaseError(operation, listName string, id int64, err error) {
	// Driver errors can contain server-controlled text. Log the operation and
	// error type, never the error string or credential-bearing connection data.
	a.logger.Printf("database %s failed (list=%q, item_id=%d, error_type=%T)", operation, listName, id, err)
}

func parseItemID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, err == nil && id > 0
}

func itemTextErrorMessage(raw string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "Enter an item before saving."
	}
	if !utf8.ValidString(text) {
		return "Item text must be valid UTF-8."
	}
	return fmt.Sprintf("Items must be %d Unicode code points or fewer.", tododb.MaxItemRunes)
}

func listTitle(listName string) string {
	if listName == tododb.ListTodo {
		return "Todo"
	}
	return "Shopping"
}

func isHTMX(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("HX-Request"), "true")
}

func limitRequestBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
	rawOrigin := r.Header.Get("Origin")
	isOrigin := rawOrigin != ""
	if !isOrigin {
		rawOrigin = r.Header.Get("Referer")
	}
	if rawOrigin == "" || strings.EqualFold(rawOrigin, "null") {
		return false
	}

	origin, err := url.Parse(rawOrigin)
	if err != nil || origin.User != nil || origin.Hostname() == "" {
		return false
	}
	origin.Scheme = strings.ToLower(origin.Scheme)
	if origin.Scheme != "http" && origin.Scheme != "https" {
		return false
	}
	if isOrigin && (origin.Path != "" && origin.Path != "/" || origin.RawQuery != "" || origin.Fragment != "") {
		return false
	}

	expectedScheme := requestScheme(r)
	if origin.Scheme != expectedScheme {
		return false
	}

	host, err := url.Parse("//" + r.Host)
	if err != nil || host.User != nil || host.Hostname() == "" {
		return false
	}
	host.Scheme = expectedScheme
	return normalizedAuthority(origin) == normalizedAuthority(host)
}

func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	forwarded := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("X-Forwarded-Proto"), ",", 2)[0]))
	if forwarded == "http" || forwarded == "https" {
		return forwarded
	}
	if forwarded != "" {
		return ""
	}
	return "http"
}

func normalizedAuthority(u *url.URL) string {
	hostname := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if hostname == "" {
		return ""
	}
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		return net.JoinHostPort(hostname, port)
	}
	if strings.Contains(hostname, ":") {
		return "[" + hostname + "]"
	}
	return hostname
}
