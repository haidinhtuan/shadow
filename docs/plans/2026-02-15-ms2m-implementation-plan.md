# MS2M Controller Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Implement the 5-phase MS2M migration controller with checkpoint, transfer, restore, replay, and finalize logic -- ready for scientific evaluation.

**Architecture:** Controller + Transfer Job pattern. The reconciler orchestrates the state machine via the K8s API. Node-local checkpoint work is delegated to a Kubernetes Job. RabbitMQ manages message mirroring via fanout exchanges. See `docs/plans/2026-02-15-ms2m-controller-design.md` for full design.

**Tech Stack:** Go 1.25, controller-runtime, client-go, `go-containerregistry` (crane), `amqp091-go` (RabbitMQ), kubeadm + CRI-O + CRIU on IONOS VMs.

---

## Task 1: Fix Build - Add controller-runtime Dependency

The project imports `sigs.k8s.io/controller-runtime` but it's missing from `go.mod`. Nothing compiles until this is fixed.

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`

**Step 1: Add controller-runtime dependency**

Run:
```bash
go get sigs.k8s.io/controller-runtime@latest
```

**Step 2: Verify the project compiles**

Run:
```bash
go build ./cmd/main.go
```
Expected: Build succeeds with no errors.

**Step 3: Run existing tests**

Run:
```bash
go test ./... 2>&1
```
Expected: All tests pass (or no tests exist yet).

**Step 4: Commit**

```bash
git add go.mod go.sum
git commit -m "build: add controller-runtime dependency"
```

---

## Task 2: Extend CRD Types

Add `MigrationStrategy`, `StartTime`, `PhaseTimings`, and `ExchangeName`/`RoutingKey` to the CRD types. Update DeepCopy functions and CRD YAML.

**Files:**
- Modify: `api/v1alpha1/types.go`
- Modify: `api/v1alpha1/deepcopy.go`
- Modify: `config/crd/bases/migration.vibe.io_statefulmigrations.yaml`

**Step 1: Write test for new CRD fields**

Create: `api/v1alpha1/types_test.go`

```go
package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStatefulMigrationSpec_MigrationStrategy(t *testing.T) {
	spec := StatefulMigrationSpec{
		SourcePod:         "app-0",
		MigrationStrategy: "ShadowPod",
	}
	if spec.MigrationStrategy != "ShadowPod" {
		t.Errorf("expected ShadowPod, got %s", spec.MigrationStrategy)
	}
}

func TestStatefulMigrationStatus_PhaseTimings(t *testing.T) {
	status := StatefulMigrationStatus{
		Phase: PhasePending,
		PhaseTimings: map[string]string{
			"Checkpointing": "2.3s",
			"Transferring":  "8.1s",
		},
	}
	if status.PhaseTimings["Checkpointing"] != "2.3s" {
		t.Errorf("expected 2.3s, got %s", status.PhaseTimings["Checkpointing"])
	}
}

func TestStatefulMigrationStatus_StartTime(t *testing.T) {
	now := metav1.Now()
	status := StatefulMigrationStatus{
		Phase:     PhasePending,
		StartTime: &now,
	}
	if status.StartTime == nil {
		t.Error("expected StartTime to be set")
	}
}

func TestMessageQueueConfig_ExchangeFields(t *testing.T) {
	cfg := MessageQueueConfig{
		QueueName:    "app.events",
		BrokerURL:    "amqp://localhost:5672",
		ExchangeName: "app.fanout",
		RoutingKey:   "events",
	}
	if cfg.ExchangeName != "app.fanout" {
		t.Errorf("expected app.fanout, got %s", cfg.ExchangeName)
	}
}

