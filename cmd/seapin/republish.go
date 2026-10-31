package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"seapin/internal/catalog"
	"seapin/internal/contract"
	"seapin/internal/rmq"
)

// runRepublish sends this region's catalog references to one region queue
// through the direct exchange, as backfill refs. It catches up a region that
// started after refs were published (M5); ingest skips refs it already has.
// Reads PG_DSN (this region's catalog) and RMQ_URL.
func runRepublish(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("republish", flag.ContinueOnError)
	queue := fs.String("queue", "", "target region queue, e.g. seapin.us")
	beforeS := fs.String("before", "", "only refs last written before this time (RFC 3339), e.g. when the target queue was created")
	after := fs.String("after", "", "resume after this ref key (the last key a previous run logged)")
	rate := fs.Int("rate", 500, "refs per second at most")
	maxDepth := fs.Int("max-depth", 50_000, "pause while the target queue holds more messages than this (it drops its oldest beyond its max length)")
	batch := fs.Int("batch", 500, "refs per catalog page and per confirm round")
	dry := fs.Bool("dry-run", false, "count what would be sent, publish nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *queue == "" || *beforeS == "" || *rate < 1 || *batch < 1 {
		fs.Usage()
		return errors.New("-queue and -before are required")
	}
	before, err := time.Parse(time.RFC3339, *beforeS)
	if err != nil {
		return fmt.Errorf("-before: %w", err)
	}
	dsn, url := os.Getenv("PG_DSN"), os.Getenv("RMQ_URL")
	if dsn == "" || (url == "" && !*dry) {
		return errors.New("PG_DSN and RMQ_URL must be set")
	}
	cat, err := catalog.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer cat.Close()

	var ch *amqp.Channel
	if !*dry {
		conn, err := rmq.Dial(url, "seapin republish "+*queue)
		if err != nil {
			return err
		}
		defer conn.Close()
		if ch, err = conn.Channel(); err != nil {
			return err
		}
		// The queue must exist already (its region's ingest declares it with the
		// right arguments); a passive declare fails instead of creating it.
		if _, err := ch.QueueDeclarePassive(*queue, true, false, false, false, nil); err != nil {
			return fmt.Errorf("queue %s: %w", *queue, err)
		}
		if err := ch.ExchangeDeclare(contract.ExchangeDirect, "direct", true, false, false, false, nil); err != nil {
			return err
		}
		if err := ch.QueueBind(*queue, *queue, contract.ExchangeDirect, false, nil); err != nil {
			return err
		}
		if err := ch.Confirm(false); err != nil {
			return err
		}
	}

	var sent, invalid int
	last := *after
	start, lastLog := time.Now(), time.Now()
	report := func(msg string) {
		el := time.Since(start).Seconds()
		log.Info(msg, "queue", *queue, "sent", sent, "invalid", invalid, "last_key", last,
			"per_s", fmt.Sprintf("%.0f", float64(sent)/max(el, 1)), "dry_run", *dry)
	}
	for {
		refs, err := cat.RefsBefore(ctx, before, last, *batch)
		if err != nil {
			report("stopped")
			return err
		}
		if len(refs) == 0 {
			report("done")
			return nil
		}
		if !*dry {
			if err := waitDepth(ctx, ch, *queue, *maxDepth, log); err != nil {
				report("stopped")
				return err
			}
		}
		t0 := time.Now()
		confirms := make([]*amqp.DeferredConfirmation, 0, len(refs))
		valid := 0
		for i := range refs {
			r := refs[i]
			if err := r.Validate(); err != nil {
				invalid++
				log.Warn("skipped invalid ref", "key", r.Key, "err", err)
				continue
			}
			valid++
			if *dry {
				continue
			}
			body, err := json.Marshal(r)
			if err != nil {
				return err
			}
			dc, err := ch.PublishWithDeferredConfirmWithContext(ctx, contract.ExchangeDirect, *queue, false, false, amqp.Publishing{
				ContentType: "application/json", DeliveryMode: amqp.Persistent, Body: body,
			})
			if err != nil {
				report("stopped")
				return err
			}
			confirms = append(confirms, dc)
		}
		for _, dc := range confirms {
			ok, err := dc.WaitContext(ctx)
			if err != nil {
				report("stopped")
				return err
			}
			if !ok {
				report("stopped")
				return errors.New("broker refused a ref")
			}
		}
		// Only now is the whole page in the queue: a rerun with -after=last_key
		// resends nothing that was lost and skips nothing.
		sent += valid
		last = refs[len(refs)-1].Key
		if time.Since(lastLog) > 30*time.Second {
			report("progress")
			lastLog = time.Now()
		}
		if wait := time.Duration(len(refs))*time.Second/time.Duration(*rate) - time.Since(t0); wait > 0 && !*dry {
			select {
			case <-ctx.Done():
				report("stopped")
				return ctx.Err()
			case <-time.After(wait):
			}
		}
	}
}

// waitDepth blocks while the queue holds more than maxDepth messages.
func waitDepth(ctx context.Context, ch *amqp.Channel, queue string, maxDepth int, log *slog.Logger) error {
	for paused := false; ; paused = true {
		q, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
		if err != nil {
			return err
		}
		if q.Messages <= maxDepth {
			if paused {
				log.Info("resumed", "queue", queue, "depth", q.Messages)
			}
			return nil
		}
		if !paused {
			log.Info("paused: queue too deep", "queue", queue, "depth", q.Messages, "max", maxDepth)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
