// Package notifications schedules and delivers encrypted Web Push reminders.
package notifications

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	tododb "github.com/IOU4/todo/internal/db"
	webpush "github.com/SherClockHolmes/webpush-go"
)

var ErrInvalidSubscription = errors.New("invalid push subscription")

type Config struct {
	PublicKey  string
	PrivateKey string
	Subject    string // Contact email (optionally mailto:) or HTTPS URL.
}

type Repository interface {
	SavePushSubscription(context.Context, tododb.PushSubscription) error
	DeletePushSubscription(context.Context, string) error
	PushSubscriptions(context.Context) ([]tododb.PushSubscription, error)
	PendingReminders(context.Context, time.Time) ([]tododb.Reminder, error)
	ClaimPushDelivery(context.Context, tododb.Reminder, int64, time.Time) (bool, error)
	FinishPushDelivery(context.Context, tododb.Reminder, int64, bool) error
}

type Service struct {
	store  Repository
	config Config
	logger *log.Logger
	send   func(context.Context, []byte, *webpush.Subscription, *webpush.Options) (*http.Response, error)
	client *http.Client
	now    func() time.Time
}

func New(store Repository, config Config, logger *log.Logger) (*Service, error) {
	if store == nil {
		return nil, errors.New("a notification repository is required")
	}
	privateBytes, err := base64.RawURLEncoding.DecodeString(config.PrivateKey)
	if err != nil {
		return nil, errors.New("invalid TODO_VAPID_PRIVATE_KEY")
	}
	privateKey, err := ecdh.P256().NewPrivateKey(privateBytes)
	if err != nil {
		return nil, errors.New("invalid TODO_VAPID_PRIVATE_KEY")
	}
	publicBytes, err := base64.RawURLEncoding.DecodeString(config.PublicKey)
	if err != nil || !bytes.Equal(publicBytes, privateKey.PublicKey().Bytes()) {
		return nil, errors.New("TODO_VAPID_PUBLIC_KEY must match TODO_VAPID_PRIVATE_KEY")
	}
	config.Subject = strings.TrimPrefix(config.Subject, "mailto:")
	if strings.HasPrefix(config.Subject, "https://") {
		u, err := url.Parse(config.Subject)
		if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return nil, errors.New("invalid TODO_VAPID_SUBJECT: use a contact email or HTTPS URL")
		}
	} else if address, err := mail.ParseAddress(config.Subject); err != nil || address.Address != config.Subject {
		return nil, errors.New("invalid TODO_VAPID_SUBJECT: use a contact email or HTTPS URL")
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Service{
		store: store, config: config, logger: logger,
		send: webpush.SendNotificationWithContext, now: time.Now,
		client: &http.Client{
			Timeout: 10 * time.Second,
			// Never forward a signed push request to an endpoint's redirect target.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (s *Service) PublicKey() string { return s.config.PublicKey }

// ValidateEndpoint only permits known browser push services, not arbitrary URLs
// supplied by a client (which would turn the scheduler into an SSRF proxy).
func ValidateEndpoint(raw string) bool {
	if len(raw) > 2048 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Opaque != "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "fcm.googleapis.com" || host == "android.googleapis.com" ||
		host == "updates.push.services.mozilla.com" || strings.HasSuffix(host, ".push.services.mozilla.com") ||
		host == "web.push.apple.com" || strings.HasSuffix(host, ".push.apple.com") ||
		strings.HasSuffix(host, ".notify.windows.com")
}

func (s *Service) Subscribe(ctx context.Context, sub tododb.PushSubscription) error {
	if !ValidateEndpoint(sub.Endpoint) {
		return ErrInvalidSubscription
	}
	auth, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(sub.Keys.Auth, "="))
	if err != nil || len(auth) != 16 {
		return ErrInvalidSubscription
	}
	public, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(sub.Keys.P256dh, "="))
	if err != nil {
		return ErrInvalidSubscription
	}
	if _, err := ecdh.P256().NewPublicKey(public); err != nil {
		return ErrInvalidSubscription
	}
	sub.Keys.Auth = base64.RawURLEncoding.EncodeToString(auth)
	sub.Keys.P256dh = base64.RawURLEncoding.EncodeToString(public)
	return s.store.SavePushSubscription(ctx, sub)
}

func (s *Service) Unsubscribe(ctx context.Context, endpoint string) error {
	if !ValidateEndpoint(endpoint) {
		return ErrInvalidSubscription
	}
	return s.store.DeletePushSubscription(ctx, endpoint)
}

// Run checks immediately and every 30 seconds. Delivery receipts live in the
// database, so restarts and multiple browsers don't repeat successful sends.
// Failed sends are retried while the item is still incomplete and not overdue.
func (s *Service) Run(ctx context.Context) {
	s.deliver(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.deliver(ctx)
		}
	}
}