func TestDeepCopy_PhaseTimings(t *testing.T) {
	original := &StatefulMigrationStatus{
		Phase: PhaseCheckpointing,
		PhaseTimings: map[string]string{
			"Pending": "1.0s",
		},
	}
	copied := original.DeepCopy()
	copied.PhaseTimings["Pending"] = "modified"
	if original.PhaseTimings["Pending"] != "1.0s" {
		t.Error("DeepCopy should not share map reference")
	}
}
```

**Step 2: Run test to verify it fails**

Run:
```bash
go test ./api/v1alpha1/ -v -run TestStatefulMigration
```
Expected: FAIL - `MigrationStrategy` field not found.

**Step 3: Update types.go**

Modify `api/v1alpha1/types.go`:

Add `MigrationStrategy` to `StatefulMigrationSpec`:
```go
type StatefulMigrationSpec struct {
	// SourcePod is the name of the pod to migrate (must be in the same namespace)
	SourcePod string `json:"sourcePod,omitempty"`

	// TargetNode is the optional node selector for the target
	TargetNode string `json:"targetNode,omitempty"`

	// CheckpointImageRepository is the registry location to push the checkpoint image
	CheckpointImageRepository string `json:"checkpointImageRepository,omitempty"`

	// ReplayCutoffSeconds is the threshold in seconds to trigger the final cutoff
	ReplayCutoffSeconds int32 `json:"replayCutoffSeconds,omitempty"`

	// MessageQueueConfig contains details about the messaging system
	MessageQueueConfig MessageQueueConfig `json:"messageQueueConfig,omitempty"`

	// MigrationStrategy determines how to handle pod identity conflicts.
	// "ShadowPod" creates a shadow pod alongside the source (for individual pods).
	// "Sequential" deletes source before creating target (required for StatefulSets).
	// If empty, auto-detected from ownerReferences.
	MigrationStrategy string `json:"migrationStrategy,omitempty"`
}
```

Add `ExchangeName` and `RoutingKey` to `MessageQueueConfig`:
```go
type MessageQueueConfig struct {
	// QueueName is the name of the queue to migrate
	QueueName string `json:"queueName,omitempty"`
	// BrokerURL is the connection string for the message broker
	BrokerURL string `json:"brokerUrl,omitempty"`
	// ExchangeName is the exchange the producer publishes to
	ExchangeName string `json:"exchangeName,omitempty"`
	// RoutingKey is the routing key used for the primary queue binding
	RoutingKey string `json:"routingKey,omitempty"`
}
```

Add `StartTime` and `PhaseTimings` to `StatefulMigrationStatus`:
```go
type StatefulMigrationStatus struct {
	// Phase represents the current phase of the migration
	Phase Phase `json:"phase,omitempty"`

	// SourceNode is the node where the source pod is running
	SourceNode string `json:"sourceNode,omitempty"`

	// CheckpointID is the identifier of the created checkpoint
	CheckpointID string `json:"checkpointID,omitempty"`

	// TargetPod is the name of the restored pod
	TargetPod string `json:"targetPod,omitempty"`

	// StartTime records when the migration was initiated
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// PhaseTimings records the duration of each completed phase
	PhaseTimings map[string]string `json:"phaseTimings,omitempty"`

	// Conditions represents the latest available observations of the object's state
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

**Step 4: Update deepcopy.go**

Update `StatefulMigrationStatus.DeepCopyInto` to handle `StartTime` and `PhaseTimings`:

```go
func (in *StatefulMigrationStatus) DeepCopyInto(out *StatefulMigrationStatus) {
	*out = *in
	if in.StartTime != nil {
		in, out := &in.StartTime, &out.StartTime
		*out = (*in).DeepCopy()
	}
	if in.PhaseTimings != nil {
		in, out := &in.PhaseTimings, &out.PhaseTimings
		*out = make(map[string]string, len(*in))
		for key, val := range *in {
			(*out)[key] = val
		}
	}
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}
```

**Step 5: Update the CRD YAML**

Add the new fields to `config/crd/bases/migration.vibe.io_statefulmigrations.yaml` under `spec.properties` and `status.properties`. Add `migrationStrategy` (type: string, enum: ShadowPod, Sequential) to spec. Add `exchangeName` and `routingKey` (type: string) under messageQueueConfig. Add `startTime` (format: date-time) and `phaseTimings` (type: object, additionalProperties: string) to status.

**Step 6: Run tests**

Run:
```bash
go test ./api/v1alpha1/ -v
```
Expected: All 5 tests PASS.

**Step 7: Verify build**

Run:
```bash
go build ./cmd/main.go
```
Expected: Succeeds.

**Step 8: Commit**

```bash
git add api/ config/crd/
git commit -m "feat: extend CRD with migration strategy, phase timings, and exchange config"
```

---

## Task 3: Implement KubeletClient

Wrapper for calling the Kubelet Checkpoint API through the API server proxy.

**Files:**
- Create: `internal/kubelet/client.go`
- Create: `internal/kubelet/client_test.go`

**Step 1: Write tests**

Create `internal/kubelet/client_test.go`:

```go
package kubelet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckpointResponse_Parse(t *testing.T) {
	raw := `{"items":["/var/lib/kubelet/checkpoints/checkpoint-app-0_default-app-1706123456.tar"]}`
	var resp CheckpointResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(resp.Items))
	}
	expected := "/var/lib/kubelet/checkpoints/checkpoint-app-0_default-app-1706123456.tar"
	if resp.Items[0] != expected {
		t.Errorf("expected %s, got %s", expected, resp.Items[0])
	}
}

func TestBuildCheckpointPath(t *testing.T) {
	tests := []struct {
		node, ns, pod, container string
		expectedPath             string
	}{
		{
			"node-1", "default", "app-0", "app",
			"/api/v1/nodes/node-1/proxy/checkpoint/default/app-0/app",
		},
		{
			"worker-2", "prod", "counter-0", "counter",
			"/api/v1/nodes/worker-2/proxy/checkpoint/prod/counter-0/counter",
		},
	}
	for _, tt := range tests {
		path := buildCheckpointPath(tt.node, tt.ns, tt.pod, tt.container)
		if path != tt.expectedPath {
			t.Errorf("expected %s, got %s", tt.expectedPath, path)
		}
	}
}
```

**Step 2: Run tests to verify failure**

Run:
```bash
go test ./internal/kubelet/ -v
```
Expected: FAIL - package doesn't exist.

**Step 3: Implement KubeletClient**

Create `internal/kubelet/client.go`:

```go
package kubelet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// CheckpointResponse represents the kubelet checkpoint API response.
type CheckpointResponse struct {
	Items []string `json:"items"`
}

// Client wraps kubernetes client for checkpoint operations.
type Client struct {
	restClient rest.Interface
}

// NewClient creates a Client from a kubernetes.Interface.
func NewClient(clientset kubernetes.Interface) *Client {
	return &Client{
		restClient: clientset.CoreV1().RESTClient(),
	}
}

// buildCheckpointPath constructs the API path for the checkpoint endpoint.
func buildCheckpointPath(nodeName, namespace, podName, containerName string) string {
	return fmt.Sprintf("/api/v1/nodes/%s/proxy/checkpoint/%s/%s/%s",
		nodeName, namespace, podName, containerName)
}

// Checkpoint triggers a container checkpoint via the kubelet API.
// The request is proxied through the API server:
// POST /api/v1/nodes/{node}/proxy/checkpoint/{namespace}/{pod}/{container}
func (c *Client) Checkpoint(ctx context.Context, nodeName, namespace, podName, containerName string) (*CheckpointResponse, error) {
	result := c.restClient.Post().
		Resource("nodes").
		Name(nodeName).
		SubResource("proxy", "checkpoint", namespace, podName, containerName).
		Do(ctx)

	if err := result.Error(); err != nil {
		return nil, fmt.Errorf("checkpoint request failed: %w", err)
	}

	var statusCode int
	result.StatusCode(&statusCode)
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("checkpoint returned status %d", statusCode)
	}

	rawBody, err := result.Raw()
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoint response: %w", err)
	}

	var resp CheckpointResponse
	if err := json.Unmarshal(rawBody, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse checkpoint response: %w", err)
	}

	return &resp, nil
}
```

**Step 4: Run tests**

Run:
```bash
go test ./internal/kubelet/ -v
```
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/kubelet/
git commit -m "feat: add kubelet checkpoint API client"
```

---

## Task 4: Implement MessagingClient Interface and RabbitMQ Backend

RabbitMQ client for fanout exchange setup, control messages, and queue depth monitoring.

**Files:**
- Create: `internal/messaging/client.go`
- Create: `internal/messaging/rabbitmq.go`
- Create: `internal/messaging/mock.go`
- Create: `internal/messaging/client_test.go`

**Step 1: Add RabbitMQ dependency**

Run:
```bash
go get github.com/rabbitmq/amqp091-go
```

**Step 2: Create the interface**

Create `internal/messaging/client.go`:

```go
package messaging

import "context"

// ControlMessageType defines migration control signals.
type ControlMessageType string

const (
	ControlStartReplay ControlMessageType = "START_REPLAY"
	ControlEndReplay   ControlMessageType = "END_REPLAY"
)

// BrokerClient defines the interface for MS2M message operations.
type BrokerClient interface {
	Connect(ctx context.Context, brokerURL string) error
	Close() error
	CreateSecondaryQueue(ctx context.Context, primaryQueue, exchangeName, routingKey string) (string, error)
	DeleteSecondaryQueue(ctx context.Context, secondaryQueue, primaryQueue string) error
	GetQueueDepth(ctx context.Context, queueName string) (int, error)
	SendControlMessage(ctx context.Context, targetPod string, msgType ControlMessageType, payload map[string]interface{}) error
}
```

**Step 3: Create mock implementation for testing**

Create `internal/messaging/mock.go`:

```go
package messaging

import (
	"context"
	"sync"
)

// MockBrokerClient implements BrokerClient for testing.
type MockBrokerClient struct {
	mu              sync.Mutex
	Connected       bool
	Queues          map[string]int // queue name -> message count
	ControlMessages []MockControlMessage
	ConnectErr      error
	CreateQueueErr  error
	DeleteQueueErr  error
	DepthErr        error
	SendErr         error
}

type MockControlMessage struct {
	TargetPod string
	Type      ControlMessageType
	Payload   map[string]interface{}
}

func NewMockBrokerClient() *MockBrokerClient {
	return &MockBrokerClient{
		Queues: make(map[string]int),
	}
}

func (m *MockBrokerClient) Connect(ctx context.Context, brokerURL string) error {
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
	return nil
}

func (m *MockBrokerClient) CreateSecondaryQueue(ctx context.Context, primaryQueue, exchangeName, routingKey string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.CreateQueueErr != nil {
		return "", m.CreateQueueErr
	}
	secondary := primaryQueue + ".ms2m-replay"
	m.Queues[secondary] = 0
	return secondary, nil
}

func (m *MockBrokerClient) DeleteSecondaryQueue(ctx context.Context, secondaryQueue, primaryQueue string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DeleteQueueErr != nil {
		return m.DeleteQueueErr
	}
	delete(m.Queues, secondaryQueue)
	return nil
}

func (m *MockBrokerClient) GetQueueDepth(ctx context.Context, queueName string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DepthErr != nil {
		return -1, m.DepthErr
	}
	return m.Queues[queueName], nil
}

func (m *MockBrokerClient) SendControlMessage(ctx context.Context, targetPod string, msgType ControlMessageType, payload map[string]interface{}) error {
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

// SetQueueDepth sets the message count for testing replay progress.
func (m *MockBrokerClient) SetQueueDepth(queue string, depth int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Queues[queue] = depth
}
```

**Step 4: Write tests for mock**

Create `internal/messaging/client_test.go`:

```go
package messaging

import (
	"context"
	"testing"
)

func TestMockBrokerClient_ConnectAndClose(t *testing.T) {
	mock := NewMockBrokerClient()
	ctx := context.Background()

	if err := mock.Connect(ctx, "amqp://localhost:5672"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mock.Connected {
		t.Error("expected Connected to be true")
	}

	mock.Close()
	if mock.Connected {
		t.Error("expected Connected to be false after close")
	}
}

func TestMockBrokerClient_CreateAndDeleteSecondaryQueue(t *testing.T) {
	mock := NewMockBrokerClient()
	ctx := context.Background()
	mock.Connect(ctx, "amqp://localhost:5672")

	secondary, err := mock.CreateSecondaryQueue(ctx, "app.events", "app.fanout", "events")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if secondary != "app.events.ms2m-replay" {
		t.Errorf("expected app.events.ms2m-replay, got %s", secondary)
	}
	if _, ok := mock.Queues[secondary]; !ok {
		t.Error("secondary queue should exist in Queues map")
	}

	if err := mock.DeleteSecondaryQueue(ctx, secondary, "app.events"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := mock.Queues[secondary]; ok {
		t.Error("secondary queue should be deleted")
	}
}

func TestMockBrokerClient_QueueDepth(t *testing.T) {
	mock := NewMockBrokerClient()
	ctx := context.Background()
	mock.Connect(ctx, "amqp://localhost:5672")

	mock.SetQueueDepth("app.events.ms2m-replay", 42)
	depth, err := mock.GetQueueDepth(ctx, "app.events.ms2m-replay")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depth != 42 {
		t.Errorf("expected depth 42, got %d", depth)
	}
}

func TestMockBrokerClient_SendControlMessage(t *testing.T) {
	mock := NewMockBrokerClient()
	ctx := context.Background()
	mock.Connect(ctx, "amqp://localhost:5672")

	err := mock.SendControlMessage(ctx, "app-0-shadow", ControlStartReplay, map[string]interface{}{
		"secondaryQueue": "app.events.ms2m-replay",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.ControlMessages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(mock.ControlMessages))
	}
	if mock.ControlMessages[0].Type != ControlStartReplay {
		t.Errorf("expected START_REPLAY, got %s", mock.ControlMessages[0].Type)
	}
}
```

**Step 5: Run tests**

Run:
```bash
go test ./internal/messaging/ -v
```
Expected: All 4 tests PASS.

**Step 6: Implement RabbitMQ backend**

Create `internal/messaging/rabbitmq.go` with the full RabbitMQ implementation using `amqp091-go`. This implements `BrokerClient` with:
- `Connect()`: Dial AMQP, open channel, enable publisher confirms
- `CreateSecondaryQueue()`: Declare fanout exchange, declare secondary queue, bind both queues
- `DeleteSecondaryQueue()`: Unbind primary, delete secondary queue, delete fanout exchange
- `GetQueueDepth()`: `QueueDeclarePassive` with channel recovery on error
- `SendControlMessage()`: Publish JSON to `ms2m.control.<pod>` queue with confirms

(Full implementation follows the patterns from the design doc Section 5.)

**Step 7: Commit**

```bash
git add internal/messaging/ go.mod go.sum
git commit -m "feat: add messaging client interface with RabbitMQ backend and mock"
```

---

## Task 5: Implement Transfer Binary

Small Go binary that builds an OCI image from a checkpoint tarball and pushes it to a registry.

**Files:**
- Create: `cmd/checkpoint-transfer/main.go`
- Create: `cmd/checkpoint-transfer/main_test.go`
- Create: `Dockerfile.transfer`

**Step 1: Add go-containerregistry dependency**

Run:
```bash
go get github.com/google/go-containerregistry
```

**Step 2: Write test**

Create `cmd/checkpoint-transfer/main_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildCheckpointImage_InvalidPath(t *testing.T) {
	_, err := buildCheckpointImage("/nonexistent/checkpoint.tar")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestBuildCheckpointImage_ValidTar(t *testing.T) {
	// Create a minimal tar file for testing
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "checkpoint.tar")

	// Create a minimal valid tar (empty tar is 1024 zero bytes)
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	// Write 1024 zero bytes (minimal empty tar)
	if _, err := f.Write(make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	img, err := buildCheckpointImage(tarPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if img == nil {
		t.Error("expected non-nil image")
	}

	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("failed to get layers: %v", err)
	}
	if len(layers) != 1 {
		t.Errorf("expected 1 layer, got %d", len(layers))
	}
}
```

**Step 3: Run test to verify failure**

Run:
```bash
go test ./cmd/checkpoint-transfer/ -v
```
Expected: FAIL - function not found.

**Step 4: Implement transfer binary**

Create `cmd/checkpoint-transfer/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func buildCheckpointImage(checkpointPath string) (v1.Image, error) {
	layer, err := tarball.LayerFromFile(checkpointPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create layer from %s: %w", checkpointPath, err)
	}

	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return nil, fmt.Errorf("failed to append layer: %w", err)
	}

	return img, nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "Usage: checkpoint-transfer <checkpoint-tar-path> <image-ref>\n")
		os.Exit(1)
	}

	checkpointPath := os.Args[1]
	imageRef := os.Args[2]

	fmt.Printf("Building checkpoint image from %s\n", checkpointPath)
	img, err := buildCheckpointImage(checkpointPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building image: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Pushing image to %s\n", imageRef)
	if err := crane.Push(img, imageRef); err != nil {
		fmt.Fprintf(os.Stderr, "Error pushing image: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Transfer complete")
}
```

**Step 5: Run tests**

Run:
```bash
go test ./cmd/checkpoint-transfer/ -v
```
Expected: PASS.

**Step 6: Create Dockerfile for transfer binary**

Create `Dockerfile.transfer`:

```dockerfile
FROM golang:1.25 as builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/checkpoint-transfer/ cmd/checkpoint-transfer/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o checkpoint-transfer ./cmd/checkpoint-transfer/

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/checkpoint-transfer /checkpoint-transfer
USER 65532:65532
ENTRYPOINT ["/checkpoint-transfer"]
```

**Step 7: Verify build**

Run:
```bash
go build -o /dev/null ./cmd/checkpoint-transfer/
```
Expected: Succeeds.

**Step 8: Commit**

```bash
git add cmd/checkpoint-transfer/ Dockerfile.transfer go.mod go.sum
git commit -m "feat: add checkpoint transfer binary using go-containerregistry"
```

---

## Task 6: Implement Controller Phase Handlers

Replace the TODO stubs with real phase logic. Wire up KubeletClient and MessagingClient.

**Files:**
- Modify: `internal/controller/statefulmigration_controller.go`
- Create: `internal/controller/statefulmigration_controller_test.go`
- Modify: `cmd/main.go`

**Step 1: Write unit tests for state machine transitions**

Create `internal/controller/statefulmigration_controller_test.go`:

```go
package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	migrationv1alpha1 "github.com/vibe-kanban/kubernetes-controller/api/v1alpha1"
	"github.com/vibe-kanban/kubernetes-controller/internal/messaging"
)

func setupTest(migration *migrationv1alpha1.StatefulMigration) (*StatefulMigrationReconciler, context.Context) {
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	migrationv1alpha1.AddToScheme(scheme)

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(migration).
		WithStatusSubresource(migration).
		Build()

	mockMsg := messaging.NewMockBrokerClient()

	r := &StatefulMigrationReconciler{
		Client:    client,
		Scheme:    scheme,
		MsgClient: mockMsg,
	}
	return r, context.Background()
}

func newMigration(name string, phase migrationv1alpha1.Phase) *migrationv1alpha1.StatefulMigration {
	return &migrationv1alpha1.StatefulMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: migrationv1alpha1.StatefulMigrationSpec{
			SourcePod:                 "app-0",
			TargetNode:                "node-2",
			CheckpointImageRepository: "registry.example.com/checkpoints",
			ReplayCutoffSeconds:       120,
			MessageQueueConfig: migrationv1alpha1.MessageQueueConfig{
				BrokerURL:    "amqp://localhost:5672",
				QueueName:    "app.events",
				ExchangeName: "app.fanout",
			},
		},
		Status: migrationv1alpha1.StatefulMigrationStatus{
			Phase: phase,
		},
	}
}

func TestReconcile_EmptyPhase_TransitionsToPending(t *testing.T) {
	m := newMigration("test-migration", "")
	r, ctx := setupTest(m)

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test-migration", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Requeue {
		t.Error("expected requeue")
	}

	updated := &migrationv1alpha1.StatefulMigration{}
	r.Get(ctx, types.NamespacedName{Name: "test-migration", Namespace: "default"}, updated)
	if updated.Status.Phase != migrationv1alpha1.PhasePending {
		t.Errorf("expected Pending, got %s", updated.Status.Phase)
	}
}

func TestReconcile_Completed_NoAction(t *testing.T) {
	m := newMigration("test-migration", migrationv1alpha1.PhaseCompleted)
	r, ctx := setupTest(m)

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test-migration", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue for completed migration")
	}
}

