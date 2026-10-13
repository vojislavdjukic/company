package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"company/backend/internal/api"
	"company/backend/internal/repository"
	"company/backend/internal/service"
)

func main() {
	// Wire the database repository into the service and then into the HTTP API.
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://company:company@localhost:5432/thecompany?sslmode=disable"
	}

	db, err := repository.OpenPostgres(databaseURL)
	if err != nil {
		log.Fatalf("open PostgreSQL connection: %v", err)
	}
	defer db.Close()

	port := 4000
	if value := os.Getenv("PORT"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 && parsed <= 65535 {
			port = parsed
		}
	}

	app := api.NewHandler(service.NewHealthService(db))
	server := &http.Server{
		Addr:              ":" + strconv.Itoa(port),
		Handler:           loggingMiddleware(app),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Give in-flight requests a short window to finish before exiting.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("server shutdown: %v", err)
		}
	}()

	log.Printf("listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// loggingMiddleware records each request's method, path, duration, and client IP.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s from %s", r.Method, r.URL.Path, time.Since(started), sourceIP(r))
	})
}

// sourceIP extracts the host portion of RemoteAddr when it includes a port.
func sourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
