// Package rmq dials the RabbitMQ cluster. The URL may list several nodes,
// comma-separated; the first one that answers is used, so a client fails over
// when its node is down.
package rmq

import (
	"errors"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Dial connects to the first reachable node in urls with AMQP heartbeats.
// name shows up as the connection name in the RabbitMQ UI.
func Dial(urls, name string) (*amqp.Connection, error) {
	var errs []error
	for _, u := range strings.Split(urls, ",") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		c, err := amqp.DialConfig(u, amqp.Config{
			Heartbeat:  10 * time.Second,
			Properties: amqp.Table{"connection_name": name},
		})
		if err == nil {
			return c, nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil, errors.New("rmq: no URL")
	}
	return nil, errors.Join(errs...)
}
