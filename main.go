package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/jmoiron/sqlx"
	"github.com/joho/godotenv"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"emly-api-go/internal/config"
	"emly-api-go/internal/database"
	"emly-api-go/internal/database/schema"
	emlyMiddleware "emly-api-go/internal/middleware"
	"emly-api-go/internal/routes"
	"emly-api-go/internal/storage"
	"emly-api-go/internal/telemetry"
)

// logBridge redirects the standard log package output to slog so that legacy
// log.Printf calls are forwarded through the OTel log pipeline.
type logBridge struct{}

func (logBridge) Write(p []byte) (int, error) {
	slog.Info(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func main() {
	_ = godotenv.Load()

	if name := os.Getenv("INSTANCE_NAME"); name != "" {
		log.SetPrefix("[" + name + "] ")
	}

	cfg := config.Load()

	// OTel setup — runs early so all subsequent logs flow through the pipeline.
	if cfg.Otel.Enabled {
		stdoutHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
		otelShutdown, err := telemetry.Setup(context.Background(), cfg.Otel.Endpoint, stdoutHandler)
		if err != nil {
			log.Fatalf("otel setup failed: %v", err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := otelShutdown(ctx); err != nil {
				slog.Error("otel shutdown error", "err", err)
			}
		}()

		// Forward standard log package output through slog → OTel.
		log.SetOutput(logBridge{})
		log.SetFlags(0)

		slog.Info("OpenTelemetry enabled", "endpoint", cfg.Otel.Endpoint)
	}

	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	defer func(db *sqlx.DB) {
		if err := db.Close(); err != nil {
			log.Fatalf("closing database failed: %v", err)
		}
	}(db)

	if err := schema.Migrate(db, cfg.Database); err != nil {
		log.Fatalf("schema migration failed: %v", err)
	}

	var s3conn *storage.S3Connector
	if cfg.UseS3CompatibleStorage {
		conn, err := storage.NewCloudflareR2Connector(cfg.R2)
		if err != nil {
			log.Fatalf("R2 connector init failed: %v", err)
		}
		if err := conn.Ping(context.Background()); err != nil {
			log.Fatalf("R2 connection test failed: %v", err)
		}
		slog.Info("R2 storage connected", "bucket", cfg.R2.BucketName)
		s3conn = conn
	}

	for _, arg := range os.Args[1:] {
		if arg == "--migrate-files" {
			if cfg.UseS3CompatibleStorage && s3conn != nil {
				slog.Info("migrating report files from db to s3")
				if err := storage.MigrateReportFilesToS3(db, s3conn, cfg.Database); err != nil {
					log.Fatalf("migrating report files failed: %v", err)
				}
				slog.Info("migration from db to s3 completed")
				continue
			}
			slog.Info("migrate-files skipped: R2 not enabled")
		}
	}

	r := chi.NewRouter()

	r.Use(chiMiddleware.RequestID)
	r.Use(chiMiddleware.RealIP)
	r.Use(chiMiddleware.Logger)
	r.Use(chiMiddleware.Recoverer)
	r.Use(chiMiddleware.Timeout(30 * time.Second))
	r.Use(emlyMiddleware.Timing)
	if cfg.Otel.Enabled {
		r.Use(otelhttp.NewMiddleware("emly-api",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return r.Method + " " + r.URL.Path
			}),
		))
	}

	rl := emlyMiddleware.NewRateLimiter(cfg)
	r.Use(rl.Handler)

	routes.RegisterAll(r, db, s3conn)

	addr := fmt.Sprintf(":%s", cfg.Port)
	slog.Info("server listening", "addr", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
