// Package config reads seapin's settings from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// S3 is the bucket seapin keeps objects in (the region's SeaweedFS S3 gateway).
type S3 struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	UseSSL    bool
}

type Config struct {
	ListenAddr      string
	S3              S3
	PGDSN           string
	MissMaxAge      time.Duration // Cache-Control max-age of a 404, so the CDN asks again soon
	TakedownRefresh time.Duration
}

// Load reads the environment. Endpoints and credentials have no defaults:
// every missing one is reported in a single error.
func Load() (Config, error) {
	var missing []string
	need := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}
	c := Config{
		ListenAddr: ListenAddr(),
		S3: S3{
			Endpoint:  need("S3_ENDPOINT"),
			Bucket:    envOr("S3_BUCKET", "ipfs"),
			AccessKey: need("S3_ACCESS_KEY"),
			SecretKey: need("S3_SECRET_KEY"),
		},
		PGDSN: need("PG_DSN"),
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing required environment: %s", strings.Join(missing, ", "))
	}
	var err error
	if c.S3.UseSSL, err = strconv.ParseBool(envOr("S3_USE_SSL", "false")); err != nil {
		return c, fmt.Errorf("S3_USE_SSL: %w", err)
	}
	if c.MissMaxAge, err = time.ParseDuration(envOr("MISS_MAX_AGE", "60s")); err != nil {
		return c, fmt.Errorf("MISS_MAX_AGE: %w", err)
	}
	if c.TakedownRefresh, err = time.ParseDuration(envOr("TAKEDOWN_REFRESH", "60s")); err != nil {
		return c, fmt.Errorf("TAKEDOWN_REFRESH: %w", err)
	}
	return c, nil
}

// ListenAddr is the HTTP listen address; it binds to loopback unless told otherwise.
func ListenAddr() string {
	return envOr("LISTEN_ADDR", "127.0.0.1:8090")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
