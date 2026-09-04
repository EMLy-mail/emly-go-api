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

// S3BucketConfig holds the connection details for one S3-compatible bucket.
// AccountID is a Cloudflare R2 convenience: when Endpoint is empty it is used
// to derive the R2 endpoint (https://<AccountID>.r2.cloudflarestorage.com).
// Any other S3-compatible provider should set Endpoint explicitly and leave
// AccountID blank. Two S3BucketConfig values can point at entirely different
// hosts/services/providers.
type S3BucketConfig struct {
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
	Port                    string
	DSN                     string
	Database                string
	APIKey                  string
	AdminKey                string
	DashboardKey            string
	LogLevel                string
	MaxOpenConns            int
	MaxIdleConns            int
	ConnMaxLifetime         int
	UpdatesEnabled          bool
	UpdatesS3Prefix         string
	UpdaterS3Prefix         string
	ConfigUpstreamURL       string
	ConfigUpstreamInterval  time.Duration
	ConfigUpstreamAPIKey    string
	StatsStreamTickInterval time.Duration
	UseS3APIFileStorage     bool
	UseS3UpdatesStorage     bool
	RateLimit               RateLimitConfig
	S3APIFile               S3BucketConfig
	S3Updates               S3BucketConfig
	Otel                    OtelConfig
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
		Port:            port,
		DSN:             os.Getenv("DB_DSN"),
		Database:        dbName,
		APIKey:          apiKey,
		AdminKey:        adminKey,
		DashboardKey:    os.Getenv("DASHBOARD_KEY"),
		LogLevel:        strings.ToLower(strings.TrimSpace(envString("LOG_LEVEL", "info"))),
		MaxOpenConns:    maxOpenConns,
		MaxIdleConns:    maxIdleConns,
		ConnMaxLifetime: connMaxLifetime,
		UpdatesEnabled:  strings.ToLower(strings.TrimSpace(os.Getenv("UPDATES_ENABLED"))) == "true",
		UpdatesS3Prefix: strings.Trim(os.Getenv("S3_UPDATES_PREFIX"), "/"),
		UpdaterS3Prefix: strings.Trim(envString("S3_UPDATER_PREFIX", "updater"), "/"),
		// ConfigUpstreamURL, empty on the cloud instance, marks this instance
		// as a site mirror for /v2/config: its admin routes refuse writes and
		// a background loop replicates the published document from upstream
		// instead (API design doc §9). ConfigUpstreamAPIKey defaults to this
		// instance's own API_KEY, since a mirror's upstream fetch typically
		// authenticates with the same shared secret its own clients use.
		ConfigUpstreamURL:      strings.TrimRight(strings.TrimSpace(os.Getenv("CONFIG_UPSTREAM_URL")), "/"),
		ConfigUpstreamInterval: envDuration("CONFIG_UPSTREAM_INTERVAL", 5*time.Minute),
		ConfigUpstreamAPIKey:   envString("CONFIG_UPSTREAM_API_KEY", apiKey),
		// StatsStreamTickInterval is the periodic resync tick for
		// GET /v2/stats/stream (design doc §6.2/§7): independent of any
		// updater_events ingest, it keeps time-derived fields like
		// connected_clients fresh on a connection that's been open a while.
		StatsStreamTickInterval: envDuration("STATS_STREAM_TICK_INTERVAL", 30*time.Second),
		UseS3APIFileStorage:     strings.ToLower(strings.TrimSpace(os.Getenv("USE_S3_API_FILE_STORAGE"))) == "true",
		UseS3UpdatesStorage:     strings.ToLower(strings.TrimSpace(os.Getenv("USE_S3_UPDATES_STORAGE"))) == "true",
		Otel: OtelConfig{
			Enabled:  strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_ENABLED"))) == "true",
			Endpoint: envString("OTEL_ENDPOINT", "http://localhost:4318"),
		},
		S3APIFile: S3BucketConfig{
			AccountID:       os.Getenv("S3_API_FILE_ACCOUNT_ID"),
			AccessKeyID:     os.Getenv("S3_API_FILE_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("S3_API_FILE_SECRET_ACCESS_KEY"),
			BucketName:      os.Getenv("S3_API_FILE_BUCKET"),
			Region:          envString("S3_API_FILE_REGION", "auto"),
			Endpoint:        os.Getenv("S3_API_FILE_ENDPOINT"),
		},
		S3Updates: S3BucketConfig{
			AccountID:       os.Getenv("S3_UPDATES_ACCOUNT_ID"),
			AccessKeyID:     os.Getenv("S3_UPDATES_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("S3_UPDATES_SECRET_ACCESS_KEY"),
			BucketName:      os.Getenv("S3_UPDATES_BUCKET"),
			Region:          envString("S3_UPDATES_REGION", "auto"),
			Endpoint:        os.Getenv("S3_UPDATES_ENDPOINT"),
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
