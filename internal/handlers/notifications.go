package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"

	tododb "github.com/IOU4/todo/internal/db"
	"github.com/IOU4/todo/internal/notifications"
)

type NotificationService interface {
	PublicKey() string
	Subscribe(context.Context, tododb.PushSubscription) error
	Unsubscribe(context.Context, string) error
}

type Option func(*App)

func WithNotifications(service NotificationService) Option {
	return func(app *App) { app.notifier = service }
}

func (a *App) serviceWorker(w http.ResponseWriter, r *http.Request) {
	content, err := fs.ReadFile(a.staticFS, "sw.js")
	if err != nil {
		http.Error(w, "The notification worker could not be loaded.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Service-Worker-Allowed", "/")
	_, _ = w.Write(content)
}

func (a *App) subscribe(w http.ResponseWriter, r *http.Request) {
	var sub tododb.PushSubscription
	if !a.readNotificationJSON(w, r, &sub) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), databaseOperationTimeout)
	defer cancel()
	a.finishNotificationRequest(w, a.notifier.Subscribe(ctx, sub))
}

func (a *App) unsubscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if !a.readNotificationJSON(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), databaseOperationTimeout)
	defer cancel()
	a.finishNotificationRequest(w, a.notifier.Unsubscribe(ctx, body.Endpoint))
}

func (a *App) readNotificationJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	w.Header().Set("Cache-Control", "no-store")
	if a.notifier == nil {
		http.Error(w, "Notifications are not configured on this server.", http.StatusServiceUnavailable)
		return false
	}
	if !sameOrigin(r) {
		http.Error(w, "Submit notification settings from this site.", http.StatusForbidden)
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "Expected a JSON subscription.", http.StatusUnsupportedMediaType)
		return false
	}
	decoder := json.NewDecoder(r.Body)
	err = decoder.Decode(destination)
	if err == nil {
		var extra any
		if trailingErr := decoder.Decode(&extra); trailingErr != io.EOF {
			err = trailingErr
			if err == nil {
				err = errors.New("unexpected trailing JSON")
			}
		}
	}
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "The subscription request was not valid.", status)
		return false
	}
	return true
}

func (a *App) finishNotificationRequest(w http.ResponseWriter, err error) {
	if errors.Is(err, notifications.ErrInvalidSubscription) {
		http.Error(w, "The push subscription was not valid or its provider is unsupported.", http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		a.logger.Printf("notification settings failed (error_type=%T)", err)
		http.Error(w, "Notification settings could not be saved. Please try again.", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
