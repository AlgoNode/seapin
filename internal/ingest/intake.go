package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	amqp "github.com/rabbitmq/amqp091-go"

	"seapin/internal/catalog"
	"seapin/internal/contract"
	"seapin/internal/rmq"
)

type IntakeCatalog interface {
	ApplyRef(ctx context.Context, r contract.Ref) (int64, error)
	AddTakedown(ctx context.Context, cid, reason string) error
}

// Intake consumes the region's queue on the "seapin" fanout exchange. Each
// message costs one catalog transaction and is acked after it; nothing here
// waits on IPFS, so the queue never backs up behind a slow fetch.
type Intake struct {
	URL       string // one or more cluster nodes, comma-separated
	Queue     string // seapin.<region>
	QueueType string // "" (classic) or "quorum"
	MaxLength int64  // the queue drops its oldest messages beyond this; anti-entropy covers the loss
	Prefetch  int
	Catalog   IntakeCatalog
	Queued    func(lane string) // wakes the scheduler
	Log       *slog.Logger

	messages *prometheus.CounterVec
}

func (in *Intake) Register(reg prometheus.Registerer) {
	in.messages = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "seapin_ingest_messages_total", Help: "Queue messages by type and result (ok, invalid, requeued).",
	}, []string{"type", "result"})
	reg.MustRegister(in.messages)
}

// Run consumes until ctx ends, reconnecting with backoff.
func (in *Intake) Run(ctx context.Context) {
	wait := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := in.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			wait = time.Second
		}
		in.Log.Warn("rabbitmq session ended, reconnecting", "err", err, "in", wait)
		if !sleep(ctx, wait) {
			return
		}
		wait = min(2*wait, 30*time.Second)
	}
}

func (in *Intake) session(ctx context.Context) error {
	conn, err := rmq.Dial(in.URL, "seapin-ingest "+in.Queue)
	if err != nil {
		return err
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	if err := ch.Qos(in.Prefetch, 0, false); err != nil {
		return err
	}
	if err := ch.ExchangeDeclare(contract.ExchangeRefs, "fanout", true, false, false, false, nil); err != nil {
		return err
	}
	args := amqp.Table{"x-max-length": in.MaxLength, "x-overflow": "drop-head"}
	if in.QueueType != "" {
		args["x-queue-type"] = in.QueueType
	}
	if _, err := ch.QueueDeclare(in.Queue, true, false, false, false, args); err != nil {
		return err
	}
	if err := ch.QueueBind(in.Queue, "", contract.ExchangeRefs, false, nil); err != nil {
		return err
	}
	deliveries, err := ch.ConsumeWithContext(ctx, in.Queue, "seapin-ingest", false, false, false, false, nil)
	if err != nil {
		return err
	}
	closed := conn.NotifyClose(make(chan *amqp.Error, 1))
	in.Log.Info("consuming", "queue", in.Queue)
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-closed:
			return e
		case d, ok := <-deliveries:
			if !ok {
				return errors.New("delivery channel closed")
			}
			in.handle(ctx, d)
		}
	}
}

func (in *Intake) handle(ctx context.Context, d amqp.Delivery) {
	msg, err := contract.Decode(d.Body)
	if err != nil {
		in.Log.Warn("dropping invalid message", "err", err)
		in.messages.WithLabelValues("unknown", "invalid").Inc()
		d.Nack(false, false)
		return
	}
	var typ string
	switch m := msg.(type) {
	case *contract.Ref:
		typ = contract.TypeRef
		var queued int64
		if queued, err = in.Catalog.ApplyRef(ctx, *m); err == nil && queued > 0 {
			if m.Src == contract.SrcBackfill {
				in.Queued(catalog.LaneBackfill)
			} else {
				in.Queued(catalog.LaneFast)
			}
		}
	case *contract.Takedown:
		typ = contract.TypeTakedown
		err = in.Catalog.AddTakedown(ctx, m.CID, m.Reason)
	}
	if err != nil {
		in.Log.Error("recording message, requeued", "type", typ, "err", err)
		in.messages.WithLabelValues(typ, "requeued").Inc()
		d.Nack(false, true)
		sleep(ctx, time.Second)
		return
	}
	in.messages.WithLabelValues(typ, "ok").Inc()
	d.Ack(false)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Events publishes object outcomes to the "seapin.events" topic exchange with
// routing key "<region>.<status>". Best effort: a lost event is recovered by
// reading the catalog, so failures are logged, not retried.
type Events struct {
	URL string
	Log *slog.Logger

	mu sync.Mutex
	ch *amqp.Channel
	cn *amqp.Connection
}

func (e *Events) Publish(ctx context.Context, ev contract.Event) {
	if e.URL == "" {
		return
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ch == nil || e.ch.IsClosed() {
		if err := e.connect(); err != nil {
			e.Log.Warn("events: connect", "err", err)
			return
		}
	}
	err = e.ch.PublishWithContext(ctx, contract.ExchangeEvents, ev.Region+"."+ev.Status, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Timestamp:    ev.At,
		Body:         body,
	})
	if err != nil {
		e.Log.Warn("events: publish", "err", err)
		e.close()
	}
}

func (e *Events) connect() error {
	e.close()
	cn, err := rmq.Dial(e.URL, "seapin-events")
	if err != nil {
		return err
	}
	ch, err := cn.Channel()
	if err == nil {
		err = ch.ExchangeDeclare(contract.ExchangeEvents, "topic", true, false, false, false, nil)
	}
	if err != nil {
		cn.Close()
		return err
	}
	e.cn, e.ch = cn, ch
	return nil
}

func (e *Events) close() {
	if e.cn != nil {
		e.cn.Close()
	}
	e.cn, e.ch = nil, nil
}

// Close releases the connection.
func (e *Events) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.close()
}