func TestReconcile_Failed_NoAction(t *testing.T) {
	m := newMigration("test-migration", migrationv1alpha1.PhaseFailed)
	r, ctx := setupTest(m)

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test-migration", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue for failed migration")
	}
}
```

**Step 2: Run tests to verify failure**

Run:
```bash
go test ./internal/controller/ -v -run TestReconcile
```
Expected: FAIL - `MsgClient` field not found.

**Step 3: Update the reconciler struct and phase handlers**

Modify `internal/controller/statefulmigration_controller.go`:

Add `MsgClient` and `KubeletClient` fields to the reconciler:
```go
type StatefulMigrationReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	MsgClient     messaging.BrokerClient
	KubeletClient *kubelet.Client
}
```

Update imports to include the new packages. Update `handlePending` to validate the source pod exists, record `StartTime`, and detect migration strategy from ownerReferences.

Update each phase handler to:
- Record phase start/end times in `status.phaseTimings`
- Use `MsgClient` for queue operations in checkpointing/replaying/finalizing
- Use `KubeletClient` for checkpoint API in checkpointing
- Create transfer Job in transferring
- Create shadow/target pod in restoring
- Monitor queue depth and enforce cutoff in replaying
- Send END_REPLAY and cleanup in finalizing

The phase handlers should remain idempotent -- check if work is done before acting.

**Step 4: Update cmd/main.go to wire dependencies**

Add `kubernetes.NewForConfig()` and wire `KubeletClient` and `MsgClient` into the reconciler:

```go
clientset, err := kubernetes.NewForConfig(ctrl.GetConfigOrDie())
if err != nil {
    setupLog.Error(err, "unable to create kubernetes clientset")
    os.Exit(1)
}

