package catalog

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"seapin/internal/contract"
)

func ref(asset uint64, path, cid, sub, creator, src string, round uint64) contract.Ref {
	return contract.NewRef(contract.Ref{
		Chain: "mainnet-v1.0", Asset: asset, Path: path, CID: cid, Subpath: sub,
		Creator: creator, Src: src, Round: round, BlockTime: time.Unix(1_700_000_000, 0).UTC(),
	})
}

func claimAll(t *testing.T, c *Catalog, lane string, n int, exclude []string, fair bool) []Job {
	t.Helper()
	jobs, err := c.Claim(context.Background(), lane, n, exclude, time.Minute, fair)
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func roots(jobs []Job) string {
	var r []string
	for _, j := range jobs {
		r = append(r, j.Root+"/"+j.Subpath+"@"+j.Creator)
	}
	sort.Strings(r)
	return strings.Join(r, " ")
}

func TestIngestQueue(t *testing.T) {
	c := openTest(t)
	ctx := context.Background()
	if err := c.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// Newest round per key wins; an older ref changes nothing and queues nothing.
	if n, err := c.ApplyRef(ctx, ref(1, "url", "bafy-json", "", "A", "live", 100)); err != nil || n != 1 {
		t.Fatalf("first ref: %d %v", n, err)
	}
	if n, err := c.ApplyRef(ctx, ref(1, "url", "bafy-old", "", "A", "live", 50)); err != nil || n != 0 {
		t.Fatalf("older ref: %d %v", n, err)
	}
	var root string
	c.pool.QueryRow(ctx, `SELECT root_cid FROM refs WHERE key = 'mainnet-v1.0/1/url'`).Scan(&root)
	if root != "bafy-json" {
		t.Fatalf("older round overwrote the ref: %s", root)
	}

	// Fair claims: one per creator per call; a capped creator is skipped.
	for i := range 5 {
		c.ApplyRef(ctx, ref(uint64(10+i), "url", fmt.Sprintf("bafy-a%d", i), "", "A", "live", 100))
	}
	c.ApplyRef(ctx, ref(20, "url", "bafy-b0", "", "B", "live", 100))
	if got := roots(claimAll(t, c, LaneFast, 10, nil, true)); got != "bafy-b0/@B bafy-json/@A" {
		t.Fatalf("fair claim: %s", got)
	}
	if got := claimAll(t, c, LaneFast, 10, []string{"A"}, true); len(got) != 0 {
		t.Fatalf("excluded creator claimed: %s", roots(got))
	}
	if got := claimAll(t, c, LaneFast, 10, nil, false); len(got) != 5 {
		t.Fatalf("unfair claim should take all of A's: %s", roots(got))
	}

	// Completing a JSON object records its links and queues the children at depth 1.
	jsonJob := Job{Root: "bafy-json", Lane: LaneFast, Creator: "A", FirstSeen: time.Unix(1_700_000_000, 0)}
	n, err := c.Complete(ctx, jsonJob, Outcome{
		Leaf: "bafy-json", Status: StatusCached, Mime: "application/json", Size: 100, Source: "kubo",
		Links: []Link{{Path: "image", CID: "bafk-png"}, {Path: "properties/file_url", CID: "bafy-dir", Subpath: "1.png"}},
	})
	if err != nil || n != 2 {
		t.Fatalf("complete json: %d %v", n, err)
	}
	children := claimAll(t, c, LaneFast, 10, nil, false)
	if got := roots(children); got != "bafk-png/@A bafy-dir/1.png@A" {
		t.Fatalf("children: %s", got)
	}
	for _, j := range children {
		if j.Depth != 1 {
			t.Errorf("child %s depth %d", j.Root, j.Depth)
		}
	}
	dirJob := children[1]
	if dirJob.Root != "bafy-dir" {
		dirJob = children[0]
	}
	if _, err := c.Complete(ctx, dirJob, Outcome{Leaf: "bafk-leaf", Status: StatusCached, Mime: "image/png", Size: 10, Source: "kubo"}); err != nil {
		t.Fatal(err)
	}
	objs, err := c.AssetObjects(ctx, "mainnet-v1.0", 1)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range objs {
		keys = append(keys, fmt.Sprintf("%s=%s:%s", strings.TrimPrefix(o.Key, "mainnet-v1.0/1/"), o.Leaf, o.Status))
	}
	if got, want := strings.Join(keys, " "), "url=bafy-json:cached url/image=bafk-png: url/properties/file_url=bafk-leaf:cached"; got != want {
		t.Fatalf("asset_objects:\n got  %s\n want %s", got, want)
	}

	// The reverse: from a leaf behind a directory path in the JSON, and from the JSON itself.
	for cid, want := range map[string]string{
		"bafk-leaf": "1 mainnet-v1.0/1/url/properties/file_url 1",
		"bafy-json": "1 mainnet-v1.0/1/url 0",
		"bafk-none": "",
	} {
		as, err := c.CIDAssets(ctx, "mainnet-v1.0", cid, 10)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, a := range as {
			got = append(got, fmt.Sprintf("%d %s %d", a.Asset, a.Key, a.Depth))
		}
		if strings.Join(got, ", ") != want {
			t.Errorf("cid_assets(%s): got %q, want %q", cid, strings.Join(got, ", "), want)
		}
	}

	// A second asset pointing at the cached JSON queues nothing new: the
	// children are pending or known already.
	if n, _ := c.ApplyRef(ctx, ref(2, "url", "bafy-json", "", "C", "live", 100)); n != 0 {
		t.Errorf("known JSON re-queued %d children", n)
	}

	// Depth bound: a chain of JSON objects stops after MaxDepth hops.
	c.ApplyRef(ctx, ref(3, "url", "bafy-c0", "", "D", "live", 100))
	for d := 0; d <= MaxDepth; d++ {
		jobs := claimAll(t, c, LaneFast, 10, nil, false)
		if d == MaxDepth {
			break
		}
		if len(jobs) != 1 || jobs[0].Root != fmt.Sprintf("bafy-c%d", d) {
			t.Fatalf("depth %d: %s", d, roots(jobs))
		}
		next := fmt.Sprintf("bafy-c%d", d+1)
		c.Complete(ctx, jobs[0], Outcome{Leaf: jobs[0].Root, Status: StatusCached, Mime: "application/json", Links: []Link{{Path: "next", CID: next}}})
	}
	var deepest int
	c.pool.QueryRow(ctx, `SELECT max(depth) FROM fetches WHERE root_cid LIKE 'bafy-c%'`).Scan(&deepest)
	if deepest != MaxDepth {
		t.Errorf("deepest queued fetch has depth %d, want %d", deepest, MaxDepth)
	}

	// A failed fetch is re-armed by a new reference, but only once per 30 days.
	c.ApplyRef(ctx, ref(4, "url", "bafy-dead", "", "E", "live", 100))
	dead := claimAll(t, c, LaneFast, 10, nil, false)[0]
	if err := c.Fail(ctx, dead, "gone"); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.ApplyRef(ctx, ref(5, "url", "bafy-dead", "", "E", "live", 100)); n != 1 {
		t.Errorf("first re-arm: %d", n)
	}
	dead = claimAll(t, c, LaneFast, 10, nil, false)[0]
	c.Fail(ctx, dead, "gone")
	if n, _ := c.ApplyRef(ctx, ref(6, "url", "bafy-dead", "", "E", "live", 100)); n != 0 {
		t.Errorf("second re-arm within 30 days: %d", n)
	}

	// Live demand promotes a waiting backfill fetch to the fast lane.
	c.ApplyRef(ctx, ref(7, "url", "bafy-old-nft", "", "F", "backfill", 100))
	c.ApplyRef(ctx, ref(8, "url", "bafy-old-nft", "", "F", "live", 100))
	var lane string
	c.pool.QueryRow(ctx, `SELECT lane FROM fetches WHERE root_cid = 'bafy-old-nft'`).Scan(&lane)
	if lane != LaneFast {
		t.Errorf("backfill fetch not promoted: %s", lane)
	}

	// Retry moves a fetch to another lane and time.
	c.ApplyRef(ctx, ref(9, "url", "bafy-retry", "", "G", "live", 100))
	r := claimAll(t, c, LaneFast, 10, nil, false)
	var retry Job
	for _, j := range r {
		if j.Root == "bafy-retry" {
			retry = j
		}
	}
	if err := c.Retry(ctx, retry, LaneSlow, time.Now().Add(-time.Second), "timeout"); err != nil {
		t.Fatal(err)
	}
	if got := claimAll(t, c, LaneSlow, 10, nil, true); len(got) != 1 || got[0].Attempts != 2 {
		t.Errorf("slow lane claim: %+v", got)
	}
	stats, err := c.LaneStats(ctx)
	if err != nil || stats[LaneSlow].Pending != 1 {
		t.Errorf("lane stats: %+v %v", stats, err)
	}
}

func TestDemand(t *testing.T) {
	c := openTest(t)
	ctx := context.Background()
	if err := c.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 4)
	lctx, stop := context.WithCancel(ctx)
	defer stop()
	go c.Notifications(lctx, func(lane string) { got <- lane })
	time.Sleep(200 * time.Millisecond) // LISTEN is in place

	if st, err := c.FetchState(ctx, "bafy-u", ""); err != nil || st.Found {
		t.Fatalf("unknown fetch: %+v %v", st, err)
	}
	if err := c.Demand(ctx, "bafy-u", "", "site:nft.example"); err != nil {
		t.Fatal(err)
	}
	select {
	case lane := <-got:
		if lane != LaneDemand {
			t.Errorf("notified lane %q", lane)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no NOTIFY")
	}
	jobs := claimAll(t, c, LaneDemand, 10, nil, true)
	if len(jobs) != 1 || jobs[0].Creator != "site:nft.example" || jobs[0].Depth != MaxDepth {
		t.Fatalf("demand job %+v", jobs)
	}
	// Its links are recorded but not followed: depth is already MaxDepth.
	n, err := c.Complete(ctx, jobs[0], Outcome{Leaf: "bafy-u", Status: StatusCached, Mime: "application/json",
		Links: []Link{{Path: "image", CID: "bafk-child"}}})
	if err != nil || n != 0 {
		t.Errorf("demand JSON queued %d children (%v)", n, err)
	}
	if st, _ := c.FetchState(ctx, "bafy-u", ""); st.Status != "done" || st.Leaf != "bafy-u" {
		t.Errorf("state after completion %+v", st)
	}

	// A waiting backfill fetch moves to the demand lane.
	c.ApplyRef(ctx, ref(50, "url", "bafy-bf", "", "Z", "backfill", 1))
	if err := c.Demand(ctx, "bafy-bf", "", "site:x"); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.FetchState(ctx, "bafy-bf", ""); st.Lane != LaneDemand {
		t.Errorf("backfill fetch not promoted: %+v", st)
	}
}

const rawCID = "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"

func TestRefsBefore(t *testing.T) {
	c := openTest(t)
	ctx := context.Background()
	if err := c.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		c.ApplyRef(ctx, ref(uint64(100+i), "url", rawCID, "", "A", "live", 100))
	}
	// Written after the cutoff: excluded.
	c.pool.Exec(ctx, `UPDATE refs SET updated_at = now() + interval '1 hour' WHERE asset = 104`)
	cut := time.Now().Add(time.Minute)

	page, err := c.RefsBefore(ctx, cut, "", 2)
	if err != nil || len(page) != 2 || page[0].Asset != 100 || page[1].Asset != 101 {
		t.Fatalf("first page: %+v %v", page, err)
	}
	for _, r := range page {
		if r.Src != contract.SrcBackfill || r.CID != rawCID || r.Creator != "A" || r.Round != 100 {
			t.Fatalf("ref fields: %+v", r)
		}
		if err := r.Validate(); err != nil {
			t.Fatalf("republished ref invalid: %v", err)
		}
	}
	rest, err := c.RefsBefore(ctx, cut, page[1].Key, 10)
	if err != nil || len(rest) != 2 || rest[0].Asset != 102 || rest[1].Asset != 103 {
		t.Fatalf("resumed page: %+v %v", rest, err)
	}
}