func (s *Service) deliver(ctx context.Context) {
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	subs, err := s.store.PushSubscriptions(queryCtx)
	cancel()
	if err != nil {
		s.logError("load subscriptions", err)
		return
	}
	if len(subs) == 0 {
		return
	}
	queryCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
	reminders, err := s.store.PendingReminders(queryCtx, s.now())
	cancel()
	if err != nil {
		s.logError("load reminders", err)
		return
	}
	for _, sub := range subs {
		for _, reminder := range reminders {
			if ctx.Err() != nil {
				return
			}
			// Check stored endpoints too, in case the database predates validation.
			if !ValidateEndpoint(sub.Endpoint) {
				break
			}
			if s.deliverOne(ctx, reminder, sub, s.now()) {
				break // This subscription has expired; don't send more to it.
			}
		}
	}
}

func (s *Service) deliverOne(ctx context.Context, reminder tododb.Reminder, sub tododb.PushSubscription, now time.Time) bool {
	due, err := time.Parse(time.RFC3339, reminder.DueAt)
	if err != nil || !due.After(now) {
		return false
	}
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	claimed, err := s.store.ClaimPushDelivery(queryCtx, reminder, sub.ID, now)
	cancel()
	if err != nil {
		s.logError("claim delivery", err)
		return false
	}
	if !claimed {
		return false
	}
	payload, _ := json.Marshal(struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Tag   string `json:"tag"`
	}{
		Title: "Todo reminder",
		Body:  reminder.Text + " — due " + due.UTC().Format("2006-01-02 15:04 UTC"),
		Tag:   "todo-" + reminder.DueAt + "-" + strconv.FormatInt(reminder.ItemID, 10),
	})
	// Never leave a reminder queued at a push provider past the todo's deadline.
	ttl := int(math.Ceil(due.Sub(now).Seconds()))
	if ttl > 86400 {
		ttl = 86400
	}
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	response, sendErr := s.send(sendCtx, payload, &webpush.Subscription{
		Endpoint: sub.Endpoint, Keys: webpush.Keys{P256dh: sub.Keys.P256dh, Auth: sub.Keys.Auth},
	}, &webpush.Options{
		HTTPClient: s.client, Subscriber: s.config.Subject,
		VAPIDPublicKey: s.config.PublicKey, VAPIDPrivateKey: s.config.PrivateKey,
		TTL: ttl, Urgency: webpush.UrgencyNormal,
	})
	cancel()
	var sent, expired bool
	if response != nil {
		sent = sendErr == nil && response.StatusCode >= 200 && response.StatusCode < 300
		expired = response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone
		if response.Body != nil {
			_ = response.Body.Close()
		}
		if !sent && !expired {
			s.logger.Printf("push delivery failed (status=%d)", response.StatusCode)
		}
	}
	if sendErr != nil {
		s.logError("send", sendErr)
	}
	// Even when shutdown cancels the HTTP request, release the claim promptly.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer finishCancel()
	if err := s.store.FinishPushDelivery(finishCtx, reminder, sub.ID, sent); err != nil {
		s.logError("record delivery", err)
	}
	if expired {
		if err := s.store.DeletePushSubscription(finishCtx, sub.Endpoint); err != nil {
			s.logError("remove expired subscription", err)
		}
	}
	return expired
}

func (s *Service) logError(operation string, err error) {
	// Error strings may include subscription URLs, VAPID keys, or DB credentials.
	s.logger.Printf("push %s failed (error_type=%T)", operation, err)
}

// GenerateKeys is deliberately separate from startup: the pair must remain
// stable across deployments or browsers must opt in again.
func GenerateKeys() (privateKey, publicKey string, err error) {
	return webpush.GenerateVAPIDKeys()
}

var _ Repository = (*tododb.Store)(nil)
