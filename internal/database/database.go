package database

import (
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"emly-api-go/internal/config"
)

func Connect(cfg *config.Config) (*sqlx.DB, error) {
	var db *sqlx.DB
	var err error

	switch cfg.Driver {
	case "sqlite":
		db, err = sqlx.Connect("sqlite", cfg.DSN)
		if err != nil {
			return nil, err
		}
		// Enable foreign key support (disabled by default in SQLite)
		if _, err = db.Exec("PRAGMA foreign_keys = ON"); err != nil {
			return nil, fmt.Errorf("sqlite: enable foreign_keys: %w", err)
		}
	case "mysql":
		db, err = sqlx.Connect("mysql", cfg.DSN)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(cfg.MaxOpenConns)
		db.SetMaxIdleConns(cfg.MaxIdleConns)
		db.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetime) * time.Minute)
	default:
		return nil, fmt.Errorf("unsupported DB_DRIVER %q: must be mysql or sqlite", cfg.Driver)
	}

	return db, nil
}
