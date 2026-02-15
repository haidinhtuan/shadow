package messaging

import "context"

// ControlMessageType identifies the type of control message sent to a pod
// during the MS2M migration process.
type ControlMessageType string

const (
	// ControlStartReplay tells the target pod to begin replaying
	// messages from the secondary queue.
	ControlStartReplay ControlMessageType = "START_REPLAY"

	// ControlEndReplay tells the target pod to stop replaying and
	// switch over to consuming from the primary queue.
	ControlEndReplay ControlMessageType = "END_REPLAY"
)

// BrokerClient abstracts message broker operations needed by the migration
// controller. The interface is intentionally small — it covers queue
// fan-out setup, depth monitoring, and control-plane messaging.
type BrokerClient interface {
	// Connect establishes a connection to the message broker.
	Connect(ctx context.Context, brokerURL string) error

	// Close tears down the broker connection and releases resources.
	Close() error

	// CreateSecondaryQueue sets up a fanout exchange and a secondary
	// replay queue, binding both the primary and secondary queues so
	// that new messages are duplicated.
	// Returns the name of the created secondary queue.
	CreateSecondaryQueue(ctx context.Context, primaryQueue, exchangeName, routingKey string) (string, error)

	// DeleteSecondaryQueue removes the secondary queue and its
	// associated exchange, restoring the original routing.
	DeleteSecondaryQueue(ctx context.Context, secondaryQueue, primaryQueue string) error

	// GetQueueDepth returns the number of messages waiting in the
	// given queue. Used to monitor replay lag.
	GetQueueDepth(ctx context.Context, queueName string) (int, error)

	// SendControlMessage publishes a control message to a pod-specific
	// queue (ms2m.control.<targetPod>).
	SendControlMessage(ctx context.Context, targetPod string, msgType ControlMessageType, payload map[string]interface{}) error
}
