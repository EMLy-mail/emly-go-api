package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
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
	// NOTE: close the DB explicitly during graceful shutdown below

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
	r.Use(emlyMiddleware.AccessLog)
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
	srv := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	// Start server in a goroutine so we can listen for shutdown signals
	go func() {
		slog.Info("server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Graceful shutdown on interrupt (Ctrl-C) or SIGTERM
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	sig := <-quit
	slog.Info("shutdown signal received", "signal", sig.String())

	ctxShut, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	slog.Info("shutting down server, waiting for in-flight requests to finish")
	// Disable keep-alives to make sure no new requests are serviced on
	// long-lived connections during shutdown.
	srv.SetKeepAlivesEnabled(false)
	if err := srv.Shutdown(ctxShut); err != nil {
		slog.Error("server shutdown error", "err", err)
	} else {
		slog.Info("server shutdown complete")
	}

	// Close database connection
	if db != nil {
		if err := db.Close(); err != nil {
			slog.Error("closing database failed", "err", err)
		} else {
			slog.Info("database closed")
		}
	}

	// Close S3 connector (best-effort)
	if s3conn != nil {
		if err := s3conn.Close(); err != nil {
			slog.Error("closing s3 connector failed", "err", err)
		} else {
			slog.Info("s3 connector closed")
		}
	}

	slog.Info("shutdown complete")
}
