package messaging

import (
	"context"
	"encoding/json"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

// RabbitMQClient implements BrokerClient using AMQP 0-9-1 (RabbitMQ).
type RabbitMQClient struct {
	conn *amqp.Connection
	ch   *amqp.Channel
}

// NewRabbitMQClient returns a RabbitMQClient. Call Connect() before using
// any other methods.
func NewRabbitMQClient() *RabbitMQClient {
	return &RabbitMQClient{}
}

func (r *RabbitMQClient) Connect(_ context.Context, brokerURL string) error {
	// Close any existing connection to avoid leaking resources
	if r.ch != nil || r.conn != nil {
		_ = r.Close()
	}

	conn, err := amqp.Dial(brokerURL)
	if err != nil {
		return fmt.Errorf("amqp dial: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return fmt.Errorf("amqp open channel: %w", err)
	}

	r.conn = conn
	r.ch = ch
	return nil
}

func (r *RabbitMQClient) Close() error {
	var firstErr error

	if r.ch != nil {
		if err := r.ch.Close(); err != nil {
			firstErr = err
		}
		r.ch = nil
	}

	if r.conn != nil {
		if err := r.conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		r.conn = nil
	}

	return firstErr
}

// CreateSecondaryQueue sets up fan-out duplication for a primary queue.
//
// It declares a fanout exchange, declares the secondary replay queue,
// then binds both the primary and secondary queues to the exchange so
// every published message reaches both consumers.
func (r *RabbitMQClient) CreateSecondaryQueue(_ context.Context, primaryQueue, exchangeName, _ string) (string, error) {
	if r.ch == nil {
		return "", fmt.Errorf("broker channel not connected")
	}

	// Declare the fanout exchange
	if err := r.ch.ExchangeDeclare(
		exchangeName,
		"fanout",
		true,  // durable
		false, // auto-deleted
		false, // internal
		false, // no-wait
		nil,
	); err != nil {
		return "", fmt.Errorf("declare exchange %q: %w", exchangeName, err)
	}

	secondaryQueue := primaryQueue + ".ms2m-replay"

	// Declare the secondary replay queue
	if _, err := r.ch.QueueDeclare(
		secondaryQueue,
		true,  // durable
		false, // auto-delete
		false, // exclusive
		false, // no-wait
		nil,
	); err != nil {
		return "", fmt.Errorf("declare queue %q: %w", secondaryQueue, err)
	}

	// Bind both queues to the fanout exchange
	if err := r.ch.QueueBind(primaryQueue, "", exchangeName, false, nil); err != nil {
		return "", fmt.Errorf("bind primary queue %q to %q: %w", primaryQueue, exchangeName, err)
	}
	if err := r.ch.QueueBind(secondaryQueue, "", exchangeName, false, nil); err != nil {
		return "", fmt.Errorf("bind secondary queue %q to %q: %w", secondaryQueue, exchangeName, err)
	}

	return secondaryQueue, nil
}

// DeleteSecondaryQueue tears down the replay setup: unbinds and deletes
// the secondary queue. The primary queue binding and the shared exchange
// are left intact so the producer can continue publishing.
func (r *RabbitMQClient) DeleteSecondaryQueue(_ context.Context, secondaryQueue, primaryQueue, exchangeName string) error {
	if r.ch == nil {
		return fmt.Errorf("broker channel not connected")
	}

	// Unbind only the secondary queue from the fanout exchange
	if err := r.ch.QueueUnbind(secondaryQueue, "", exchangeName, nil); err != nil {
		return fmt.Errorf("unbind secondary queue %q from %q: %w", secondaryQueue, exchangeName, err)
	}

	// Delete the secondary queue
	if _, err := r.ch.QueueDelete(secondaryQueue, false, false, false); err != nil {
		return fmt.Errorf("delete queue %q: %w", secondaryQueue, err)
	}

	return nil
}

func (r *RabbitMQClient) GetQueueDepth(_ context.Context, queueName string) (int, error) {
	if r.ch == nil {
		return 0, fmt.Errorf("broker channel not connected")
	}
	q, err := r.ch.QueueInspect(queueName)
	if err != nil {
		return 0, fmt.Errorf("inspect queue %q: %w", queueName, err)
	}
	return q.Messages, nil
}

// controlMessage is the JSON envelope sent over the control queue.
type controlMessage struct {
	Type    ControlMessageType     `json:"type"`
	Payload map[string]interface{} `json:"payload,omitempty"`
}

func (r *RabbitMQClient) SendControlMessage(ctx context.Context, targetPod string, msgType ControlMessageType, payload map[string]interface{}) error {
	if r.ch == nil {
		return fmt.Errorf("broker channel not connected")
	}

	controlQueue := "ms2m.control." + targetPod

	// Declare the control queue (idempotent)
	if _, err := r.ch.QueueDeclare(
		controlQueue,
		true,  // durable
		false, // auto-delete
		false, // exclusive
		false, // no-wait
		nil,
	); err != nil {
		return fmt.Errorf("declare control queue %q: %w", controlQueue, err)
	}

	body, err := json.Marshal(controlMessage{
		Type:    msgType,
		Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("marshal control message: %w", err)
	}

	if err := r.ch.PublishWithContext(ctx,
		"",           // default exchange
		controlQueue, // routing key = queue name
		false,        // mandatory
		false,        // immediate
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Body:         body,
		},
	); err != nil {
		return fmt.Errorf("publish control message to %q: %w", controlQueue, err)
	}

	return nil
}

// Compile-time check that RabbitMQClient satisfies BrokerClient.
var _ BrokerClient = (*RabbitMQClient)(nil)
