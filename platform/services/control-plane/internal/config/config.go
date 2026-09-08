// Package config loads control-plane configuration from the environment.
package config

import (
	"os"
	"strconv"
)

// Config carries every runtime knob. Sensible defaults allow local boot.
type Config struct {
	Bind          string // HTTP bind address
	DatabaseURL   string // PostgreSQL DSN
	EngineURL     string // engine-service base URL
	AllowedOrigin string // CORS allowed origin (empty = reflect request)
	BodyLimitMB   int64  // max JSON body size
	JWTSecret     string // session token secret
}

// Load reads the environment with defaults.
func Load() Config {
	return Config{
		Bind:          getenv("CONTROL_PLANE_BIND", "0.0.0.0:8080"),
		DatabaseURL:   getenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/platform?sslmode=disable"),
		EngineURL:     getenv("ENGINE_URL", "http://127.0.0.1:8081"),
		AllowedOrigin: getenv("ALLOWED_ORIGIN", ""),
		BodyLimitMB:   int64getenv("BODY_LIMIT_MB", 10),
		JWTSecret:     getenv("SESSION_SECRET", "dev-secret-change-me"),
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func int64getenv(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