if err = (&controller.StatefulMigrationReconciler{
    Client:        mgr.GetClient(),
    Scheme:        mgr.GetScheme(),
    KubeletClient: kubelet.NewClient(clientset),
    MsgClient:     rabbitmqmsg.NewRabbitMQClient(),
}).SetupWithManager(mgr); err != nil {
    // ...
}
```

**Step 5: Run tests**

Run:
```bash
go test ./internal/controller/ -v
```
Expected: All 3 tests PASS.

**Step 6: Verify full build**

Run:
```bash
go build -o /dev/null ./cmd/main.go
```
Expected: Succeeds.

**Step 7: Commit**

```bash
git add internal/controller/ cmd/main.go
git commit -m "feat: implement controller phase handlers with kubelet and messaging integration"
```

---

## Task 7: Update RBAC and CRD Manifests

Add permissions for Jobs, StatefulSets, and Nodes. Update the Dockerfile.

**Files:**
- Modify: `config/rbac/role.yaml`
- Modify: `Dockerfile`
- Modify: `Makefile`

**Step 1: Update RBAC**

Add to `config/rbac/role.yaml`:

```yaml
# Jobs for checkpoint transfer
- apiGroups:
  - batch
  resources:
  - jobs
  verbs:
  - create
  - delete
  - get
  - list
  - watch
