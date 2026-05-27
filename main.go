package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jmoiron/sqlx"
	"github.com/joho/godotenv"

	"emly-api-go/internal/config"
	"emly-api-go/internal/database"
	"emly-api-go/internal/database/schema"
	"emly-api-go/internal/routes"
	"emly-api-go/internal/storage"

	emlyMiddleware "emly-api-go/internal/middleware"
)

func main() {
	// Load .env (ignored if not present in production)
	_ = godotenv.Load()

	if name := os.Getenv("INSTANCE_NAME"); name != "" {
		log.SetPrefix("[" + name + "] ")
	}

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

	var s3conn *storage.S3Connector
	if cfg.UseS3CompatibleStorage {
		conn, err := storage.NewCloudflareR2Connector(cfg.R2)
		if err != nil {
			log.Fatalf("R2 connector init failed: %v", err)
		}
		if err := conn.Ping(context.Background()); err != nil {
			log.Fatalf("R2 connection test failed: %v", err)
		}
		log.Printf("R2 storage connected (bucket: %s)", cfg.R2.BucketName)
		s3conn = conn
	}

	argsWithoutProg := os.Args[1:]
	for _, arg := range argsWithoutProg {
		log.Printf("arg: %s", arg)
		if arg == "--migrate-files" {
			if cfg.UseS3CompatibleStorage && s3conn != nil {
				log.Printf("migrate report files from db to s3...")
				if err := storage.MigrateReportFilesToS3(db, s3conn, cfg.Database); err != nil {
					log.Fatalf("migrating report files failed: %v", err)
				}
				log.Printf("migrate report files from db to s3 completed successfully")
				continue
			} else {
				log.Printf("migrate report files from db to s3 skipped (R2 not enabled)")
			}

		}
	}

	r := chi.NewRouter()

	// Global middlewares
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(emlyMiddleware.Timing)

	rl := emlyMiddleware.NewRateLimiter(cfg)

	r.Use(rl.Handler)

	routes.RegisterAll(r, db, s3conn)

	addr := fmt.Sprintf(":%s", cfg.Port)
	log.Printf("server listening on %s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
