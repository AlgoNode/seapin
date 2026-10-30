package serve

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Takedowns is an in-memory copy of the catalog's takedown list, so the hit
// path never waits on Postgres.
type Takedowns struct {
	m atomic.Pointer[map[string]string]
}

type takedownLoader func(context.Context) (map[string]string, error)

func (t *Takedowns) Reason(cid string) (string, bool) {
	m := t.m.Load()
	if m == nil {
		return "", false
	}
	r, ok := (*m)[cid]
	return r, ok
}

func (t *Takedowns) Set(m map[string]string) { t.m.Store(&m) }

// Load blocks until the first copy is loaded, retrying with backoff: serving
// before the list is known could hand out taken-down content.
func (t *Takedowns) Load(ctx context.Context, load takedownLoader, log *slog.Logger) error {
	wait := time.Second
	for {
		m, err := load(ctx)
		if err == nil {
			t.Set(m)
			return nil
		}
		log.Warn("loading takedowns, retrying", "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, 30*time.Second)
	}
}

// Refresh reloads the list every interval until ctx ends. A failed reload
// keeps the last good copy.
func (t *Takedowns) Refresh(ctx context.Context, load takedownLoader, every time.Duration, log *slog.Logger) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if m, err := load(ctx); err != nil {
			log.Warn("refreshing takedowns", "err", err)
		} else {
			t.Set(m)
		}
	}
}
