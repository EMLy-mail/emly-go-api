package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port            string
	DSN             string
	Database        string
	APIKey          string
	AdminKey        string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime int
}

func Load() *Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	raw := os.Getenv("API_KEY")
	var apiKey string
	for _, k := range strings.Split(raw, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			apiKey = k
			break
		}
	}

	raw = os.Getenv("ADMIN_KEY")
	var adminKey string
	for _, k := range strings.Split(raw, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			adminKey = k
			break
		}
	}

	maxOpenConns, err := strconv.Atoi(os.Getenv("DB_MAX_OPEN_CONNS"))
	if err != nil {
		maxOpenConns = 30
	}
	maxIdleConns, err := strconv.Atoi(os.Getenv("DB_MAX_IDLE_CONNS"))
	if err != nil {
		maxIdleConns = 5
	}
	connMaxLifetime, err := strconv.Atoi(os.Getenv("DB_CONN_MAX_LIFETIME"))
	if err != nil {
		connMaxLifetime = 5
	}

	dbName := os.Getenv("DATABASE_NAME")
	if dbName == "" {
		panic("DATABASE_NAME environment variable is required")
	}

	if os.Getenv("DB_DSN") == "" {
		panic("DB_DSN environment variable is required")
	}

	return &Config{
		Port:            port,
		DSN:             os.Getenv("DB_DSN"),
		Database:        dbName,
		APIKey:          apiKey,
		AdminKey:        adminKey,
		MaxOpenConns:    maxOpenConns,
		MaxIdleConns:    maxIdleConns,
		ConnMaxLifetime: connMaxLifetime,
	}
}
