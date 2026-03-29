# MS2M Kubernetes Migration Controller - Design Document

## 1. Overview

This document describes the design for implementing the Message-based Stateful Microservice Migration (MS2M) controller for Kubernetes. The controller orchestrates live migration of stateful microservices by checkpointing running containers, transferring their state via OCI images, and synchronizing message queues for zero-loss handoffs.

The implementation targets scientific evaluation, reproducing and extending the results from the MS2M framework (Chapters 10-11 of the dissertation). Four migration strategies will be evaluated:

1. **Stop-and-Copy** (baseline)
2. **MS2M for Individual Pods** (shadow pod, concurrent replay)
3. **MS2M with Cutoff Mechanism** (bounded replay via T_cutoff)
4. **MS2M for StatefulSets** (sequential delete-then-restore)

## 2. Architecture

### 2.1 Four-Actor Model

Per the dissertation's Kubernetes integration (Ch. 11), the system comprises four actors:

| Actor | Role |
|---|---|
| **Migration Manager** | Custom controller running the 5-phase state machine |
| **API Server** | Proxies checkpoint requests to kubelets, manages pod/job lifecycle |
| **Source Node** | Hosts the source pod and checkpoint tarball |
| **Target Node** | Hosts the restored pod |

### 2.2 Components

| Component | Location | Purpose |
|---|---|---|
| Controller binary | `cmd/main.go` | Reconciler with state machine, Job orchestration |
| KubeletClient | `internal/kubelet/client.go` | Checkpoint API wrapper via API server proxy |
| MessagingClient | `internal/messaging/` | RabbitMQ fanout exchange, control messages, queue monitoring |
| Transfer binary | `cmd/checkpoint-transfer/main.go` | Builds OCI image from checkpoint tar, pushes to registry |
| Transfer Job image | `Dockerfile.transfer` | Minimal container for the transfer binary |

### 2.3 Approach: Controller + Transfer Job

The controller orchestrates all phases via the Kubernetes API but delegates node-local work to Kubernetes Jobs. This separation is necessary because the checkpoint tarball exists only on the source node's filesystem.

- **Controller** (runs anywhere): Calls checkpoint API (proxied through API server), manages CRD state, handles RabbitMQ queue setup, creates/monitors Jobs and Pods
- **Transfer Job** (runs on source node): Scheduled via `nodeName`, mounts `/var/lib/kubelet/checkpoints/` via `hostPath`, uses `go-containerregistry` to build and push OCI image

Alternatives considered:
- **Monolithic controller**: Cannot access checkpoint files on remote nodes. Not viable.
- **DaemonSet agent**: Over-engineered. Wastes resources on idle nodes. Not used in the dissertation.

## 3. CRD Design

### 3.1 StatefulMigrationSpec

```go
type StatefulMigrationSpec struct {
    SourcePod                 string             `json:"sourcePod"`
    TargetNode                string             `json:"targetNode,omitempty"`
    CheckpointImageRepository string            `json:"checkpointImageRepository"`
    ReplayCutoffSeconds       int32              `json:"replayCutoffSeconds,omitempty"`
    MessageQueueConfig        MessageQueueConfig `json:"messageQueueConfig,omitempty"`
    MigrationStrategy         string             `json:"migrationStrategy,omitempty"`
}
```

`MigrationStrategy` accepts:
- `"ShadowPod"` - Creates `<name>-shadow` pod on target node. Source and target run concurrently during replay. For individual pods or Deployment-managed pods.
- `"Sequential"` - Deletes source pod before creating target. Required for StatefulSet pods where identity collision prevents concurrent existence.

If unset, the controller auto-detects based on whether the source pod has StatefulSet `ownerReferences`.

### 3.2 StatefulMigrationStatus

```go
type StatefulMigrationStatus struct {
    Phase        Phase              `json:"phase,omitempty"`
    SourceNode   string             `json:"sourceNode,omitempty"`
    CheckpointID string             `json:"checkpointID,omitempty"`
    TargetPod    string             `json:"targetPod,omitempty"`
    StartTime    *metav1.Time       `json:"startTime,omitempty"`
    PhaseTimings map[string]string  `json:"phaseTimings,omitempty"`
    Conditions   []metav1.Condition `json:"conditions,omitempty"`
}
```

`PhaseTimings` records duration per phase (e.g., `{"Checkpointing": "2.3s", "Transferring": "8.1s"}`). Combined with `StartTime`, this enables computing Total Migration Time and Downtime for evaluation.

