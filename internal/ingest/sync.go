package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"seapin/internal/catalog"
)

// SyncCreator is the fair-share key of fetches anti-entropy queues.
const SyncCreator = "peer-sync"

// Syncer is the anti-entropy pull (PLAN §2, M5): it pages through the peer
// region's catalog and queues whatever this region lacks, so lost queue
// messages, outages and bugs heal within a day, and a new or rebuilt region
// fills itself from the peer. Catalogs only grow, so this is a set union.
type Syncer struct {
	PeerURL  string // the peer's internal listener, e.g. http://62.83.39.60:8098
	Token    string
	Client   *http.Client
	Catalog  *catalog.Catalog
	Interval time.Duration // between runs (daily)
	Overlap  time.Duration // each run re-reads this much before the last watermark
	Page     int
	Log      *slog.Logger

	items   *prometheus.CounterVec
	lastOK  prometheus.Gauge
	errors_ prometheus.Counter
}

// SyncStats counts one run.
type SyncStats struct {
	Refs, RefsApplied, Objects, ObjectsQueued, Aliases, AliasesQueued, Takedowns, Invalid int
}

func (s *Syncer) Register(reg prometheus.Registerer) {
	s.items = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "seapin_sync_items_total", Help: "Anti-entropy: peer catalog items read and acted on, by kind and result (seen, queued, invalid).",
	}, []string{"kind", "result"})
	s.lastOK = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "seapin_sync_last_success_timestamp_seconds", Help: "Anti-entropy: end of the last complete run (unix time).",
	})
	s.errors_ = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "seapin_sync_errors_total", Help: "Anti-entropy runs that stopped on an error.",
	})
	reg.MustRegister(s.items, s.lastOK, s.errors_)
}

// Run syncs a minute after start, then every Interval, until ctx ends.
func (s *Syncer) Run(ctx context.Context) {
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		st, err := s.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			s.errors_.Inc()
			s.Log.Error("anti-entropy", "err", err)
			wait = 30 * time.Minute // retry sooner than a full interval
			continue
		}
		s.Log.Info("anti-entropy done", "refs", st.Refs, "refs_applied", st.RefsApplied, "objects", st.Objects,
			"objects_queued", st.ObjectsQueued, "aliases", st.Aliases, "aliases_queued", st.AliasesQueued,
			"takedowns", st.Takedowns, "invalid", st.Invalid)
		wait = s.Interval
	}
}

// RunOnce pulls every kind once.
func (s *Syncer) RunOnce(ctx context.Context) (SyncStats, error) {
	var st SyncStats
	if err := s.syncRefs(ctx, &st); err != nil {
		return st, fmt.Errorf("refs: %w", err)
	}
	if err := s.syncObjects(ctx, &st); err != nil {
		return st, fmt.Errorf("objects: %w", err)
	}
	if err := s.syncAliases(ctx, &st); err != nil {
		return st, fmt.Errorf("aliases: %w", err)
	}
	if err := s.syncTakedowns(ctx, &st); err != nil {
		return st, fmt.Errorf("takedowns: %w", err)
	}
	if s.lastOK != nil {
		s.lastOK.SetToCurrentTime()
	}
	return st, nil
}

func (s *Syncer) count(kind, result string, n int) {
	if s.items != nil && n > 0 {
		s.items.WithLabelValues(kind, result).Add(float64(n))
	}
}

