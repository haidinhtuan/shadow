# SHADOW

**Seamless Handoff And Zero-Downtime Orchestrated Workload Migration for Stateful Microservices**

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.30+-326CE5?logo=kubernetes&logoColor=white)](https://kubernetes.io)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

SHADOW is a Kubernetes operator that performs live migration of stateful microservices between cluster nodes with zero downtime and zero message loss. It combines CRIU-based container checkpointing with message queue replay to preserve both in-memory execution state and in-flight message consistency.

SHADOW builds on the MS2M (Message-based Stateful Microservice Migration) framework [[CIoT 2022]](https://ieeexplore.ieee.org/abstract/document/9766576), [[ICIN 2025]](https://ieeexplore.ieee.org/abstract/document/10942720) and introduces three key contributions:

- **ShadowPod migration strategy** -- Creates a shadow pod on the target node while the source continues serving traffic. Enables zero-downtime migration for both StatefulSet and Deployment workloads.
- **Direct node-to-node checkpoint transfer** -- A DaemonSet agent (`ms2m-agent`) receives checkpoint archives via HTTP and loads them into the local container runtime, bypassing the OCI registry entirely.
- **Kubernetes-native operator** -- Replaces the external Python migration manager with a declarative `StatefulMigration` custom resource and a controller-runtime reconciler.

## Architecture

```
                    Kubernetes API Server
                   /          |          \
                  /           |           \
      +-----------+   +---------------+   +------------+
      |  SHADOW   |   | Transfer Job  |   |  Registry  |
      | Operator  |   | (source node) |   |  (OCI)     |
      +-----------+   +---------------+   +------------+
           |                  |                  |
           |   +-----------+  |  +-----------+   |
           +-->| Source     |--+  | Target    |<--+
               | Kubelet   |     | Kubelet   |
               | (Node A)  |     | (Node B)  |
               +-----------+     +-----------+
                                      ^
                                      |
                                 +-----------+
                                 | ms2m-agent|  (Direct transfer mode)
                                 | DaemonSet |
                                 +-----------+
```

- **SHADOW Operator** -- Watches `StatefulMigration` custom resources and drives the phase-based state machine through each migration stage.
- **Source Kubelet** -- Executes the CRIU checkpoint via the kubelet checkpoint API, proxied through the API server.
- **Transfer Job** -- Ephemeral Job scheduled on the source node. Packages the checkpoint tarball as a single-layer OCI image. In Registry mode, pushes to a container registry. In Direct mode, POSTs to the ms2m-agent on the target node.
- **ms2m-agent** -- DaemonSet on each node (Direct transfer mode). Receives checkpoint tarballs via HTTP, builds the OCI image locally, and loads it into CRI-O via `skopeo copy`.
- **Target Kubelet** -- Pulls (or loads) the checkpoint image and restores the container on the target node.

## Migration Phases

A `StatefulMigration` resource progresses through a state machine:

```
Pending → Checkpointing → Transferring → Restoring → Replaying → Finalizing → Completed
                                                                                (or Failed)
```

| Phase | Description |
|:------|:------------|
| **Pending** | Validates the source pod, resolves owner references, caches pod metadata, auto-detects strategy. |
| **Checkpointing** | Creates a fanout exchange and replay queue on the message broker. Triggers CRIU checkpoint via the kubelet API. |
| **Transferring** | Launches a Transfer Job on the source node to build and transfer the OCI checkpoint image. |
| **Restoring** | Creates the target pod on the destination node. Sequential strategy scales the StatefulSet to zero first; ShadowPod creates the shadow pod alongside the still-running source. |
| **Replaying** | Sends `START_REPLAY` to the target pod. Monitors replay queue depth until drained or cutoff reached. |
| **Finalizing** | Sends `END_REPLAY`, tears down the replay queue. Removes the source (StatefulSet scale-down, Deployment deletion, or direct pod deletion depending on workload type). |

## Migration Strategies

### ShadowPod (zero downtime)

Creates a shadow pod (e.g., `consumer-0-shadow`) on the target node while the source pod continues serving traffic. Both pods coexist during the replay phase. During finalization:

- **Deployment-managed pods**: Source pod is deleted; owning Deployment is patched with `nodeAffinity` for the target node.
- **StatefulSet-managed pods**: The StatefulSet is scaled down by one replica. The shadow pod (carrying the app labels) continues serving traffic via the Service.

### Sequential (baseline)

For StatefulSet pods with strict identity requirements. Scales the StatefulSet to zero, waits for source termination, then creates the target pod with the same identity from the checkpoint image. During finalization, the controller removes its ownerReference from the target pod and scales the StatefulSet back up, allowing automatic adoption by the StatefulSet controller. Incurs ~38s downtime due to the StatefulSet scale-down/up cycle.

### Auto-Detection

When `migrationStrategy` is omitted, SHADOW inspects the source pod's `ownerReferences`:
- StatefulSet → defaults to **Sequential**
- Deployment/standalone → defaults to **ShadowPod**

Set `migrationStrategy: ShadowPod` explicitly to override auto-detection for StatefulSet workloads (enables zero-downtime migration at the cost of temporary orphaning until re-adoption is implemented).

## Checkpoint Transfer Modes

| Mode | Description |
|:-----|:------------|
| **Registry** (default) | Builds an uncompressed OCI image from the checkpoint, pushes to a container registry, pulled by the target kubelet. |
| **Direct** | Streams the checkpoint tarball to the `ms2m-agent` on the target node via HTTP. Agent builds the OCI image locally and loads into CRI-O, bypassing the registry. |

## Quick Start

### 1. Install the CRD

```bash
kubectl apply -f config/crd/bases/migration.ms2m.io_statefulmigrations.yaml
```

### 2. Deploy the operator

```bash
kubectl apply -f config/rbac/role.yaml
kubectl apply -f config/manager/manager.yaml
```

### 3. Create a migration

```yaml
apiVersion: migration.ms2m.io/v1alpha1
kind: StatefulMigration
metadata:
  name: migrate-consumer-0
  namespace: default
spec:
  sourcePod: consumer-0
  targetNode: worker-2
  checkpointImageRepository: registry.ms2m-system.svc:5000/checkpoints
  replayCutoffSeconds: 120
  migrationStrategy: ShadowPod     # or Sequential, or omit for auto-detection
  transferMode: Registry            # or Direct (requires ms2m-agent DaemonSet)
  messageQueueConfig:
    queueName: app.events
    brokerUrl: amqp://rabbitmq.default.svc:5672
    exchangeName: app.fanout
    routingKey: ""
```

### 4. Monitor progress

```bash
kubectl get statefulmigration migrate-consumer-0 -w
kubectl describe statefulmigration migrate-consumer-0
```

## Prerequisites

| Requirement | Details |
|:------------|:--------|
| **Kubernetes** | v1.30+ with `ContainerCheckpoint` feature gate enabled |
| **Container Runtime** | CRI-O with CRIU checkpoint/restore support |
| **CRIU** | Installed on all worker nodes |
| **Container Registry** | Accessible from all nodes (Registry transfer mode) |
| **Message Broker** | RabbitMQ (AMQP 0-9-1) |
| **Go** | v1.25+ (for building from source) |

## Development

```bash
make build          # Build binary to bin/manager
make run            # Run operator locally (uses ~/.kube/config)
make test           # Format, vet, run all tests with coverage
make docker-build   # Build Docker image
```

## Project Structure

```
cmd/
  main.go                              Operator entry point (controller-runtime manager)
  checkpoint-transfer/main.go          OCI image builder for checkpoint transfer
  ms2m-agent/main.go                   Node-local DaemonSet agent for direct transfer
api/v1alpha1/
  types.go                             StatefulMigration CRD type definitions
  groupversion_info.go                 API group registration
  deepcopy.go                          Deep copy functions
internal/
  controller/
    statefulmigration_controller.go    Reconciler with phase-based state machine
    statefulmigration_controller_test.go  Unit tests for all phases
  checkpoint/
    image.go                           Uncompressed OCI image builder
  kubelet/
    client.go                          Kubelet checkpoint API client
  messaging/
    client.go                          BrokerClient interface
    rabbitmq.go                        RabbitMQ implementation
    mock.go                            In-memory mock broker for tests
config/
  crd/bases/                           CRD YAML with OpenAPI v3 schema
  rbac/                                ClusterRole
  manager/                             Operator Deployment manifest
  daemonset/                           ms2m-agent DaemonSet + Service
eval/
  results/                             Evaluation CSV data (210 runs)
  workloads/                           Consumer workload manifests
  scripts/                             Evaluation and downtime measurement scripts
```

## Evaluation Results

Evaluated on a 3-node bare-metal Kubernetes cluster (dedicated servers from a European cloud provider, 4 vCPUs / 8 GB RAM per node, CRI-O + CRIU v4.0). Three configurations across seven message rates (10--120 msg/s), 10 repetitions each, totaling **210 migration runs**.

### Key Metrics

| Metric | Sequential (baseline) | ShadowPod |
|:-------|:---------------------|:----------|
| **Service downtime** | ~31 s | **0 ms** (140/140 runs) |
| **Restore phase** | ~38.5 s | ~2.9 s (92% reduction) |
| **Total time @ 10 msg/s** | 50.8 s | 12.4--13.8 s (73--76% reduction) |
| **Total time @ 120 msg/s** | 164.8 s | 129.0--129.2 s (22% reduction) |
| **Message loss** | 0 | 0 |

### Total Migration Time

```
Total Migration Time (seconds, median n=10)

  170 ┤ ■─────────────────────────────■──■──■
      │
  150 ┤          ■
      │
  130 ┤                      ●───────●──●──●
      │                      ○───────○──○──○
  110 ┤               ●
      │
   90 ┤         ■
      │
   70 ┤
      │                ○
   50 ┤■
      │               ●
   40 ┤
      │
   20 ┤●  ●  ●       ○  ○
      │○  ○  ○
    0 ┼───┬───┬───┬───┬───┬───┬───
      10  20  40  60  80 100 120  msg/s

  ■ SS-Sequential    ● SS-ShadowPod    ○ D-Registry
  ── Replay cutoff: 120s
```

At low rates (10 msg/s), ShadowPod reduces migration time by **73--76%** (50.8s to 12.4--13.8s). The improvement comes from eliminating the ~38s StatefulSet scale-down/up cycle. At high rates (>=80 msg/s), the 120s replay cutoff dominates all configurations, narrowing the gap to ~22%.

### Service Downtime

```
Service Downtime (seconds, median n=10)

   31 ┤■──■──■──■──■──■──■   Sequential: ~31s (all rates)
      │
      │
      │
      │
    0 ┤●──●──●──●──●──●──●   ShadowPod:   0ms (140/140 runs)
      ○──○──○──○──○──○──○   Deployment:  0ms (70/70 runs)
      ┼───┬───┬───┬───┬───┬───┬───
      10  20  40  60  80 100 120  msg/s
```

The ShadowPod strategy achieves **zero measured downtime** across all 140 StatefulSet-ShadowPod runs and all 70 Deployment runs. The Sequential baseline shows a consistent ~31s gap corresponding to the StatefulSet identity-constrained restore phase.

### Phase Breakdown at 60 msg/s

```
Phase Duration Breakdown (median seconds at 60 msg/s)

SS-Sequential  |▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓████████████████████████████████████████| 157.5s
               |ckpt|  transfer  |      restore (38.4s)      |    replay (112.8s)     |

SS-ShadowPod   |▓▓▓▓▓▓▓██|                                                    36.0s
               |ckpt|transfer|R|  replay (27.7s) |

D-Registry     |▓▓▓▓▓▓▓▓▓▓▓▓█████|                                            58.2s
               |ckpt|transfer|R|     replay (49.3s)    |

  ▓ Checkpoint + Transfer + Restore    █ Replay    R = Restore (~2.9s)
```

The restore phase -- which dominates Sequential at 38.4s -- is reduced to ~2.9s with ShadowPod (92% reduction), shifting the bottleneck entirely to the replay phase.

### Conclusions

- **ShadowPod eliminates service downtime** for both StatefulSet and Deployment workloads by keeping the source pod running throughout migration. Zero downtime was confirmed across all 210 ShadowPod runs.
- **Restore phase reduction of 92%** (38.4s to 2.9s) by creating an independent shadow pod instead of waiting for the StatefulSet scale-down/up cycle.
- **Replay is the remaining bottleneck** at high message rates. When the incoming rate exceeds the consumer's processing capacity (~65 msg/s), the replay cutoff fires and total migration time converges across all strategies.
- **Zero message loss** was maintained across all 210 runs, confirming the correctness of the MS2M message replay mechanism with the ShadowPod extension.

Raw evaluation data is available in [`eval/results/`](eval/results/).

To reproduce the evaluation:

```bash
# Run all 210 evaluation runs (3 configs x 7 rates x 10 reps)
./eval/scripts/run_all_evaluations.sh

# Run a single configuration
./eval/scripts/run_optimized_evaluation.sh statefulset-shadowpod 10,20,40,60,80,100,120 10
```

## Publications

SHADOW is the implementation of a multi-year research effort on live migration of stateful microservices:

1. **H. Dinh-Tuan and F. Beierle**, "MS2M: A Message-Based Approach for Live Stateful Microservices Migration," *2022 5th Conference on Cloud and Internet of Things (CIoT)*, 2022. [[IEEE]](https://ieeexplore.ieee.org/abstract/document/9766576)

2. **H. Dinh-Tuan and J. Jiang**, "Optimizing Stateful Microservice Migration in Kubernetes with MS2M and Forensic Checkpointing," *2025 28th Conference on Innovation in Clouds, Internet and Networks (ICIN)*, 2025. [[IEEE]](https://ieeexplore.ieee.org/abstract/document/10942720)

3. **H. Dinh-Tuan**, "SHADOW: Seamless Handoff And Zero-Downtime Orchestrated Workload Migration for Stateful Microservices," 2026. *(under review)*

## License

This project is licensed under the MIT License. See [LICENSE](LICENSE) for details.