### 3.3 MessageQueueConfig

```go
type MessageQueueConfig struct {
    BrokerURL    string `json:"brokerUrl,omitempty"`
    QueueName    string `json:"queueName,omitempty"`
    ExchangeName string `json:"exchangeName,omitempty"`
    RoutingKey   string `json:"routingKey,omitempty"`
}
```

## 4. Phase Implementation

### 4.1 Phase 1: Checkpointing

**Entry**: `handleCheckpointing()`

Steps:
1. Connect to RabbitMQ via `spec.messageQueueConfig.brokerUrl`
2. Create fanout exchange bound to both primary queue and new secondary queue (`<queue>.ms2m-replay`). From this point, all new messages flow to both queues.
3. Resolve source pod's node: `pod.Spec.NodeName`
4. Call Kubelet Checkpoint API:
   ```
   POST /api/v1/nodes/{nodeName}/proxy/checkpoint/{namespace}/{pod}/{container}
   ```
   Using `client-go` RESTClient: `.Resource("nodes").Name(node).SubResource("proxy", "checkpoint", ns, pod, container).Do(ctx)`
5. Parse response to get checkpoint tar path (stored in `/var/lib/kubelet/checkpoints/`)
6. Store path in `status.checkpointID`, record phase timing
7. Transition to `Transferring`

**Error handling**: Checkpoint API failure requeues with 10s backoff. After 3 retries, transition to `Failed`.

### 4.2 Phase 2: Transferring

**Entry**: `handleTransferring()`

Steps:
1. Check if transfer Job already exists (idempotency)
2. Create a Kubernetes Job:
   - `nodeName` set to `status.sourceNode` (must run on source node)
   - `hostPath` volume mounting `/var/lib/kubelet/checkpoints/`
   - Runs the transfer binary with args: checkpoint path, target registry
   - Registry credentials via Kubernetes Secret or Workload Identity
3. Watch Job for completion (`job.Status.Succeeded > 0`)
4. On success, transition to `Restoring`
5. On failure, record error in conditions, transition to `Failed`

**Transfer binary** (`cmd/checkpoint-transfer/main.go`):
Uses `github.com/google/go-containerregistry` (crane library):
```go
layer, _ := tarball.LayerFromFile(checkpointPath)
img, _ := mutate.AppendLayers(empty.Image, layer)
crane.Push(img, imageRef)
```

This is ~20 lines of Go. No privileged access needed. No external binary dependencies.

### 4.3 Phase 3: Restoring

**Entry**: `handleRestoring()`

Branches based on `spec.migrationStrategy`:

**ShadowPod strategy** (individual pods):
1. Clone source pod spec via `DeepCopy()`
2. Set name to `<sourcePod>-shadow`
3. Set `spec.hostname` to original pod name (preserves internal identity)
4. Set `spec.subdomain` to headless service name (enables DNS registration)
5. Override container image with checkpoint image from registry
6. Set `spec.nodeName` to `spec.targetNode`
7. Remove `ownerReferences` (shadow pod is standalone)
8. Create the pod. Wait for `Running` status.

**Sequential strategy** (StatefulSet pods):
1. Delete source pod
2. Wait for termination confirmation
3. Create target pod on target node with checkpoint image
4. Wait for `Running` status

Store target pod name in `status.targetPod`, transition to `Replaying`.

### 4.4 Phase 4: Replaying

**Entry**: `handleReplaying()`

Steps:
1. Send `START_REPLAY` control message to target pod's control queue (`ms2m.control.<targetPod>`)
2. Record `ReplayStarted` condition with timestamp
3. Poll secondary queue depth every 2s using AMQP `QueueDeclarePassive`
4. **Cutoff check**: If elapsed time since `ReplayStarted` >= `spec.replayCutoffSeconds`:
   - Delete source pod (for ShadowPod strategy; already deleted in Sequential)
   - This freezes the secondary queue (no new messages being mirrored)
   - The remaining messages form a finite, bounded batch
5. When queue depth reaches 0, transition to `Finalizing`

**Cutoff formula** (from dissertation Section 11.3.2):
```
T_cutoff = T_accum <= (T_replay_max * mu_target) / lambda
```
The `spec.replayCutoffSeconds` maps to T_cutoff. When set, the migration time is bounded regardless of message rate.

### 4.5 Phase 5: Finalizing

**Entry**: `handleFinalizing()`

