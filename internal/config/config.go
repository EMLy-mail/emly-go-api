package config

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type RateLimitConfig struct {
	UnauthMaxReqs  int
	UnauthWindow   time.Duration
	UnauthMaxFails int
	UnauthBanDur   time.Duration
	AuthMaxReqs    int
	AuthWindow     time.Duration
	AuthMaxFails   int
	AuthBanDur     time.Duration
}

type Config struct {
	Port            string
	DSN             string
	Database        string
	APIKey          string
	AdminKey        string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime int
	RateLimit       RateLimitConfig
}

var (
	instance *Config
	once     sync.Once
)

func Load() *Config {
	once.Do(func() { instance = load() })
	return instance
}

func load() *Config {
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
		RateLimit: RateLimitConfig{
			UnauthMaxReqs:  envInt("RL_UNAUTH_MAX_REQS", 10),
			UnauthWindow:   envDuration("RL_UNAUTH_WINDOW", 5*time.Minute),
			UnauthMaxFails: envInt("RL_UNAUTH_MAX_FAILS", 5),
			UnauthBanDur:   envDuration("RL_UNAUTH_BAN_DUR", 15*time.Minute),
			AuthMaxReqs:    envInt("RL_AUTH_MAX_REQS", 100),
			AuthWindow:     envDuration("RL_AUTH_WINDOW", time.Minute),
			AuthMaxFails:   envInt("RL_AUTH_MAX_FAILS", 20),
			AuthBanDur:     envDuration("RL_AUTH_BAN_DUR", 5*time.Minute),
		},
	}
}

func envInt(key string, fallback int) int {
	if s := os.Getenv(key); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if s := os.Getenv(key); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return fallback
}
