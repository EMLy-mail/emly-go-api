package config

import (
	"os"
	"strings"
)

type Config struct {
	Port    string
	DSN     string
	APIKeys []string
}

func Load() *Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	raw := os.Getenv("API_KEYS")
	var keys []string
	for _, k := range strings.Split(raw, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			keys = append(keys, k)
		}
	}

	return &Config{
		Port:    port,
		DSN:     os.Getenv("DB_DSN"),
		APIKeys: keys,
	}
}
