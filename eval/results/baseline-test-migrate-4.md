# Baseline Migration Metrics (test-migrate-4)

Date: 2026-02-16
Cluster: IONOS bare-metal, 3 nodes (Ubuntu 22.04, K8s 1.31, CRI-O 1.31, CRIU 4.0)
Migration: consumer-0 from worker-1 → worker-2 (Sequential/StatefulSet strategy)
Message rate: ~10 msg/s (producer default)
Processing delay: 50ms per message (max capacity: 20 msg/s)

## Infrastructure

- control-plane: 217.154.88.108
- worker-1: 194.164.195.44 (source)
- worker-2: 217.154.232.241 (target)
- Container runtime: CRI-O 1.31 with custom crun (dynamically linked, --tcp-close patch)
- Checkpoint mechanism: Forensic Container Checkpointing (kubelet API)
- Registry: in-cluster Docker registry (ClusterIP 10.99.85.209:5000)
- Message broker: RabbitMQ (in-cluster)

## Phase Timings

| Phase | Duration | % of Total | Notes |
|-------|----------|-----------|-------|
| Checkpointing | 478ms | 0.9% | Kubelet FCC API → CRIU checkpoint + replay queue setup |
| Transferring | 5.9s | 10.6% | OCI image build (no compression) + registry push |
| Restoring | 39.1s | 70.2% | StatefulSet scale-down, pod scheduling, image pull, CRIU restore |
| Replaying | 10.2s | 18.3% | Drain replay queue via START_REPLAY/END_REPLAY control messages |
| Finalizing | 26ms | <0.1% | Scale-up StatefulSet, cleanup |
| **Total** | **~55.7s** | **100%** | |

## Phase Breakdown Analysis

### Checkpointing (478ms)
- FCC reduces checkpoint overhead from 88.04% (Ch10 Podman) to <1%
- Includes: CRIU freeze, memory dump, tarball creation, container resume
- Also creates fanout exchange + replay queue binding

### Transferring (5.9s)
- Packaging checkpoint tarball as OCI image (no gzip, raw layer)
- Push to in-cluster registry over HTTP
- Bottleneck: image build + push, not network

### Restoring (39.1s) -- DOMINANT BOTTLENECK
- StatefulSet scale-down to 0 (release pod identity)
- Wait for source pod termination
- StatefulSet scale-up on target node (with nodeAffinity)
- Kubelet pulls checkpoint image from registry
- CRIU restore (with --tcp-established --tcp-close)
- Wait for pod readiness
- This matches dissertation Ch11 finding: Service Restoration dominates for StatefulSets

### Replaying (10.2s)
- Controller sends START_REPLAY to target pod's control queue
- Target pod switches to replay queue, processes buffered messages
- Controller polls queue depth until 0
- Controller triggers Finalizing when queue is drained

### Finalizing (26ms)
- Send END_REPLAY to target pod
- Target pod switches back to primary queue
- Delete replay queue and fanout exchange
- Scale StatefulSet back up (already running on target)

## Comparison with Dissertation

| Metric | Ch10 (Podman) | Ch11 (K8s, StatefulSet) | Our Implementation |
|--------|--------------|------------------------|-------------------|
| Platform | GCP e2-medium VMs | GCE e2-medium VMs | IONOS bare-metal |
| Runtime | Podman 3.1.0 | CRI-O 1.28 | CRI-O 1.31 |
| Consumer | Java 17 Spring Boot | Java 17 Spring Boot | Python 3.11 |
| Transfer | Rsync/SSH + Buildah | Rsync/SSH + Buildah | OCI registry |
| Migration Manager | Python script | Python script | K8s controller (Go) |
| Checkpoint+Restore % | 88.04% | 36.30% | ~71% (dominated by K8s orchestration) |
| Total time (low rate) | 4.85s | ~53s | ~55.7s |
| Downtime reduction vs baseline | 19.92% | 36.69% (1 msg/s) | N/A (single test) |

## Resolved Issues (since baseline-e2e-test-8)

1. Replay queue now works correctly (fan-out exchange pattern)
2. Consumer reconnects after CRIU restore (--tcp-close + reconnection logic)
3. StatefulSet migration handled (scale-down/up strategy)
4. Target pod has full spec from source (labels, ports, env)
5. Transfer uses no compression (faster for in-cluster)
6. ImagePullPolicy: Always prevents stale checkpoint cache
7. Phase chaining reduces reconciliation overhead
8. OCI manifest format (not Docker v2) for CRI-O compatibility

## Optimization Opportunities

### 1. Restoring Phase (39.1s → target: <10s)
- **Pre-pull checkpoint image** on target node before scale-down
- **Warm pod / pre-created pod** to avoid scheduling overhead
- **Parallel scale-down/restore** instead of sequential
- **Skip StatefulSet controller** by using direct pod manipulation

### 2. Transferring Phase (5.9s → target: <2s)
- **Direct node-to-node transfer** (rsync/scp) instead of registry round-trip
- **Streaming transfer** (pipe checkpoint directly to target)
- **Compress checkpoint** if network is the bottleneck (currently not)

### 3. Replaying Phase (10.2s → target: <5s)
- **Batch acknowledgment** in consumer for faster replay
- **Pre-fetch messages** before restore completes
- **Reduce processing delay** during replay mode
