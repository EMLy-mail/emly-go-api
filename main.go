package main

import (
	apimw "emly-api-go/internal/middleware"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httprate"
	"github.com/jmoiron/sqlx"
	"github.com/joho/godotenv"

	"emly-api-go/internal/config"
	"emly-api-go/internal/database"
	"emly-api-go/internal/database/schema"
	"emly-api-go/internal/handlers"
)

func main() {
	// Load .env (ignored if not present in production)
	_ = godotenv.Load()

	cfg := config.Load()

	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	defer func(db *sqlx.DB) {
		err := db.Close()
		if err != nil {
			log.Fatalf("closing database failed: %v", err)
		}
	}(db)

	// Run conditional schema migrations
	if err := schema.Migrate(db, cfg.Database); err != nil {
		log.Fatalf("schema migration failed: %v", err)
	}

	r := chi.NewRouter()

	// Global middlewares
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	// Global rate limit to 100 requests per minute
	r.Use(httprate.LimitByIP(100, time.Minute))

	// Public routes (Not protected by any API Key)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("emly-api-go"))
	})

	r.Route("/v1", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Server", "emly-api-go")
				w.Header().Set("X-Powered-By", "Pure Protogen sillyness :3")
				next.ServeHTTP(w, r)
			})
		})

		// Health – public, no API key required
		r.Get("/health", handlers.Health(db))

		r.Route("/api", func(r chi.Router) {
			r.Route("/admin", func(r chi.Router) {
				r.Use(httprate.LimitByIP(30, time.Minute))

				// ROUTE: Auth — public, handles its own credential checks
				r.Route("/auth", func(r chi.Router) {
					r.Post("/login", handlers.LoginUser(db))
					r.Get("/validate", handlers.ValidateSession(db))
					r.Post("/logout", handlers.LogoutSession(db))
				})

				// ROUTE: User management — protected via Admin Key
				r.Route("/users", func(r chi.Router) {
					r.Use(apimw.AdminKeyAuth(db))

					r.Get("/", handlers.ListUsers(db))
					r.Post("/", handlers.CreateUser(db))
					r.Get("/{id}", handlers.GetUserByID(db))
					r.Patch("/{id}", handlers.UpdateUser(db))
					r.Post("/{id}/reset-password", handlers.ResetPassword(db))
					r.Delete("/{id}", handlers.DeleteUser(db))
				})
			})

			// ROUTE: Bug Reports - Protected via API Key
			r.Route("/bug-reports", func(r chi.Router) {
				r.Group(func(r chi.Router) {
					r.Use(apimw.APIKeyAuth(db))

					// Tighter rate-limit on protected group: 30 req / min per IP
					r.Use(httprate.LimitByIP(30, time.Minute))

					r.Get("/count", handlers.GetReportsCount(db))
					r.Post("/", handlers.CreateBugReport(db))
				})

				r.Group(func(r chi.Router) {
					// More strict auth due to sensitive info
					r.Use(apimw.APIKeyAuth(db))
					r.Use(apimw.AdminKeyAuth(db))

					// Tighter rate-limit on protected group: 30 req / min per IP
					r.Use(httprate.LimitByIP(30, time.Minute))

					r.Get("/", handlers.GetAllBugReports(db))
					r.Get("/{id}", handlers.GetBugReportByID(db))
					r.Get("/{id}/status", handlers.GetReportStatusByID(db))
					r.Get("/{id}/files", handlers.GetReportFilesByReportID(db))
					r.Get("/{id}/files/{file_id}", handlers.GetReportFileByFileID(db))
					r.Get("/{id}/download", handlers.GetBugReportZipById(db))
					r.Patch("/{id}/status", handlers.PatchBugReportStatus(db))
					r.Delete("/{id}", handlers.DeleteBugReportByID(db))
				})
			})
		})

	})

	addr := fmt.Sprintf(":%s", cfg.Port)
	log.Printf("server listening on %s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
