package notifications

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	tododb "github.com/IOU4/todo/internal/db"
	webpush "github.com/SherClockHolmes/webpush-go"
)

type memoryRepository struct {
	subs      []tododb.PushSubscription
	reminders []tododb.Reminder
	claims    map[int64]bool
	sent      map[int64]bool
	deleted   []string
	saved     []tododb.PushSubscription
	claimable bool
	fail      error
}

func (m *memoryRepository) SavePushSubscription(_ context.Context, sub tododb.PushSubscription) error {
	m.saved = append(m.saved, sub)
	return m.fail
}
func (m *memoryRepository) DeletePushSubscription(_ context.Context, endpoint string) error {
	m.deleted = append(m.deleted, endpoint)
	return m.fail
}
func (m *memoryRepository) PushSubscriptions(context.Context) ([]tododb.PushSubscription, error) {
	return m.subs, m.fail
}
func (m *memoryRepository) PendingReminders(context.Context, time.Time) ([]tododb.Reminder, error) {
	return m.reminders, m.fail
}
func (m *memoryRepository) ClaimPushDelivery(_ context.Context, _ tododb.Reminder, id int64, _ time.Time) (bool, error) {
	if m.fail != nil {
		return false, m.fail
	}
	if !m.claimable || m.claims[id] || m.sent[id] {
		return false, nil
	}
	m.claims[id] = true
	return true, nil
}
func (m *memoryRepository) FinishPushDelivery(_ context.Context, _ tododb.Reminder, id int64, sent bool) error {
	delete(m.claims, id)
	m.sent[id] = sent
	return m.fail
}

func validSubscription(t *testing.T, id int64) tododb.PushSubscription {
	t.Helper()
	privateKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return tododb.PushSubscription{
		ID: id, Endpoint: "https://fcm.googleapis.com/fcm/send/browser-" + strings.Repeat("x", int(id)),
		Keys: tododb.PushKeys{
			P256dh: base64.RawURLEncoding.EncodeToString(privateKey.PublicKey().Bytes()),
			Auth:   base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef")),
		},
	}
}

func testService(t *testing.T) (*Service, *memoryRepository, *bytes.Buffer) {
	t.Helper()
	private, public, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryRepository{claims: make(map[int64]bool), sent: make(map[int64]bool), claimable: true}
	var logs bytes.Buffer
	service, err := New(store, Config{PublicKey: public, PrivateKey: private, Subject: "mailto:todo@example.com"}, log.New(&logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2030, 6, 9, 23, 0, 0, 0, time.UTC) }
	store.reminders = []tododb.Reminder{{ItemID: 42, Text: "Finish project", DueAt: "2030-06-10T15:00:00Z"}}
	return service, store, &logs
}

func TestEndpointValidationBlocksSSRF(t *testing.T) {
	for _, endpoint := range []string{
		"https://fcm.googleapis.com/fcm/send/token",
		"https://updates.push.services.mozilla.com/wpush/v2/token",
		"https://updates-autopush.push.services.mozilla.com/wpush/v2/token",
		"https://web.push.apple.com/token",
		"https://wns2-bl2p.notify.windows.com/w/?token=secret",
	} {
		if !ValidateEndpoint(endpoint) {
			t.Errorf("valid endpoint rejected: %q", endpoint)
		}
	}
	for _, endpoint := range []string{
		"", "http://fcm.googleapis.com/token", "https://127.0.0.1/token", "https://[::1]/token",
		"https://169.254.169.254/latest/meta-data", "https://example.com/token",
		"https://fcm.googleapis.com.evil.example/token", "https://fcm.googleapis.com@localhost/token",
		"https://user:password@fcm.googleapis.com/token", "https://fcm.googleapis.com:8080/token",
		"https://fcm.googleapis.com/token#fragment", "https://fcm.googleapis.com/" + strings.Repeat("x", 2048),
	} {
		if ValidateEndpoint(endpoint) {
			t.Errorf("unsafe endpoint accepted: %q", endpoint)
		}
	}
}

