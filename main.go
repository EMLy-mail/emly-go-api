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

// parseLogLevel maps the LOG_LEVEL env var to a slog.Level, defaulting to
// Info for an empty or unrecognized value.
func parseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func main() {
	_ = godotenv.Load()

	if name := os.Getenv("INSTANCE_NAME"); name != "" {
		log.SetPrefix("[" + name + "] ")
	}

	cfg := config.Load()

	logLevel := parseLogLevel(cfg.LogLevel)
	if !cfg.Otel.Enabled {
		// Without OTel, slog would otherwise fall back to its built-in
		// default handler (text on stderr, fixed at LevelInfo) — set one
		// explicitly so LOG_LEVEL is honored here too.
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))
	}

	// OTel setup — runs early so all subsequent logs flow through the pipeline.
	if cfg.Otel.Enabled {
		stdoutHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
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

	slog.Info("log level set", "level", logLevel.String())

	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	// NOTE: close the DB explicitly during graceful shutdown below

	if err := schema.Migrate(db, cfg.Database); err != nil {
		log.Fatalf("schema migration failed: %v", err)
	}

	// Two independent S3-compatible buckets, each optionally on a different
	// host/service/provider: one for bug-report file attachments, one for
	// software update installers.
	var apiFileS3conn *storage.S3Connector
	if cfg.UseS3APIFileStorage {
		conn, err := storage.NewS3Connector(cfg.S3APIFile)
		if err != nil {
			log.Fatalf("API file S3 connector init failed: %v", err)
		}
		if err := conn.Ping(context.Background()); err != nil {
			// Don't crash the whole API over an unreachable object store: warn and
			// keep the connector nil. Bug-report file handlers already treat a nil
			// connector as "fall back to the DB copy", so this bucket degrades
			// gracefully rather than failing requests.
			slog.Warn("API file S3 unreachable, degrading to DB-backed file storage", "bucket", cfg.S3APIFile.BucketName, "err", err)
		} else {
			slog.Info("API file S3 storage connected", "bucket", cfg.S3APIFile.BucketName)
			apiFileS3conn = conn
		}
	}

	var updatesS3conn *storage.S3Connector
	if cfg.UseS3UpdatesStorage {
		conn, err := storage.NewS3Connector(cfg.S3Updates)
		if err != nil {
			log.Fatalf("updates S3 connector init failed: %v", err)
		}
		if err := conn.Ping(context.Background()); err != nil {
			// Unlike the API file bucket, there is no DB fallback for release
			// installers: keep the connector nil so every update-release endpoint
			// that touches storage replies 503 until the bucket is reachable again.
			slog.Error("updates S3 unreachable, update-release storage endpoints will return 503", "bucket", cfg.S3Updates.BucketName, "err", err)
		} else {
			slog.Info("updates S3 storage connected", "bucket", cfg.S3Updates.BucketName)
			updatesS3conn = conn
		}
	}

	for _, arg := range os.Args[1:] {
		if arg == "--migrate-files" {
			if cfg.UseS3APIFileStorage && apiFileS3conn != nil {
				slog.Info("migrating report files from db to s3")
				if err := storage.MigrateReportFilesToS3(db, apiFileS3conn, cfg.Database); err != nil {
					log.Fatalf("migrating report files failed: %v", err)
				}
				slog.Info("migration from db to s3 completed")
				continue
			}
			slog.Info("migrate-files skipped: API file S3 storage not enabled")
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

	routes.RegisterAll(r, db, apiFileS3conn, updatesS3conn)

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

	// Close S3 connectors (best-effort)
	if apiFileS3conn != nil {
		if err := apiFileS3conn.Close(); err != nil {
			slog.Error("closing API file s3 connector failed", "err", err)
		} else {
			slog.Info("API file s3 connector closed")
		}
	}
	if updatesS3conn != nil {
		if err := updatesS3conn.Close(); err != nil {
			slog.Error("closing updates s3 connector failed", "err", err)
		} else {
			slog.Info("updates s3 connector closed")
		}
	}

	slog.Info("shutdown complete")
}
