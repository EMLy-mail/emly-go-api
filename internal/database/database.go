package database

import (
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/config"
)

func Connect(cfg *config.Config) (*sqlx.DB, error) {
	db, err := sqlx.Connect("mysql", cfg.DSN)
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetime) * time.Minute)

	return db, nil
}