func TestSubscriptionsRequireValidKeys(t *testing.T) {
	service, store, _ := testService(t)
	sub := validSubscription(t, 1)
	if err := service.Subscribe(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	if len(store.saved) != 1 {
		t.Fatal("subscription was not persisted")
	}
	for _, change := range []func(*tododb.PushSubscription){
		func(sub *tododb.PushSubscription) { sub.Endpoint = "https://localhost/secret" },
		func(sub *tododb.PushSubscription) { sub.Keys.Auth = "invalid" },
		func(sub *tododb.PushSubscription) { sub.Keys.P256dh = "invalid" },
		func(sub *tododb.PushSubscription) {
			sub.Keys.P256dh = base64.RawURLEncoding.EncodeToString(make([]byte, 65))
		},
	} {
		invalid := sub
		change(&invalid)
		if err := service.Subscribe(context.Background(), invalid); !errors.Is(err, ErrInvalidSubscription) {
			t.Fatalf("invalid subscription error = %v", err)
		}
	}
	if len(store.saved) != 1 {
		t.Fatal("invalid subscription reached the database")
	}
	if err := service.Unsubscribe(context.Background(), sub.Endpoint); err != nil {
		t.Fatal(err)
	}
	if len(store.deleted) != 1 {
		t.Fatal("unsubscribe did not persist")
	}
}

func TestPushConfigRequiresMatchingStableKeysAndContact(t *testing.T) {
	service, store, _ := testService(t)
	if service.PublicKey() != service.config.PublicKey || service.config.Subject != "todo@example.com" {
		t.Fatal("config was not normalized")
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.PublicKey = "bad" },
		func(c *Config) { c.PrivateKey = "bad" },
		func(c *Config) { c.PrivateKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32)) },
		func(c *Config) { c.Subject = "" },
		func(c *Config) { c.Subject = "http://example.com" },
		func(c *Config) { c.Subject = "https://user:secret@example.com" },
	} {
		config := service.config
		mutate(&config)
		if _, err := New(store, config, nil); err == nil {
			t.Fatalf("invalid config accepted: subject=%q", config.Subject)
		}
	}
	config := service.config
	config.Subject = "https://example.com/contact"
	if _, err := New(store, config, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryIsPerBrowserAndSuccessfulSendsAreNotRepeated(t *testing.T) {
	service, store, _ := testService(t)
	store.subs = []tododb.PushSubscription{validSubscription(t, 1), validSubscription(t, 2)}
	calls := 0
	service.send = func(_ context.Context, payload []byte, sub *webpush.Subscription, options *webpush.Options) (*http.Response, error) {
		calls++
		var message map[string]string
		if err := json.Unmarshal(payload, &message); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(message["body"], "Finish project") || !strings.Contains(message["body"], "2030-06-10 15:00 UTC") || message["tag"] == "" {
			t.Fatalf("bad notification payload: %s", payload)
		}
		if sub.Endpoint == "" || options.TTL != 16*60*60 || options.VAPIDPrivateKey == "" {
			t.Fatalf("invalid send options: TTL=%d", options.TTL)
		}
		return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	service.deliver(context.Background())
	service.deliver(context.Background())
	if calls != 2 || !store.sent[1] || !store.sent[2] {
		t.Fatalf("sends=%d, receipts=%v", calls, store.sent)
	}
}

func TestFailedSendsRetryAndExpiredSubscriptionsAreRemoved(t *testing.T) {
	for _, test := range []struct {
		status                int
		networkError, expired bool
	}{
		{503, false, false}, {429, false, false}, {404, false, true}, {410, false, true}, {0, true, false},
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			service, store, logs := testService(t)
			store.subs = []tododb.PushSubscription{validSubscription(t, 1)}
			service.send = func(context.Context, []byte, *webpush.Subscription, *webpush.Options) (*http.Response, error) {
				if test.networkError {
					return nil, errors.New("secret-endpoint-and-keys")
				}
				return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader("secret-provider-error"))}, nil
			}
			service.deliver(context.Background())
			if store.claims[1] || store.sent[1] {
				t.Fatal("failed send was marked successful or left claimed")
			}
			if (len(store.deleted) == 1) != test.expired {
				t.Fatalf("deleted subscriptions = %v", store.deleted)
			}
			if strings.Contains(logs.String(), "secret") {
				t.Fatalf("credentials leaked into logs: %s", logs.String())
			}
			calls := 0
			service.send = func(context.Context, []byte, *webpush.Subscription, *webpush.Options) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 201}, nil
			}
			if !test.expired {
				service.deliver(context.Background())
				if calls != 1 {
					t.Fatal("failed send was not retried")
				}
			}
		})
	}
}

func TestNoSendForInvalidOverdueOrUnclaimableItems(t *testing.T) {
	for _, test := range []struct {
		due       string
		claimable bool
	}{
		{"not a date", true}, {"2030-06-09T23:00:00Z", true}, {"2030-06-09T22:00:00Z", true}, {"2030-06-10T15:00:00Z", false},
	} {
		service, store, _ := testService(t)
		store.subs = []tododb.PushSubscription{validSubscription(t, 1)}
		store.reminders[0].DueAt, store.claimable = test.due, test.claimable
		service.send = func(context.Context, []byte, *webpush.Subscription, *webpush.Options) (*http.Response, error) {
			t.Fatal("ineligible item was sent")
			return nil, nil
		}
		service.deliver(context.Background())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRealWebPushRequestIsEncryptedAndSigned(t *testing.T) {
	service, store, _ := testService(t)
	store.subs = []tododb.PushSubscription{validSubscription(t, 1)}
	called := false
	service.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte("Finish project")) || len(body) == 0 {
			t.Fatal("push payload is not encrypted")
		}
		if request.Header.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(request.Header.Get("Authorization"), "vapid ") {
			t.Fatal("push request lacks encryption or VAPID headers")
		}
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	service.deliver(context.Background())
	if !called || !store.sent[1] {
		t.Fatal("encrypted Web Push was not delivered")
	}
}
