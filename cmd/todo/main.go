package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tododb "github.com/IOU4/todo/internal/db"
	"github.com/IOU4/todo/internal/handlers"
	"github.com/IOU4/todo/internal/notifications"
)

func main() {
	generateKeys := flag.Bool("generate-vapid-keys", false, "generate a persistent Web Push key pair and exit")
	flag.Parse()
	if *generateKeys {
		privateKey, publicKey, err := notifications.GenerateKeys()
		if err != nil {
			log.Fatal("could not generate Web Push keys")
		}
		fmt.Printf("TODO_VAPID_PUBLIC_KEY=%s\nTODO_VAPID_PRIVATE_KEY=%s\n", publicKey, privateKey)
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	databaseURL := os.Getenv("TODO_DB_URL")
	if strings.TrimSpace(databaseURL) == "" {
		return errors.New("TODO_DB_URL is required (for example: libsql://your-db.turso.io?authToken=YOUR_TOKEN)")
	}

	dbConn, err := tododb.Connect(context.Background(), databaseURL)
	if err != nil {
		return fmt.Errorf("database startup failed: %w", err)
	}
	defer dbConn.Close()

	store := tododb.NewStore(dbConn)
	pushConfig := notifications.Config{
		PublicKey: os.Getenv("TODO_VAPID_PUBLIC_KEY"), PrivateKey: os.Getenv("TODO_VAPID_PRIVATE_KEY"),
		Subject: os.Getenv("TODO_VAPID_SUBJECT"),
	}
	var notifier *notifications.Service
	var options []handlers.Option
	if pushConfig.PublicKey != "" || pushConfig.PrivateKey != "" || pushConfig.Subject != "" {
		notifier, err = notifications.New(store, pushConfig, log.Default())
		if err != nil {
			return fmt.Errorf("notification startup failed: %w", err)
		}
		options = append(options, handlers.WithNotifications(notifier))
	}
	app, err := handlers.New(store, log.Default(), options...)
	if err != nil {
		return fmt.Errorf("application startup failed: %w", err)
	}

	addr := os.Getenv("TODO_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	stopCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if notifier != nil {
		workerCtx, cancelWorker := context.WithCancel(stopCtx)
		workerDone := make(chan struct{})
		go func() {
			defer close(workerDone)
			notifier.Run(workerCtx)
		}()
		defer func() {
			cancelWorker()
			<-workerDone
		}()
	}

	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.ListenAndServe()
	}()

	log.Printf("listening on %s", addr)
	select {
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("HTTP server failed: %w", err)
		}
		return nil
	case <-stopCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return errors.New("HTTP server shutdown timed out")
		}
		if err := <-serverErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("HTTP server failed during shutdown: %w", err)
		}
		return nil
	}
}
