package messaging

import (
	"context"
	"errors"
	"testing"
)

func TestMockBrokerClient_ConnectAndClose(t *testing.T) {
	mock := NewMockBrokerClient()
	ctx := context.Background()

	// Should not be connected initially
	if mock.Connected {
		t.Fatal("expected Connected to be false before Connect()")
	}

	if err := mock.Connect(ctx, "amqp://localhost:5672"); err != nil {
		t.Fatalf("Connect() unexpected error: %v", err)
	}
	if !mock.Connected {
		t.Fatal("expected Connected to be true after Connect()")
	}

	if err := mock.Close(); err != nil {
		t.Fatalf("Close() unexpected error: %v", err)
	}
	if mock.Connected {
		t.Fatal("expected Connected to be false after Close()")
	}

	// Test error injection on connect
	mock.ConnectErr = errors.New("connection refused")
	if err := mock.Connect(ctx, "amqp://bad-host"); err == nil {
		t.Fatal("expected Connect() to return an error when ConnectErr is set")
	}
}

func TestMockBrokerClient_CreateAndDeleteSecondaryQueue(t *testing.T) {
	mock := NewMockBrokerClient()
	ctx := context.Background()

	_ = mock.Connect(ctx, "amqp://localhost:5672")

	// Create a secondary queue
	secondaryQ, err := mock.CreateSecondaryQueue(ctx, "orders", "orders.fanout", "")
	if err != nil {
		t.Fatalf("CreateSecondaryQueue() unexpected error: %v", err)
	}

	expectedName := "orders.ms2m-replay"
	if secondaryQ != expectedName {
		t.Errorf("expected secondary queue %q, got %q", expectedName, secondaryQ)
	}

	// The queue should appear in the Queues map with depth 0
	depth, ok := mock.Queues[secondaryQ]
	if !ok {
		t.Fatalf("expected queue %q to exist in Queues map", secondaryQ)
	}
	if depth != 0 {
		t.Errorf("expected initial depth 0, got %d", depth)
	}

	// Delete the secondary queue
	if err := mock.DeleteSecondaryQueue(ctx, secondaryQ, "orders"); err != nil {
		t.Fatalf("DeleteSecondaryQueue() unexpected error: %v", err)
	}
	if _, ok := mock.Queues[secondaryQ]; ok {
		t.Error("expected queue to be removed from Queues map after delete")
	}

	// Test error injection on create
	mock.CreateQueueErr = errors.New("exchange declare failed")
	if _, err := mock.CreateSecondaryQueue(ctx, "payments", "payments.fanout", ""); err == nil {
		t.Fatal("expected CreateSecondaryQueue() to return error when CreateQueueErr is set")
	}

	// Test error injection on delete
	mock.CreateQueueErr = nil
	mock.DeleteQueueErr = errors.New("queue not found")
	if err := mock.DeleteSecondaryQueue(ctx, "payments.ms2m-replay", "payments"); err == nil {
		t.Fatal("expected DeleteSecondaryQueue() to return error when DeleteQueueErr is set")
	}
}

func TestMockBrokerClient_QueueDepth(t *testing.T) {
	mock := NewMockBrokerClient()
	ctx := context.Background()

	_ = mock.Connect(ctx, "amqp://localhost:5672")

	// Set up a queue and give it some depth
	_, _ = mock.CreateSecondaryQueue(ctx, "events", "events.fanout", "")
	mock.SetQueueDepth("events.ms2m-replay", 42)

	depth, err := mock.GetQueueDepth(ctx, "events.ms2m-replay")
	if err != nil {
		t.Fatalf("GetQueueDepth() unexpected error: %v", err)
	}
	if depth != 42 {
		t.Errorf("expected depth 42, got %d", depth)
	}

	// Unknown queue should return 0
	depth, err = mock.GetQueueDepth(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("GetQueueDepth() for unknown queue: %v", err)
	}
	if depth != 0 {
		t.Errorf("expected depth 0 for unknown queue, got %d", depth)
	}

	// Test error injection
	mock.DepthErr = errors.New("broker unreachable")
	if _, err := mock.GetQueueDepth(ctx, "events.ms2m-replay"); err == nil {
		t.Fatal("expected GetQueueDepth() to return error when DepthErr is set")
	}
}

func TestMockBrokerClient_SendControlMessage(t *testing.T) {
	mock := NewMockBrokerClient()
	ctx := context.Background()

	_ = mock.Connect(ctx, "amqp://localhost:5672")

	payload := map[string]interface{}{
		"queue": "orders.ms2m-replay",
	}

	if err := mock.SendControlMessage(ctx, "orders-pod-0", ControlStartReplay, payload); err != nil {
		t.Fatalf("SendControlMessage() unexpected error: %v", err)
	}

	if len(mock.ControlMessages) != 1 {
		t.Fatalf("expected 1 control message, got %d", len(mock.ControlMessages))
	}

	msg := mock.ControlMessages[0]
	if msg.TargetPod != "orders-pod-0" {
		t.Errorf("expected TargetPod %q, got %q", "orders-pod-0", msg.TargetPod)
	}
	if msg.Type != ControlStartReplay {
		t.Errorf("expected message type %q, got %q", ControlStartReplay, msg.Type)
	}
	if msg.Payload["queue"] != "orders.ms2m-replay" {
		t.Errorf("expected payload queue %q, got %v", "orders.ms2m-replay", msg.Payload["queue"])
	}

	// Send another message
	if err := mock.SendControlMessage(ctx, "orders-pod-0", ControlEndReplay, nil); err != nil {
		t.Fatalf("SendControlMessage() unexpected error: %v", err)
	}
	if len(mock.ControlMessages) != 2 {
		t.Errorf("expected 2 control messages, got %d", len(mock.ControlMessages))
	}

	// Test error injection
	mock.SendErr = errors.New("publish failed")
	if err := mock.SendControlMessage(ctx, "orders-pod-0", ControlEndReplay, nil); err == nil {
		t.Fatal("expected SendControlMessage() to return error when SendErr is set")
	}
}
