package main

import (
	"log"
	"net/http"
	"os"

	tododb "todo/internal/db"
	"todo/internal/handlers"
)

func main() {
	dsn := os.Getenv("TODO_DB_DSN")
	if dsn == "" {
		log.Fatal("TODO_DB_DSN env var is required, e.g. user:pass@tcp(host:3306)/dbname?parseTime=true")
	}

	addr := os.Getenv("TODO_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	conn, err := tododb.Connect(dsn)
	if err != nil {
		log.Fatalf("db connect failed: %v", err)
	}
	defer conn.Close()

	items := &handlers.Items{DB: conn}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/items", items.List)
	mux.HandleFunc("POST /api/items", items.Add)
	mux.HandleFunc("POST /api/items/{id}/toggle", items.Toggle)
	mux.HandleFunc("DELETE /api/items/{id}", items.Delete)
	mux.Handle("/", http.FileServer(http.Dir("web")))

	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
