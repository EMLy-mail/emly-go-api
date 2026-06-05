package config

import (
	"os"
	"regexp"
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

type R2Config struct {
	AccountID       string
	AccessKeyID     string
	SecretAccessKey string
	BucketName      string
	Region          string
	Endpoint        string
}

type OtelConfig struct {
	Enabled  bool
	Endpoint string
}

type Config struct {
	Port                   string
	DSN                    string
	Database               string
	APIKey                 string
	AdminKey               string
	DashboardKey           string
	MaxOpenConns           int
	MaxIdleConns           int
	ConnMaxLifetime        int
	UpdatesEnabled         bool
	APIBaseURL             string
	UpdatesS3Prefix        string
	UseS3CompatibleStorage bool
	RateLimit              RateLimitConfig
	R2                     R2Config
	Otel                   OtelConfig
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
	dbNameRegex := regexp.MustCompile("^[a-zA-Z0-9_]+$")
	// Test the regex against the dbName, otherwise panic to prevent potential SQL injection
	validDbName, err := regexp.Match(dbNameRegex.String(), []byte(dbName))
	if err != nil {
		panic("failed to validate database name: " + err.Error())
	}
	if !validDbName {
		panic("invalid database name: must match regex " + dbNameRegex.String())
	}

	if os.Getenv("DB_DSN") == "" {
		panic("DB_DSN environment variable is required")
	}

	return &Config{
		Port:                   port,
		DSN:                    os.Getenv("DB_DSN"),
		Database:               dbName,
		APIKey:                 apiKey,
		AdminKey:               adminKey,
		DashboardKey:           os.Getenv("DASHBOARD_KEY"),
		MaxOpenConns:           maxOpenConns,
		MaxIdleConns:           maxIdleConns,
		ConnMaxLifetime:        connMaxLifetime,
		UpdatesEnabled:         strings.ToLower(strings.TrimSpace(os.Getenv("UPDATES_ENABLED"))) == "true",
		APIBaseURL:             strings.TrimRight(envString("API_BASE_URL", "http://localhost:8080"), "/"),
		UpdatesS3Prefix:        strings.Trim(os.Getenv("UPDATES_S3_PREFIX"), "/"),
		UseS3CompatibleStorage: strings.ToLower(strings.TrimSpace(os.Getenv("USE_S3_COMPATIBLE_STORAGE"))) == "true",
		Otel: OtelConfig{
			Enabled:  strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_ENABLED"))) == "true",
			Endpoint: envString("OTEL_ENDPOINT", "http://localhost:4318"),
		},
		R2: R2Config{
			AccountID:       os.Getenv("CF_ACCOUNT_ID"),
			AccessKeyID:     os.Getenv("CF_R2_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("CF_R2_SECRET_ACCESS_KEY"),
			BucketName:      os.Getenv("CF_R2_BUCKET_NAME"),
			Region:          envString("CF_R2_REGION", "auto"),
			Endpoint:        os.Getenv("CF_R2_ENDPOINT"),
		},
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

func envString(key, fallback string) string {
	if s := os.Getenv(key); s != "" {
		return s
	}
	return fallback
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
