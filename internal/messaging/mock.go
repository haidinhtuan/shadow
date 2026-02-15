package messaging

import (
	"context"
	"sync"
)

// MockControlMessage records a control message sent via the mock client.
type MockControlMessage struct {
	TargetPod string
	Type      ControlMessageType
	Payload   map[string]interface{}
}

// MockBrokerClient is an in-memory implementation of BrokerClient for use
// in unit tests. It tracks connection state, queue creation/deletion, and
// control messages. Individual errors can be injected to simulate failures.
type MockBrokerClient struct {
	mu sync.Mutex

	Connected       bool
	Queues          map[string]int // queue name -> message depth
	ControlMessages []MockControlMessage

	// Error injection fields — set these before calling the method
	// to simulate broker failures.
	ConnectErr     error
	CreateQueueErr error
	DeleteQueueErr error
	DepthErr       error
	SendErr        error
	CloseErr       error
}

// NewMockBrokerClient returns a MockBrokerClient ready for use in tests.
func NewMockBrokerClient() *MockBrokerClient {
	return &MockBrokerClient{
		Queues: make(map[string]int),
	}
}

func (m *MockBrokerClient) Connect(_ context.Context, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.ConnectErr != nil {
		return m.ConnectErr
	}
	m.Connected = true
	return nil
}

func (m *MockBrokerClient) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Connected = false
	if m.CloseErr != nil {
		return m.CloseErr
	}
	return nil
}

func (m *MockBrokerClient) CreateSecondaryQueue(_ context.Context, primaryQueue, _, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.CreateQueueErr != nil {
		return "", m.CreateQueueErr
	}

	secondaryQueue := primaryQueue + ".ms2m-replay"
	m.Queues[secondaryQueue] = 0
	return secondaryQueue, nil
}

func (m *MockBrokerClient) DeleteSecondaryQueue(_ context.Context, secondaryQueue, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.DeleteQueueErr != nil {
		return m.DeleteQueueErr
	}

	delete(m.Queues, secondaryQueue)
	return nil
}

func (m *MockBrokerClient) GetQueueDepth(_ context.Context, queueName string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.DepthErr != nil {
		return 0, m.DepthErr
	}

	return m.Queues[queueName], nil
}

func (m *MockBrokerClient) SendControlMessage(_ context.Context, targetPod string, msgType ControlMessageType, payload map[string]interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.SendErr != nil {
		return m.SendErr
	}

	m.ControlMessages = append(m.ControlMessages, MockControlMessage{
		TargetPod: targetPod,
		Type:      msgType,
		Payload:   payload,
	})
	return nil
}

// SetQueueDepth is a test helper that sets the message count for a queue.
func (m *MockBrokerClient) SetQueueDepth(queue string, depth int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Queues[queue] = depth
}

// Compile-time check that MockBrokerClient satisfies BrokerClient.
var _ BrokerClient = (*MockBrokerClient)(nil)
