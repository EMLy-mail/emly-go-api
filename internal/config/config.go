package config

import (
	"os"
	"strings"
)

type Config struct {
	Port     string
	DSN      string
	APIKey   string
	AdminKey string
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

	return &Config{
		Port:     port,
		DSN:      os.Getenv("DB_DSN"),
		APIKey:   apiKey,
		AdminKey: adminKey,
	}
}