# StatefulSet management
- apiGroups:
  - apps
  resources:
  - statefulsets
  - statefulsets/scale
  verbs:
  - get
  - list
  - patch
  - update
  - watch
# Node operations
- apiGroups:
  - ""
  resources:
  - nodes
  verbs:
  - get
  - list
```

**Step 2: Update Dockerfile to build from cmd/main.go**

Update `Dockerfile` to copy the full source tree and build from `cmd/main.go` instead of root `main.go`:

```dockerfile
FROM golang:1.25 as builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager ./cmd/main.go

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
```

**Step 3: Update Makefile if needed**

Verify `make build` still works (it already points to `cmd/main.go`).

Run:
```bash
make build
```
Expected: Binary created at `bin/manager`.

**Step 4: Commit**

```bash
git add config/rbac/role.yaml Dockerfile Makefile
git commit -m "feat: update RBAC with job and statefulset permissions, fix Dockerfile"
```

---

## Task 8: Evaluation Infrastructure Scripts

Automated IONOS VM provisioning and Kubernetes cluster setup.

**Files:**
- Create: `eval/infra/setup_ionos.sh`
- Create: `eval/infra/install_k8s.sh`
- Create: `eval/infra/teardown_ionos.sh`

**Step 1: Create IONOS provisioning script**

Create `eval/infra/setup_ionos.sh`:

Script that uses `ionosctl` to:
1. Create a datacenter in `de/txl`
2. Create a private LAN + public LAN
3. Create 3x VCPU servers (2 cores, 4GB RAM, Ubuntu 22.04) with SSH keys
4. Attach public IPs
5. Output the IP addresses to `eval/infra/hosts.env`

**Step 2: Create Kubernetes installation script**

Create `eval/infra/install_k8s.sh`:

Script that SSHes into each node and:
1. Installs CRI-O v1.28+ and CRIU
2. Configures kubelet with `--feature-gates=ContainerCheckpoint=true`
3. Runs `kubeadm init` on node 1 (control plane)
4. Runs `kubeadm join` on nodes 2 and 3
5. Installs RabbitMQ via Helm
6. Applies CRD and RBAC manifests
7. Copies kubeconfig to local machine

**Step 3: Create teardown script**

Create `eval/infra/teardown_ionos.sh`:

Script that uses `ionosctl` to delete the datacenter and all resources.

**Step 4: Commit**

```bash
git add eval/infra/
git commit -m "feat: add IONOS infrastructure provisioning scripts for evaluation"
```

---

## Task 9: Evaluation Workloads and Test Runner

Deploy the test producer/consumer and automate evaluation runs.

**Files:**
- Create: `eval/workloads/producer.yaml`
- Create: `eval/workloads/consumer.yaml`
- Create: `eval/workloads/rabbitmq-values.yaml`
- Create: `eval/scripts/run_evaluation.sh`
- Create: `eval/scripts/collect_metrics.sh`

**Step 1: Create producer deployment**

Create `eval/workloads/producer.yaml`:

A Deployment running a producer app that sends N messages/sec to RabbitMQ. Message rate configurable via environment variable `MSG_RATE`.

**Step 2: Create consumer StatefulSet**

Create `eval/workloads/consumer.yaml`:

A StatefulSet running a consumer app that processes messages from RabbitMQ with a configurable delay (`PROCESSING_DELAY_MS`, default 50ms). Includes a headless service.

**Step 3: Create RabbitMQ Helm values**

Create `eval/workloads/rabbitmq-values.yaml` with persistence enabled and resource limits.

**Step 4: Create evaluation runner**

Create `eval/scripts/run_evaluation.sh`:

Script that:
1. Loops over message rates: 1, 4, 7, 10, 13, 16, 19
2. For each rate, loops 10 times (repetitions)
3. Deploys producer with the rate, waits for steady state
4. Creates a `StatefulMigration` CR
5. Waits for `Completed` or `Failed` phase
6. Extracts `status.phaseTimings` and `status.startTime`
7. Appends metrics to CSV file

**Step 5: Create metrics collection script**

Create `eval/scripts/collect_metrics.sh`:

Script that reads `StatefulMigration` CRs via `kubectl get statefulmigrations -o json` and extracts:
- Total Migration Time
- Downtime per phase
- Checkpoint size
- Messages replayed

Outputs results as CSV for analysis.

**Step 6: Commit**

```bash
git add eval/
git commit -m "feat: add evaluation workloads and automated test runner"
```

---

## Task 10: Integration Tests with envtest

Test controller against a real (simulated) API server.

**Files:**
- Create: `internal/controller/suite_test.go`
- Modify: `internal/controller/statefulmigration_controller_test.go`

**Step 1: Add envtest dependency**

Run:
```bash
go get sigs.k8s.io/controller-runtime/pkg/envtest
```

**Step 2: Create test suite setup**

Create `internal/controller/suite_test.go`:

```go
package controller

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	migrationv1alpha1 "github.com/vibe-kanban/kubernetes-controller/api/v1alpha1"
)

