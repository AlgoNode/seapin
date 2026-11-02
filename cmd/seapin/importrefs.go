package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"seapin/internal/contract"
	"seapin/internal/rmq"
)

// runImportRefs publishes references from an outside list to the "seapin"
// fanout exchange, so every region queues the fetches: a one-off import of
// objects the chain no longer points at (destroyed assets listed in
// x_asset_meta, 2026-11-02). Input: TSV, optionally gzipped, with columns
// asset, class, cid, subpath. Each line becomes a backfill ref
// "<chain>/<asset>/import/<class>" by creator -creator, timestamped now (so an
// object's first-seen date never moves back). Reads RMQ_URL.
func runImportRefs(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("import-refs", flag.ContinueOnError)
	file := fs.String("file", "", "TSV (.gz) with asset, class, cid, subpath")
	chain := fs.String("chain", "mainnet-v1.0", "chain genesis id")
	creator := fs.String("creator", "x_asset_meta:destroyed", "fair-share key the fetches are scheduled under")
	skip := fs.Int("skip", 0, "skip this many input lines (resume: the line a previous run logged)")
	rate := fs.Int("rate", 100, "refs per second at most")
	queues := fs.String("queues", "seapin.eu,seapin.us", "region queues to watch for depth")
	maxDepth := fs.Int("max-depth", 20_000, "pause while any watched queue holds more messages than this")
	batch := fs.Int("batch", 500, "refs per confirm round")
	dry := fs.Bool("dry-run", false, "parse and validate only")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" || *rate < 1 || *batch < 1 {
		fs.Usage()
		return errors.New("-file is required")
	}
	f, err := os.Open(*file)
	if err != nil {
		return err
	}
	defer f.Close()
	var in io.Reader = f
	if strings.HasSuffix(*file, ".gz") {
		g, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		in = g
	}

	var ch *amqp.Channel
	watch := strings.FieldsFunc(*queues, func(r rune) bool { return r == ',' })
	if !*dry {
		conn, err := rmq.Dial(os.Getenv("RMQ_URL"), "seapin import-refs")
		if err != nil {
			return err
		}
		defer conn.Close()
		if ch, err = conn.Channel(); err != nil {
			return err
		}
		for _, q := range watch {
			if _, err := ch.QueueDeclarePassive(q, true, false, false, false, nil); err != nil {
				return fmt.Errorf("queue %s: %w", q, err)
			}
		}
		if err := ch.Confirm(false); err != nil {
			return err
		}
	}

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	line, sent, invalid := 0, 0, 0
	confirmed := *skip // input lines whose refs the broker has all taken: resume with -skip confirmed
	start, lastLog := time.Now(), time.Now()
	report := func(msg string) {
		log.Info(msg, "confirmed_line", confirmed, "sent", sent, "invalid", invalid,
			"per_s", fmt.Sprintf("%.0f", float64(sent)/max(time.Since(start).Seconds(), 1)), "dry_run", *dry)
	}
	var page []contract.Ref
	flush := func() error {
		if len(page) == 0 || *dry {
			page = page[:0]
			confirmed = line
			return nil
		}
		for _, q := range watch {
			if err := waitDepth(ctx, ch, q, *maxDepth, log); err != nil {
				return err
			}
		}
		t0 := time.Now()
		confirms := make([]*amqp.DeferredConfirmation, 0, len(page))
		for _, r := range page {
			body, err := json.Marshal(r)
			if err != nil {
				return err
			}
			dc, err := ch.PublishWithDeferredConfirmWithContext(ctx, contract.ExchangeRefs, "", false, false, amqp.Publishing{
				ContentType: "application/json", DeliveryMode: amqp.Persistent, Body: body,
			})
			if err != nil {
				return err
			}
			confirms = append(confirms, dc)
		}
		for _, dc := range confirms {
			ok, err := dc.WaitContext(ctx)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("broker refused a ref")
			}
		}
		page = page[:0]
		confirmed = line
		if wait := time.Duration(len(confirms))*time.Second/time.Duration(*rate) - time.Since(t0); wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		return nil
	}
	for sc.Scan() {
		line++
		if line <= *skip {
			continue
		}
		col := strings.Split(sc.Text(), "\t")
		r, err := importRef(col, *chain, *creator)
		if err != nil {
			invalid++
			log.Warn("skipped line", "line", line, "err", err)
			continue
		}
		page = append(page, r)
		sent++
		if len(page) >= *batch {
			if err := flush(); err != nil {
				report("stopped")
				return err
			}
		}
		if time.Since(lastLog) > 30*time.Second {
			report("progress")
			lastLog = time.Now()
		}
	}
	if err := sc.Err(); err != nil {
		report("stopped")
		return err
	}
	if err := flush(); err != nil {
		report("stopped")
		return err
	}
	report("done")
	return nil
}

func importRef(col []string, chain, creator string) (contract.Ref, error) {
	if len(col) < 3 {
		return contract.Ref{}, errors.New("want asset, class, cid[, subpath]")
	}
	asset, err := strconv.ParseUint(col[0], 10, 64)
	if err != nil {
		return contract.Ref{}, err
	}
	class := col[1]
	if class == "" {
		class = "none"
	}
	sub := ""
	if len(col) > 3 {
		sub = col[3]
	}
	r := contract.NewRef(contract.Ref{
		Chain: chain, Asset: asset, Path: contract.PathImportPrefix + class, CID: col[2], Subpath: sub,
		Creator: creator, Src: contract.SrcBackfill, Round: 1, BlockTime: time.Now().UTC(),
	})
	return r, r.Validate()
}
