package scan

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"seapin/internal/contract"
	"seapin/internal/rmq"
)

// TestPublisherConfirms publishes through a real broker (SEAPIN_TEST_RMQ_URL,
// e.g. the dev stack's, possibly after a dead node to exercise failover) into
// an exclusive queue bound to the exchange.
func TestPublisherConfirms(t *testing.T) {
	url := os.Getenv("SEAPIN_TEST_RMQ_URL")
	if url == "" {
		t.Skip("SEAPIN_TEST_RMQ_URL not set")
	}
	conn, err := rmq.Dial(url, "seapin-scan test consumer")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ch, _ := conn.Channel()
	if err := ch.ExchangeDeclare(contract.ExchangeRefs, "fanout", true, false, false, false, nil); err != nil {
		t.Fatal(err)
	}
	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.QueueBind(q.Name, "", contract.ExchangeRefs, false, nil); err != nil {
		t.Fatal(err)
	}
	msgs, _ := ch.Consume(q.Name, "", true, true, false, false, nil)

	p := &Publisher{URL: url, Name: "seapin-scan test"}
	defer p.Close()
	var refs []contract.Ref
	for i := range 3 {
		refs = append(refs, contract.NewRef(contract.Ref{Chain: "testchain", Asset: uint64(i + 1), Path: contract.PathURL,
			CID: rawCID, Creator: "C", Src: contract.SrcLive, Round: 7, BlockTime: time.Now()}))
	}
	if err := p.Publish(context.Background(), refs); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		select {
		case m := <-msgs:
			var r contract.Ref
			if err := json.Unmarshal(m.Body, &r); err != nil || r.Asset != uint64(i+1) || m.DeliveryMode != amqp.Persistent {
				t.Errorf("message %d: %+v %v persistent=%v", i, r, err, m.DeliveryMode == amqp.Persistent)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 3 refs arrived", i)
		}
	}
}
