# MS2M Controller

A Kubernetes controller for live migration of stateful microservices between nodes. It uses CRIU-based container checkpointing and message replay to preserve in-memory state and ensure zero message loss during pod migration.

This project implements the **Message-based Stateful Microservice Migration (MS2M)** framework, originally developed as part of a master's thesis on live migration of stateful microservices in Kubernetes environments.

## Table of Contents

- [Motivation](#motivation)
- [Architecture](#architecture)
- [Migration Phases](#migration-phases)
- [CRD Reference](#crd-reference)
- [Migration Strategies](#migration-strategies)
- [Quick Start](#quick-start)
- [Prerequisites](#prerequisites)
- [Project Structure](#project-structure)
- [Building](#building)
- [Development](#development)
- [Evaluation](#evaluation)
- [License](#license)

## Motivation

Standard Kubernetes StatefulSets provide stable identities and persistent storage, but they do not preserve **execution state** (in-memory data) when pods are rescheduled. A pod eviction or node drain causes cold restarts, connection drops, and loss of all cached data.

MS2M addresses this gap:

| Aspect | StatefulSet (default) | MS2M Controller |
|:---|:---|:---|
| **Migration mechanism** | Delete and recreate (destructive) | Checkpoint and restore (preservative) |
| **In-memory state** | Lost | Preserved via CRIU |
| **Transfer method** | Detach/attach PV | OCI image (fast, portable) |
| **Pod coexistence** | Impossible (name collision) | Shadow pods (`pod-shadow`) |
| **Data consistency** | Crash recovery | Message replay (zero-loss handoff) |

## Architecture

The controller follows a **Controller + Transfer Job** pattern with four cooperating actors:

```
                  Kubernetes API Server
                 /          |          \
                /           |           \
    +-----------+   +---------------+   +------------+
    | Controller|   | Transfer Job  |   |  Registry  |
    | (manager) |   | (source node) |   |  (OCI)     |
    +-----------+   +---------------+   +------------+
         |                  |                  |
         |   +-----------+  |  +-----------+   |
         +-->| Source     |--+  | Target    |<--+
             | Kubelet   |     | Kubelet   |
             | (Node A)  |     | (Node B)  |
             +-----------+     +-----------+
```

- **Controller** -- Runs as a Deployment in the cluster. Watches `StatefulMigration` custom resources and drives the state machine through each migration phase by interacting with the Kubernetes API.
- **Source Kubelet** -- Executes the CRIU checkpoint on the source node via the kubelet checkpoint API (`POST /checkpoint/{namespace}/{pod}/{container}`), proxied through the API server.
- **Transfer Job** -- An ephemeral pod scheduled on the source node with access to the local checkpoint archive. It packages the checkpoint tarball as a single-layer OCI image and pushes it to the container registry.
- **Target Kubelet** -- Pulls the checkpoint image from the registry and restores the container with its full in-memory state on the target node.

## Migration Phases

A `StatefulMigration` resource progresses through five phases, each handled by a dedicated phase handler in the reconciler:

```
 +-----------+     +----------------+     +--------------+
 |  Pending  |---->| Checkpointing  |---->| Transferring |
 +-----------+     +----------------+     +--------------+
                                                |
                                                v
                   +--------------+     +--------------+
                   |  Finalizing  |<----| Restoring    |
                   +--------------+     +--------------+
                         |                      |
                         v                      v
                   +-----------+         +-----------+
                   | Completed |         | Replaying |---+
                   +-----------+         +-----------+   |
                                               ^         |
                                               +---------+
                                            (monitor lag)
```

### Phase 1: Checkpointing

The controller creates a secondary message queue (fan-out) so that new messages arriving after this point are duplicated to both the primary and replay queues. It then calls the kubelet checkpoint API on the source node to create a CRIU checkpoint of the running container.

### Phase 2: Transferring

A Transfer Job pod is scheduled on the source node. It reads the checkpoint tarball from the local filesystem, packages it as a single-layer OCI image using [go-containerregistry](https://github.com/google/go-containerregistry), and pushes it to the configured container registry.

### Phase 3: Restoring

The controller creates a new pod on the target node using the checkpoint image. Depending on the migration strategy, this may be a shadow pod (running alongside the source) or a replacement pod (created after the source is deleted).

### Phase 4: Replaying

The controller sends a `START_REPLAY` control message to the target pod, instructing it to consume and process messages from the secondary replay queue. The controller monitors the replay queue depth and triggers cutoff when the lag falls below the configured `replayCutoffSeconds` threshold.

### Phase 5: Finalizing

The controller sends an `END_REPLAY` control message, switches traffic from the source to the target pod, deletes the source pod, tears down the secondary queue and fan-out exchange, and marks the migration as `Completed`.

## CRD Reference

The controller defines a single Custom Resource: `StatefulMigration` in API group `migration.ms2m.io/v1alpha1`.

### Spec

```yaml
apiVersion: migration.ms2m.io/v1alpha1
kind: StatefulMigration
metadata:
  name: migrate-worker-0
  namespace: default
spec:
  # Name of the pod to migrate (must be in the same namespace)
  sourcePod: worker-0

  # Target node for the restored pod (optional; uses scheduler if omitted)
  targetNode: node-b

  # Registry location to push the checkpoint OCI image
  checkpointImageRepository: registry.example.com/checkpoints/worker-0

  # Seconds of replay lag below which to trigger final cutoff
  replayCutoffSeconds: 5

  # Message queue configuration for replay
  messageQueueConfig:
    queueName: tasks
    brokerUrl: amqp://rabbitmq.default.svc:5672
    exchangeName: tasks.fanout
    routingKey: tasks

  # Migration strategy: "ShadowPod" or "Sequential" (auto-detected if omitted)
  migrationStrategy: ShadowPod
```

### Status

The controller populates the following status fields during the migration:

| Field | Type | Description |
|:---|:---|:---|
| `phase` | string | Current migration phase (`Pending`, `Checkpointing`, `Transferring`, `Restoring`, `Replaying`, `Finalizing`, `Completed`, `Failed`) |
| `sourceNode` | string | Node where the source pod is running |
| `checkpointID` | string | Identifier of the created CRIU checkpoint |
| `targetPod` | string | Name of the restored pod on the target node |
| `startTime` | datetime | Timestamp when the migration was initiated |
| `phaseTimings` | map[string]string | Duration of each completed phase |
| `conditions` | []Condition | Standard Kubernetes conditions for detailed status |

## Migration Strategies

### ShadowPod (default for standalone pods)

The controller creates a shadow pod (e.g., `worker-0-shadow`) on the target node while the source pod is still running. Both pods coexist during the replay phase, ensuring the target can catch up before traffic is switched. This is the default strategy for pods that are not owned by a StatefulSet.

### Sequential (required for StatefulSets)

StatefulSet pods have strict identity requirements -- `worker-0` cannot exist as two pods simultaneously. The Sequential strategy deletes the source pod before creating the target pod with the same identity. This avoids name collisions but introduces a brief period where the pod is unavailable.

### Auto-detection

If `migrationStrategy` is left empty, the controller inspects the source pod's `ownerReferences`:
- If owned by a StatefulSet, the strategy defaults to `Sequential`.
- Otherwise, it defaults to `ShadowPod`.

## Quick Start

### 1. Install the CRD

```bash
kubectl apply -f config/crd/bases/migration.ms2m.io_statefulmigrations.yaml
```

### 2. Deploy the controller

```bash
kubectl apply -f config/rbac/role.yaml
kubectl apply -f config/manager/manager.yaml
```

### 3. Create a migration

```yaml
apiVersion: migration.ms2m.io/v1alpha1
kind: StatefulMigration
metadata:
  name: migrate-worker-0
  namespace: default
spec:
  sourcePod: worker-0
  targetNode: node-b
  checkpointImageRepository: registry.example.com/checkpoints/worker-0
  replayCutoffSeconds: 5
  messageQueueConfig:
    queueName: tasks
    brokerUrl: amqp://rabbitmq.default.svc:5672
    exchangeName: tasks.fanout
    routingKey: tasks
```

```bash
kubectl apply -f migration.yaml
```

### 4. Monitor progress

```bash
# Watch migration status
kubectl get statefulmigration migrate-worker-0 -w

# Inspect detailed status
kubectl describe statefulmigration migrate-worker-0
```

## Prerequisites

- **Kubernetes 1.30+** with the `ContainerCheckpoint` feature gate enabled
- **CRI-O** or **containerd** configured with CRIU support for container checkpointing
- **Container registry** accessible from all cluster nodes (for checkpoint image transfer)
- **RabbitMQ** (or compatible AMQP 0-9-1 broker) for message queue operations
- **Go 1.25+** (for building from source)

## Project Structure

```
cmd/main.go                          Controller entry point
cmd/checkpoint-transfer/             Transfer binary (OCI image builder)
api/v1alpha1/                        CRD type definitions and deepcopy
  types.go                           StatefulMigration spec/status structs
  groupversion_info.go               API group registration
  deepcopy.go                        Generated deep copy functions
internal/controller/                 Reconciler and phase handlers
  statefulmigration_controller.go    Main reconciliation loop (state machine)
internal/kubelet/                    Kubelet checkpoint API client
  client.go                          Checkpoint request via API server proxy
internal/messaging/                  Message broker interface and implementations
  client.go                          BrokerClient interface definition
  rabbitmq.go                        RabbitMQ (AMQP 0-9-1) implementation
  mock.go                            In-memory mock for unit tests
config/crd/bases/                    CRD YAML (OpenAPI v3 schema)
config/rbac/                         RBAC ClusterRole for the controller
config/manager/                      Controller Deployment manifest
manifests/                           Legacy Kubernetes manifests
terraform/                           GKE cluster provisioning (Terraform)
docs/                                Design documents and plans
```

## Building

```bash
# Build the controller binary
make build

# Build the Docker image (runs tests first)
make docker-build

# Build with a custom image tag
make docker-build IMG=registry.example.com/ms2m-controller:v0.1.0

# Push the image to a registry
make docker-push IMG=registry.example.com/ms2m-controller:v0.1.0

# Build the checkpoint transfer image
docker build -t checkpoint-transfer:latest -f Dockerfile.transfer .
```

## Development

### Running locally

The controller can run outside the cluster for development, using your local kubeconfig:

```bash
# Ensure your kubeconfig points to the target cluster
kubectl cluster-info

# Install the CRD
kubectl apply -f config/crd/bases/migration.ms2m.io_statefulmigrations.yaml

# Run the controller
make run
```

### Running tests

```bash
# Run all tests with coverage
make test

# Run tests for a specific package
go test ./internal/messaging/... -v
go test ./internal/kubelet/... -v
go test ./cmd/checkpoint-transfer/... -v
```

### Code formatting and vetting

```bash
make fmt
make vet
```

### Deploying to a cluster

1. Build and push the controller and transfer images:

```bash
export REGISTRY=registry.example.com
make docker-build IMG=$REGISTRY/ms2m-controller:latest
make docker-push IMG=$REGISTRY/ms2m-controller:latest
docker build -t $REGISTRY/checkpoint-transfer:latest -f Dockerfile.transfer .
docker push $REGISTRY/checkpoint-transfer:latest
```

2. Update `config/manager/manager.yaml` with your image reference.

3. Apply all resources:

```bash
kubectl apply -f config/crd/bases/migration.ms2m.io_statefulmigrations.yaml
kubectl apply -f config/rbac/role.yaml
kubectl apply -f config/manager/manager.yaml
```

4. Verify the controller is running:

```bash
kubectl get pods -l control-plane=controller-manager
kubectl logs -l control-plane=controller-manager -f
```

## Evaluation

The evaluation infrastructure uses bare-metal cloud VMs provisioned with `kubeadm`. The cluster runs CRI-O as the container runtime with CRIU compiled from source to support forensic container checkpointing.

The test workload is a stateful message-processing microservice backed by RabbitMQ. Evaluation scenarios measure:

- **Checkpoint duration** -- time to freeze and serialize container state
- **Transfer duration** -- time to package and push the OCI checkpoint image
- **Restore duration** -- time to pull the image and resume the container
- **Replay lag** -- time to drain the secondary queue and reach consistency
- **Total downtime** -- end-to-end unavailability window as perceived by clients
- **Message loss** -- number of messages dropped during migration (target: zero)

## License

This project is licensed under the MIT License. See [LICENSE](LICENSE) for details.