var cfg *rest.Config
var testEnv *envtest.Environment

func TestMain(m *testing.M) {
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
		},
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		panic(err)
	}

	migrationv1alpha1.AddToScheme(scheme.Scheme)

	code := m.Run()
	testEnv.Stop()
	os.Exit(code)
}
```

**Step 3: Add integration test**

Add to `internal/controller/statefulmigration_controller_test.go`:

```go
func TestIntegration_CreateMigration_SetsPending(t *testing.T) {
	if cfg == nil {
		t.Skip("envtest not available")
	}
	// Create a real client from envtest config
	// Create a StatefulMigration CR
	// Verify status becomes Pending after reconciliation
}
```

**Step 4: Run tests**

Run:
```bash
go test ./internal/controller/ -v -count=1
```
Expected: All tests pass (envtest tests may be skipped if binaries not installed).

**Step 5: Commit**

```bash
git add internal/controller/ go.mod go.sum
git commit -m "test: add envtest integration tests for controller"
```

---

## Summary

| Task | Component | Effort |
|---|---|---|
| 1 | Fix build (add controller-runtime) | Small |
| 2 | Extend CRD types | Small |
| 3 | KubeletClient | Small |
| 4 | MessagingClient + RabbitMQ + Mock | Medium |
| 5 | Transfer binary | Small |
| 6 | Controller phase handlers | Large |
| 7 | RBAC + Dockerfile | Small |
| 8 | IONOS infra scripts | Medium |
| 9 | Eval workloads + test runner | Medium |
| 10 | Integration tests with envtest | Medium |

**Dependency order**: Task 1 -> Task 2 -> Tasks 3, 4, 5 (parallel) -> Task 6 -> Task 7 -> Tasks 8, 9, 10 (parallel)
