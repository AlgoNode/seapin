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
	// The internal listener (other region's fetches, the scanner's asset
	// reads) is off unless INTERNAL_ADDR is set; it then needs INTERNAL_TOKEN.
	InternalAddr  string
	InternalToken string
	// Read-through (D2): openresty sets DemandHeader to "1" on requests whose
	// referer is whitelisted; a miss on those queues a fetch and waits for it.
	DemandHeader     string
	DemandWait       time.Duration
	DemandMaxWaiters int
}

// Ingest holds what `seapin ingest` needs on top of Config.
type Ingest struct {
	Region      string // eu | us; the queue is seapin.<region>
	RMQURL      string
	QueueType   string // "" (classic) or "quorum"
	QueueMaxLen int64
	Prefetch    int
	Addr        string // /health and /metrics
	SidecarURL  string // IPFS gateway sidecar (rainbow, D1)
	LegacyURL   string // old gateway caches, comma-separated, only-if-cached; migration only (M4–M6)
	PeerURL     string // the other region's internal listener; optional
	PeerToken   string
	PinataURL   string // optional last source, slow lanes only
	PinataToken string

	FastSlots, SlowSlots, BackfillSlots, DemandSlots        int
	FastTTFB, SlowTTFB, BackfillTTFB, RetryTTFB, DemandTTFB time.Duration
}

// LoadIngest reads Config plus the ingest settings.
func LoadIngest() (Config, Ingest, error) {
	c, err := Load()
	if err != nil {
		return c, Ingest{}, err
	}
	p := parser{}
	in := Ingest{
		Region:      p.need("REGION"),
		RMQURL:      p.need("RMQ_URL"),
		QueueType:   os.Getenv("QUEUE_TYPE"),
		QueueMaxLen: int64(p.int("QUEUE_MAX_LENGTH", 10_000_000)),
		Prefetch:    p.int("PREFETCH", 256),
		Addr:        envOr("INGEST_ADDR", "127.0.0.1:8091"),
		SidecarURL:  envOr("SIDECAR_URL", "http://127.0.0.1:8090"),
		LegacyURL:   os.Getenv("LEGACY_URL"),
		PeerURL:     os.Getenv("PEER_URL"),
		PeerToken:   os.Getenv("PEER_TOKEN"),
		PinataURL:   os.Getenv("PINATA_URL"),
		PinataToken: os.Getenv("PINATA_TOKEN"),

		FastSlots:     p.int("FAST_SLOTS", 48),
		SlowSlots:     p.int("SLOW_SLOTS", 16),
		BackfillSlots: p.int("BACKFILL_SLOTS", 32),
		FastTTFB:      p.dur("FAST_TTFB", 10*time.Second),
		SlowTTFB:      p.dur("SLOW_TTFB", 120*time.Second),
		BackfillTTFB:  p.dur("BACKFILL_TTFB", 10*time.Second),
		RetryTTFB:     p.dur("BACKFILL_RETRY_TTFB", 120*time.Second),
		DemandSlots:   p.int("DEMAND_SLOTS", 32),
		DemandTTFB:    p.dur("DEMAND_TTFB", 10*time.Second),
	}
	if in.PeerURL != "" && in.PeerToken == "" {
		p.errs = append(p.errs, "PEER_URL needs PEER_TOKEN")
	}
	if in.PinataURL != "" && in.PinataToken == "" {
		p.errs = append(p.errs, "PINATA_URL needs PINATA_TOKEN")
	}
	return c, in, p.err()
}

// parser collects every problem with the environment into one error.
type parser struct{ errs []string }

func (p *parser) need(key string) string {
	v := os.Getenv(key)
	if v == "" {
		p.errs = append(p.errs, "missing "+key)
	}
	return v
}

func (p *parser) int(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		p.errs = append(p.errs, key+": not a non-negative integer")
	}
	return n
}

func (p *parser) dur(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		p.errs = append(p.errs, key+": "+err.Error())
	}
	return d
}

