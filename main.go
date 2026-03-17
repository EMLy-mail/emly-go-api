package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httprate"
	"github.com/joho/godotenv"

	"emly-api-go/internal/config"
	"emly-api-go/internal/database"
	"emly-api-go/internal/handlers"
	apimw "emly-api-go/internal/middleware"
)

func main() {
	// Load .env (ignored if not present in production)
	_ = godotenv.Load()

	cfg := config.Load()

	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	defer db.Close()

	r := chi.NewRouter()

	// ── Global middleware ────────────────────────────────────────────────────
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	// ── Global rate-limit: 100 req / min per IP ──────────────────────────────
	r.Use(httprate.LimitByIP(100, time.Minute))

	// ── Public routes ────────────────────────────────────────────────────────
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("emly-api-go"))
	})

	r.Route("/api/v1", func(r chi.Router) {
		// Add a header called X-Server
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Server", "emly-api-go")
				next.ServeHTTP(w, r)
			})
		})

		// Health – public, no API key required
		r.Get("/health", handlers.Health(db))

		// ── Protected routes: require valid API key ──────────────────────────
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))

			// Tighter rate-limit on protected group: 30 req / min per IP
			r.Use(httprate.LimitByIP(30, time.Minute))

			r.Get("/example", handlers.ExampleGet)
			r.Post("/example", handlers.ExamplePost)
		})

		r.Route("/bug-reports", func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(apimw.APIKeyAuth(db))

				// Tighter rate-limit on protected group: 30 req / min per IP
				r.Use(httprate.LimitByIP(30, time.Minute))

				r.Get("/", handlers.GetAllBugReports(db))
				r.Get("/{id}", handlers.GetBugReportByID(db))
				r.Get("/count", handlers.GetReportsCount(db))
				r.Get("/{id}/files", handlers.GetReportFilesByReportID(db))
				r.Get("/{id}/files/{file_id}", handlers.GetReportFileByFileID(db))
				r.Get("/{id}/zip", handlers.GetBugReportZipById(db))
			})
		})

	})

	addr := fmt.Sprintf(":%s", cfg.Port)
	log.Printf("server listening on %s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
