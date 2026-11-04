package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	tododb "github.com/IOU4/todo/internal/db"
	"github.com/IOU4/todo/internal/notifications"
)

type fakeNotifier struct {
	subs    []tododb.PushSubscription
	deleted []string
	err     error
}

func (*fakeNotifier) PublicKey() string { return "public-application-key" }
func (n *fakeNotifier) Subscribe(_ context.Context, sub tododb.PushSubscription) error {
	n.subs = append(n.subs, sub)
	return n.err
}
func (n *fakeNotifier) Unsubscribe(_ context.Context, endpoint string) error {
	n.deleted = append(n.deleted, endpoint)
	return n.err
}

func notificationRequest(path, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://lists.test"+path, strings.NewReader(body))
	r.Header.Set("Origin", "http://lists.test")
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestNotificationRoutesAndOptInUI(t *testing.T) {
	notifier := &fakeNotifier{}
	app, err := New(newMemoryStore(), nil, WithNotifications(notifier))
	if err != nil {
		t.Fatal(err)
	}
	handler := app.Handler()
	response := perform(handler, notificationRequest("/notifications/subscribe", `{"endpoint":"https://fcm.googleapis.com/token","expirationTime":null,"keys":{"p256dh":"public","auth":"auth"}}`))
	if response.Code != http.StatusNoContent || len(notifier.subs) != 1 || notifier.subs[0].Keys.Auth != "auth" {
		t.Fatalf("subscribe: status=%d, subs=%v", response.Code, notifier.subs)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("subscription response can be cached")
	}
	response = perform(handler, notificationRequest("/notifications/unsubscribe", `{"endpoint":"https://fcm.googleapis.com/token"}`))
	if response.Code != http.StatusNoContent || len(notifier.deleted) != 1 {
		t.Fatalf("unsubscribe status=%d", response.Code)
	}
	for _, list := range []string{"shopping", "todo"} {
		response = perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/"+list, nil))
		if strings.Contains(response.Body.String(), "data-notifications") != (list == "todo") {
			t.Fatalf("unexpected notification controls on %s", list)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "http://lists.test/lists/todo", nil)
	request.Header.Set("HX-Request", "true")
	response = perform(handler, request)
	if !strings.Contains(response.Body.String(), `data-vapid-public-key="public-application-key"`) {
		t.Fatal("HTMX workspace lost opt-in controls")
	}
}

func TestNotificationRoutesRejectInvalidRequests(t *testing.T) {
	notifier := &fakeNotifier{}
	app, err := New(newMemoryStore(), nil, WithNotifications(notifier))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, body string
		modify     func(*http.Request)
		want       int
	}{
		{"cross origin", `{}`, func(r *http.Request) { r.Header.Set("Origin", "https://evil.test") }, 403},
		{"no origin", `{}`, func(r *http.Request) { r.Header.Del("Origin") }, 403},
		{"form", `{}`, func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, 415},
		{"malformed", `{`, nil, 400},
		{"empty", ``, nil, 400},
		{"multiple values", `{} {}`, nil, 400},
		{"trailing junk", `{} broken`, nil, 400},
		{"too large", `{"endpoint":"` + strings.Repeat("x", maxRequestBodyBytes) + `"}`, nil, 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := notificationRequest("/notifications/subscribe", test.body)
			if test.modify != nil {
				test.modify(request)
			}
			response := perform(app.Handler(), request)
			if response.Code != test.want {
				t.Fatalf("status=%d, want %d", response.Code, test.want)
			}
		})
	}
	if len(notifier.subs) != 0 {
		t.Fatal("invalid request reached the notification service")
	}
	response := perform(app.Handler(), httptest.NewRequest(http.MethodGet, "http://lists.test/notifications/subscribe", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET subscription status=%d", response.Code)
	}
}

func TestNotificationSettingsFailuresAreSafe(t *testing.T) {
	for _, test := range []struct {
		err  error
		want int
	}{
		{notifications.ErrInvalidSubscription, 422},
		{errors.New("private-subscription-and-db-credentials"), 503},
	} {
		var logs bytes.Buffer
		app, err := New(newMemoryStore(), log.New(&logs, "", 0), WithNotifications(&fakeNotifier{err: test.err}))
		if err != nil {
			t.Fatal(err)
		}
		response := perform(app.Handler(), notificationRequest("/notifications/subscribe", `{}`))
		if response.Code != test.want || strings.Contains(logs.String()+response.Body.String(), "private-subscription") {
			t.Fatalf("unsafe failure: status=%d, logs=%s, body=%s", response.Code, logs.String(), response.Body.String())
		}
	}
	handler, _ := newTestHandler(t, newMemoryStore())
	response := perform(handler, notificationRequest("/notifications/subscribe", `{}`))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured notifications status=%d", response.Code)
	}
	response = perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/todo", nil))
	if strings.Contains(response.Body.String(), "data-notifications") {
		t.Fatal("unconfigured server offers notification opt-in")
	}
}

func TestEmbeddedManifestSupportsHomeScreenLaunch(t *testing.T) {
	handler, _ := newTestHandler(t, newMemoryStore())
	page := perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/lists/todo", nil))
	match := regexp.MustCompile(`rel="manifest" href="([^"]+)" crossorigin="use-credentials"`).FindStringSubmatch(page.Body.String())
	if len(match) != 2 {
		t.Fatal("page has no authenticated manifest link")
	}
	response := perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test"+match[1], nil))
	var manifest struct {
		StartURL string `json:"start_url"`
		Display  string `json:"display"`
		Scope    string `json:"scope"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || manifest.Display != "standalone" || manifest.StartURL != "/lists/todo" || manifest.Scope != "/" {
		t.Fatalf("invalid manifest: status=%d, data=%+v", response.Code, manifest)
	}
}

func TestNotificationJavaScript(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	if output, err := exec.Command("node", "--test", "testdata/notifications.test.cjs").CombinedOutput(); err != nil {
		t.Fatalf("notification JavaScript tests failed: %v\n%s", err, output)
	}
}

func TestServiceWorkerHasRootScopeAndRevalidates(t *testing.T) {
	handler, _ := newTestHandler(t, newMemoryStore())
	response := perform(handler, httptest.NewRequest(http.MethodGet, "http://lists.test/sw.js", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `self.addEventListener("push"`) || !strings.Contains(response.Body.String(), "notificationclick") {
		t.Fatalf("worker status=%d, body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-cache" || response.Header().Get("Service-Worker-Allowed") != "/" ||
		!strings.Contains(response.Header().Get("Content-Type"), "application/javascript") ||
		!strings.Contains(response.Header().Get("Content-Security-Policy"), "worker-src 'self'") {
		t.Fatalf("invalid worker headers: %v", response.Header())
	}
}