func (p *parser) err() error {
	if len(p.errs) == 0 {
		return nil
	}
	return fmt.Errorf("environment: %s", strings.Join(p.errs, "; "))
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
	c.InternalAddr, c.InternalToken = os.Getenv("INTERNAL_ADDR"), os.Getenv("INTERNAL_TOKEN")
	c.DemandHeader = envOr("DEMAND_HEADER", "X-Seapin-Whitelisted")
	p := parser{}
	c.DemandWait = p.dur("DEMAND_WAIT", 20*time.Second)
	c.DemandMaxWaiters = p.int("DEMAND_MAX_WAITERS", 256)
	if err := p.err(); err != nil {
		return c, err
	}
	if c.InternalAddr != "" && len(c.InternalToken) < 32 {
		return c, fmt.Errorf("INTERNAL_ADDR needs an INTERNAL_TOKEN of at least 32 characters")
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

// Scan holds what seapin-scan needs.
type Scan struct {
	Chain          string // genesis id, e.g. mainnet-v1.0
	AlgodURL       string // live blocks and asset lookups
	AlgodToken     string
	AlgodReferer   string // optional Referer header on algod requests
	BlocksURL      string // backfill blocks (blksrv); defaults to AlgodURL
	BlocksToken    string
	BlocksReferer  string
	RMQURL         string
	PGDSN          string // scanner state
	BackfillRounds uint64 // 0 = down to genesis
	Window         int
	Fetchers       int
	MemoryBudget   int64 // bytes of decoded blocks the backfill may hold ahead
	MaxRefsPerSec  int
	MaxLiveLag     uint64
	MetaDSN        string // database with x_asset_meta; empty = compat writer off
	SeapinURL      string // seapin's internal listener, for the compat writer
	SeapinToken    string
	Addr           string // /health and /metrics
}

func LoadScan() (Scan, error) {
	p := parser{}
	s := Scan{
		Chain:          envOr("CHAIN", "mainnet-v1.0"),
		AlgodURL:       p.need("ALGOD_URL"),
		AlgodToken:     os.Getenv("ALGOD_TOKEN"),
		AlgodReferer:   os.Getenv("ALGOD_REFERER"),
		BlocksURL:      os.Getenv("BLOCKS_URL"),
		BlocksToken:    os.Getenv("BLOCKS_TOKEN"),
		BlocksReferer:  os.Getenv("BLOCKS_REFERER"),
		RMQURL:         p.need("RMQ_URL"),
		PGDSN:          p.need("PG_DSN"),
		BackfillRounds: uint64(p.int("BACKFILL_ROUNDS", 0)),
		Window:         p.int("BACKFILL_WINDOW", 64),
		Fetchers:       p.int("BACKFILL_FETCHERS", 16),
		MemoryBudget:   int64(p.int("BACKFILL_MEMORY_MB", 256)) << 20,
		MaxRefsPerSec:  p.int("BACKFILL_MAX_REFS_PER_SEC", 200),
		MaxLiveLag:     uint64(p.int("MAX_LIVE_LAG", 10)),
		MetaDSN:        os.Getenv("META_PG_DSN"),
		SeapinURL:      os.Getenv("SEAPIN_INTERNAL_URL"),
		SeapinToken:    os.Getenv("SEAPIN_INTERNAL_TOKEN"),
		Addr:           envOr("SCAN_ADDR", "127.0.0.1:8093"),
	}
	if s.BlocksURL == "" {
		s.BlocksURL, s.BlocksToken, s.BlocksReferer = s.AlgodURL, s.AlgodToken, s.AlgodReferer
	}
	if s.MetaDSN != "" && (s.SeapinURL == "" || s.SeapinToken == "") {
		p.errs = append(p.errs, "META_PG_DSN needs SEAPIN_INTERNAL_URL and SEAPIN_INTERNAL_TOKEN")
	}
	if s.Window < 1 {
		p.errs = append(p.errs, "BACKFILL_WINDOW must be at least 1")
	}
	return s, p.err()
}