Steps:
1. Send `END_REPLAY` control message to target pod
2. Target pod exits replay mode, switches subscription from secondary to primary queue
3. Delete source pod (if still running, i.e., ShadowPod without cutoff)
4. Delete secondary queue and fanout exchange
5. Close broker connection
6. Record final phase timings
7. Transition to `Completed`

## 5. Messaging Architecture

### 5.1 Fanout Exchange Pattern

RabbitMQ message mirroring uses a fanout exchange with dual queue bindings:

```
                        +--> [Primary Queue]   --> Source Pod (consuming)
[Producer] --> [Fanout] |
                        +--> [Secondary Queue] --> accumulates during migration
```

This is native AMQP (no plugins like Shovel or Federation needed). Setup and teardown are atomic channel operations.

### 5.2 Control Messages

Control signals use a **separate queue per pod** (`ms2m.control.<podName>`), not inline message headers. This ensures control messages are received immediately regardless of data queue depth.

Two control message types:
- `START_REPLAY`: Target pod enters replay mode (suppresses side-effects), starts consuming from secondary queue
- `END_REPLAY`: Target pod exits replay mode, switches to primary queue for live traffic

### 5.3 Queue Depth Monitoring

Primary method: AMQP `QueueDeclarePassive` (protocol-level, no plugins required).
Supplementary: RabbitMQ Management API (`GET /api/queues/{vhost}/{name}`) for consume rate metrics.

Note: `QueueDeclarePassive` closes the channel on error per AMQP spec. Implementation recovers by reopening the channel.

### 5.4 MessageBrokerClient Interface

```go
type MessageBrokerClient interface {
    Connect(ctx context.Context, brokerURL string) error
    Close() error
    CreateSecondaryQueue(ctx context.Context, primaryQueueName string) (string, error)
    DeleteSecondaryQueue(ctx context.Context, secondaryQueueName string) error
    GetQueueDepth(ctx context.Context, queueName string) (int, error)
    SendControlMessage(ctx context.Context, targetPod string, msgType ControlMessageType, payload map[string]interface{}) error
}
```

Abstracting behind an interface allows swapping RabbitMQ for NATS or another broker, and enables mock-based unit testing.

## 6. Error Handling

### 6.1 Retry Strategy

Each phase handler follows a consistent pattern:
- **Transient failures** (network errors, API timeouts): `ctrl.Result{RequeueAfter: N * time.Second}` with exponential backoff
- **Permanent failures** (pod not found, invalid CRD spec): Set `status.phase = Failed` with descriptive condition
- **Idempotency**: Every phase checks if its work is already done before acting (e.g., Job exists? Pod exists?)

### 6.2 Condition Tracking

Standard Kubernetes conditions track sub-state within each phase:
- `CheckpointCreated` (True/False)
- `TransferJobCompleted` (True/False)
- `TargetPodReady` (True/False)
- `ReplayStarted` (True/False)
- `ReplayCompleted` (True/False)

### 6.3 Cleanup on Failure

If any phase transitions to `Failed`:
- Delete transfer Job (if exists)
- Delete shadow pod (if exists)
- Delete secondary queue and fanout exchange (if created)
- Source pod remains untouched (no data loss on failure)

## 7. RBAC Requirements

```yaml
rules:
  # CRD management
  - apiGroups: ["migration.ms2m.io"]
    resources: ["statefulmigrations", "statefulmigrations/status"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  # Checkpoint API (proxied through API server)
  - apiGroups: [""]
    resources: ["nodes/proxy"]
    verbs: ["create", "get"]
  # Pod lifecycle
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch", "create", "delete"]
  # Transfer Jobs
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["get", "list", "watch", "create", "delete"]
  # StatefulSet management (for Sequential strategy)
  - apiGroups: ["apps"]
    resources: ["statefulsets", "statefulsets/scale"]
    verbs: ["get", "list", "patch", "update", "watch"]
  # Node operations (cordon for StatefulSet migration)
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "patch", "update"]
```

## 8. Infrastructure & Runtime

### 8.1 Runtime Requirements

| Component | Version | Notes |
|---|---|---|
| Kubernetes | 1.30+ | `ContainerCheckpoint` beta (enabled by default) |
| CRI-O | 1.28+ | Primary runtime, proven in dissertation evaluation |
| CRIU | 3.17+ | Required on all worker nodes |
| RabbitMQ | 3.13+ | Quorum queue support for secondary queue durability |
| Go | 1.25+ | Controller and transfer binary |

