package kubelet

import (
	"encoding/json"
	"testing"
)

func TestCheckpointResponse_Parse(t *testing.T) {
	raw := `{"items":["checkpoint-myapp-2024-01-15T10:30:00Z.tar","checkpoint-myapp-2024-01-15T10:31:00Z.tar"]}`

	var resp CheckpointResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("failed to unmarshal checkpoint response: %v", err)
	}

	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(resp.Items))
	}

	expected := []string{
		"checkpoint-myapp-2024-01-15T10:30:00Z.tar",
		"checkpoint-myapp-2024-01-15T10:31:00Z.tar",
	}
	for i, item := range resp.Items {
		if item != expected[i] {
			t.Errorf("item[%d]: expected %q, got %q", i, expected[i], item)
		}
	}
}

func TestCheckpointResponse_ParseEmpty(t *testing.T) {
	raw := `{"items":[]}`

	var resp CheckpointResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("failed to unmarshal empty checkpoint response: %v", err)
	}

	if len(resp.Items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(resp.Items))
	}
}

func TestCheckpoint_EmptyParams(t *testing.T) {
	// Client with nil restClient — we only need to test the parameter validation
	// that runs before any REST call.
	c := &Client{}

	tests := []struct {
		name      string
		node      string
		ns        string
		pod       string
		container string
	}{
		{"empty node", "", "default", "app-0", "app"},
		{"empty namespace", "node-1", "", "app-0", "app"},
		{"empty pod", "node-1", "default", "", "app"},
		{"empty container", "node-1", "default", "app-0", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.Checkpoint(t.Context(), tt.node, tt.ns, tt.pod, tt.container)
			if err == nil {
				t.Error("expected error for empty parameter")
			}
		})
	}
}
