package scan

import (
	"context"
	"encoding/json"
	"errors"

	amqp "github.com/rabbitmq/amqp091-go"

	"seapin/internal/contract"
	"seapin/internal/rmq"
)

// Publisher sends refs to the "seapin" fanout exchange and waits for the
// broker's confirms, so a cursor only moves past refs RabbitMQ has taken.
// One per cursor; not safe for concurrent use.
type Publisher struct {
	URL  string // one or more cluster nodes, comma-separated
	Name string // connection name, for the RabbitMQ UI

	conn *amqp.Connection
	ch   *amqp.Channel
}

func (p *Publisher) Publish(ctx context.Context, refs []contract.Ref) error {
	if len(refs) == 0 {
		return nil
	}
	if p.ch == nil || p.ch.IsClosed() {
		if err := p.connect(); err != nil {
			return err
		}
	}
	confirms := make([]*amqp.DeferredConfirmation, 0, len(refs))
	for _, r := range refs {
		body, err := json.Marshal(r)
		if err != nil {
			return err
		}
		dc, err := p.ch.PublishWithDeferredConfirmWithContext(ctx, contract.ExchangeRefs, "", false, false, amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Body:         body,
		})
		if err != nil {
			p.Close()
			return err
		}
		confirms = append(confirms, dc)
	}
	for _, dc := range confirms {
		ok, err := dc.WaitContext(ctx)
		if err != nil {
			p.Close()
			return err
		}
		if !ok {
			return errors.New("broker refused a ref")
		}
	}
	return nil
}

func (p *Publisher) connect() error {
	p.Close()
	conn, err := rmq.Dial(p.URL, p.Name)
	if err != nil {
		return err
	}
	ch, err := conn.Channel()
	if err == nil {
		err = ch.ExchangeDeclare(contract.ExchangeRefs, "fanout", true, false, false, false, nil)
	}
	if err == nil {
		err = ch.Confirm(false)
	}
	if err != nil {
		conn.Close()
		return err
	}
	p.conn, p.ch = conn, ch
	return nil
}

func (p *Publisher) Close() {
	if p.conn != nil {
		p.conn.Close()
	}
	p.conn, p.ch = nil, nil
}