The dissertation validated with CRI-O v1.28. containerd v2.0+ is an alternative but less mature for FCC.

### 8.2 Development Environment (Local)

- **kind** cluster for fast iteration
- Kubelet Checkpoint API is mocked (kind nodes don't have CRIU)
- Unit tests with mock clients, integration tests with `envtest`

### 8.3 Evaluation Environment (IONOS Cloud)

3x VCPU servers (2 cores, 4GB RAM) with Ubuntu 22.04, self-managed Kubernetes via kubeadm:

```
Node 1: Control plane + Migration Controller
Node 2: Worker (Source Node)
Node 3: Worker (Target Node)
```

Stack: kubeadm + CRI-O + CRIU + RabbitMQ + Google Artifact Registry (or local registry).

Setup automated via `ionosctl` scripts:
1. Create datacenter + private LAN + 3 VMs
2. Install CRI-O + CRIU on all nodes
3. Bootstrap K8s cluster with `--feature-gates=ContainerCheckpoint=true`
4. Deploy RabbitMQ via Helm
5. Deploy the MS2M controller

VMs are provisioned on-demand for evaluation runs and torn down after to minimize cost.

## 9. Evaluation Framework

### 9.1 Test Workloads

Two microservice types (matching dissertation Section 11.5.1):

- **Producer**: Deployment that continuously sends messages to RabbitMQ at a configurable rate (N msg/s)
- **Consumer**: StatefulSet that consumes messages from RabbitMQ with a configurable processing delay (default 50ms, giving max capacity of 20 msg/s)

### 9.2 Configurations

Four migration strategies evaluated:

| Strategy | Description |
|---|---|
| Stop-and-Copy | Baseline: delete pod, recreate on target |
| MS2M Individual | Shadow pod, unbounded replay |
| MS2M + Cutoff | Shadow pod, T_cutoff = 120s |
| MS2M StatefulSet | Sequential delete-then-restore |

### 9.3 Parameters

- **Message rates**: 1, 4, 7, 10, 13, 16, 19 msg/s
- **Processing time**: 50ms (fixed)
- **Repetitions**: 10 per configuration
- **T_cutoff**: 120s (when cutoff is enabled)

### 9.4 Metrics

Collected from `status.phaseTimings` and `status.startTime`:

| Metric | Definition |
|---|---|
| **Total Migration Time** | Time from CR creation to `Completed` phase |
| **Downtime** | Duration where no instance can process new messages |
| **Checkpoint Size** | Size of the checkpoint OCI image |
| **Messages Replayed** | Number of messages in secondary queue at replay start |
| **Phase Breakdown** | Time per phase (for latency analysis charts) |

### 9.5 Expected Results

Based on the dissertation's evaluation (Section 11.5):
- MS2M for individual pods: ~97% downtime reduction vs stop-and-copy at 16 msg/s
- MS2M with cutoff: ~82% reduction in total migration time vs uncapped MS2M at 19 msg/s
- MS2M for StatefulSets: ~37% downtime reduction at low rates, diminishing at high rates

## 10. File Structure

```
cmd/
  main.go                                  # Controller entrypoint (existing)
  checkpoint-transfer/
    main.go                                # Transfer binary (new)
internal/
  controller/
    statefulmigration_controller.go         # Phase handlers (modify)
    statefulmigration_controller_test.go    # Unit tests (new)
  kubelet/
    client.go                              # Checkpoint API wrapper (new)
  messaging/
    client.go                              # MessageBrokerClient interface (new)
    rabbitmq.go                            # RabbitMQ implementation (new)
api/v1alpha1/
  types.go                                # CRD types (modify)
  deepcopy.go                             # Regenerate after types change (modify)
config/
  rbac/role.yaml                          # Add Job, StatefulSet, Node perms (modify)
  crd/bases/                              # Regenerate CRD YAML (modify)
eval/
  infra/
    setup_ionos.sh                         # IONOS VM provisioning (new)
    install_k8s.sh                         # kubeadm + CRI-O + CRIU setup (new)
    teardown_ionos.sh                      # Cleanup (new)
  workloads/
    producer.yaml                          # Test producer Deployment (new)
    consumer.yaml                          # Test consumer StatefulSet (new)
    rabbitmq-values.yaml                   # RabbitMQ Helm values (new)
  scripts/
    run_evaluation.sh                      # Automated evaluation runner (new)
    collect_metrics.sh                     # Metrics extraction from CRD status (new)
Dockerfile.transfer                        # Transfer Job container image (new)
```
