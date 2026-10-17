package main

import (
	"context"
	"errors"
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
)

func main() {
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

	app, err := handlers.New(tododb.NewStore(dbConn), log.Default())
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