func (s *Syncer) get(ctx context.Context, kind string, q url.Values, out any) error {
	q.Set("limit", strconv.Itoa(s.Page))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.PeerURL+"/internal/catalog/"+kind+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer %s: http %d", kind, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// start is where a run begins: the saved watermark minus the overlap.
func (s *Syncer) start(ctx context.Context, kind string) (time.Time, error) {
	since, _, err := s.Catalog.SyncState(ctx, kind)
	if err != nil {
		return since, err
	}
	if since.After(time.Unix(0, 0)) {
		since = since.Add(-s.Overlap)
	}
	return since, nil
}

func (s *Syncer) syncRefs(ctx context.Context, st *SyncStats) error {
	since, err := s.start(ctx, "refs")
	if err != nil {
		return err
	}
	after := ""
	for {
		var page []catalog.SyncRef
		if err := s.get(ctx, "refs", url.Values{"since": {since.Format(time.RFC3339Nano)}, "after": {after}}, &page); err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		st.Refs += len(page)
		s.count("refs", "seen", len(page))
		stale, err := s.Catalog.StaleRefs(ctx, page)
		if err != nil {
			return err
		}
		for _, r := range stale {
			ref := r.Ref
			if err := ref.Validate(); err != nil {
				st.Invalid++
				s.count("refs", "invalid", 1)
				continue
			}
			if _, err := s.Catalog.ApplyRef(ctx, ref); err != nil {
				return err
			}
			st.RefsApplied++
			s.count("refs", "queued", 1)
		}
		last := page[len(page)-1]
		since, after = last.UpdatedAt, last.Key
		if err := s.Catalog.SaveSyncState(ctx, "refs", since, after); err != nil {
			return err
		}
	}
}

func (s *Syncer) syncObjects(ctx context.Context, st *SyncStats) error {
	since, err := s.start(ctx, "objects")
	if err != nil {
		return err
	}
	after := ""
	for {
		var page []catalog.SyncObject
		if err := s.get(ctx, "objects", url.Values{"since": {since.Format(time.RFC3339Nano)}, "after": {after}}, &page); err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		st.Objects += len(page)
		s.count("objects", "seen", len(page))
		cids := make([]string, len(page))
		first := map[string]time.Time{}
		for i, o := range page {
			cids[i], first[o.CID] = o.CID, o.FirstSeen
		}
		missing, err := s.Catalog.MissingObjects(ctx, cids)
		if err != nil {
			return err
		}
		queued := 0
		for _, c := range missing {
			n, err := s.Catalog.EnqueueFetch(ctx, c, "", SyncCreator, first[c])
			if err != nil {
				return err
			}
			queued += int(n)
		}
		st.ObjectsQueued += queued
		s.count("objects", "queued", queued)
		last := page[len(page)-1]
		since, after = last.CachedAt, last.CID
		if err := s.Catalog.SaveSyncState(ctx, "objects", since, after); err != nil {
			return err
		}
	}
}

// syncAliases walks the peer's aliases in full (they carry no timestamp; a few hundred thousand rows).
func (s *Syncer) syncAliases(ctx context.Context, st *SyncStats) error {
	afterRoot, afterSub := "", ""
	for {
		var page []catalog.SyncAlias
		if err := s.get(ctx, "aliases", url.Values{"after": {afterRoot}, "after_subpath": {afterSub}}, &page); err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		st.Aliases += len(page)
		s.count("aliases", "seen", len(page))
		missing, err := s.Catalog.MissingAliases(ctx, page)
		if err != nil {
			return err
		}
		queued := 0
		for _, a := range missing {
			n, err := s.Catalog.EnqueueFetch(ctx, a.Root, a.Subpath, SyncCreator, time.Now())
			if err != nil {
				return err
			}
			queued += int(n)
		}
		st.AliasesQueued += queued
		s.count("aliases", "queued", queued)
		last := page[len(page)-1]
		afterRoot, afterSub = last.Root, last.Subpath
	}
}

func (s *Syncer) syncTakedowns(ctx context.Context, st *SyncStats) error {
	var peer map[string]string
	if err := s.get(ctx, "takedowns", url.Values{}, &peer); err != nil {
		return err
	}
	local, err := s.Catalog.TakedownList(ctx)
	if err != nil {
		return err
	}
	for cid, reason := range peer {
		if _, ok := local[cid]; ok {
			continue
		}
		if err := s.Catalog.AddTakedown(ctx, cid, reason); err != nil {
			return err
		}
		st.Takedowns++
		s.count("takedowns", "queued", 1)
	}
	return nil
}
