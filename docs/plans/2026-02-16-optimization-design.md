# MS2M Controller Optimization Design

Date: 2026-02-16
Status: Approved
Baseline: test-migrate-4 (~55.7s total, Sequential strategy on StatefulSet)

## Goal

Reduce total migration time from ~55.7s to <15s (target: <10s with pre-dumps) while
maintaining zero message loss. Keep the existing StatefulSet Sequential baseline for
comparison; add Deployment-based ShadowPod as the optimized path.

## Three Optimizations

### 1. Deployment-based Workload (Restoring: 39.1s -> ~3-5s)

**Problem:** StatefulSet pod identity constraints force a scale-down/up cycle during
migration. This 39.1s Restoring phase dominates total time. StatefulSets are designed
for workloads that need stable identity (databases, brokers), but message-consuming
microservices don't need stable pod names — the broker handles routing.

**Solution:** Convert consumer workload from StatefulSet to Deployment (1 replica).
The ShadowPod strategy already works for non-StatefulSet pods. Shadow pod
(`<source>-shadow`) runs alongside source during migration. At cutover, patch the
Deployment's pod template with `nodeAffinity` for the target node, then delete the
source pod. The Deployment controller creates a replacement on the target node.
Shadow covers traffic during this brief transition.

**Changes:**
- `eval/workloads/consumer.yaml`: StatefulSet -> Deployment
- `handleFinalizing`: Patch Deployment nodeSelector/nodeAffinity before deleting source
- `handlePending`: Already auto-detects non-StatefulSet -> ShadowPod (ReplicaSet ownerRef)
- Consumer app: No changes (hostname-based control queue works with Deployment pod names)

### 2. Direct Node-to-Node Transfer (Transferring: 5.9s -> ~1-2s)

**Problem:** Current transfer path: checkpoint tar -> build OCI image -> push to
in-cluster registry -> target node pulls from registry. The OCI build + registry
round-trip takes 5.9s for a 27MB checkpoint.

**Solution:** Deploy `ms2m-agent` DaemonSet on worker nodes. Transfer Job on source
node POSTs checkpoint tar directly to agent on target node via HTTP. Target agent
builds OCI image locally and loads into CRI-O's image store via `skopeo copy`.
No registry involved.

**New component: `ms2m-agent` DaemonSet**
- Runs on every worker node
- `hostPath` mounts: `/var/lib/kubelet/checkpoints` (read), `/var/lib/ms2m` (read/write)
- Exposes HTTP endpoint (port 9443) for receiving checkpoint tars
- Receives tar -> builds OCI image (same logic as checkpoint-transfer) -> loads into CRI-O
- Future: also handles pre-dump orchestration (Optimization 3)

**Transfer flow:**
1. Controller creates transfer Job on source node
2. Job reads checkpoint tar from `/var/lib/kubelet/checkpoints/`
3. Job POSTs tar to `http://ms2m-agent.<target-node>:9443/checkpoint`
4. Target agent builds OCI image, loads into local CRI-O image store
5. When shadow pod starts, image is already local (no pull needed)

**Changes:**
- New: `cmd/ms2m-agent/` — DaemonSet binary with HTTP server
- New: `config/daemonset/` — DaemonSet manifests, RBAC
- `cmd/checkpoint-transfer/main.go`: Refactor OCI build logic into shared package
- `handleTransferring`: Transfer Job args change (target node endpoint instead of registry)
- CRD: `checkpointImageRepository` becomes optional (not needed for direct transfer)

### 3. Background Pre-dumps (Checkpointing: 478ms -> <50ms, Transfer: 27MB -> <1MB)

**Problem:** Full CRIU checkpoint captures all process memory (27MB for our consumer).
Most of this memory is unchanged between pre-dumps (libraries, static data). Only
the application state delta changes between iterations.

**Solution:** The `ms2m-agent` DaemonSet periodically runs `criu pre-dump` on tracked
pods. Pre-dumps use soft-dirty bit tracking to capture only changed memory pages.
When migration triggers, the final dump is a tiny delta.

**Pre-dump mechanism:**
1. Agent watches pods with annotation `migration.ms2m.io/predump-enabled: "true"`
2. Every 30s (configurable), runs `criu pre-dump --track-mem` on the container's PID
3. Stores pre-dump chain at `/var/lib/ms2m/predumps/<pod-uid>/`
4. Each pre-dump references the previous one (CRIU's `--prev-images-dir`)

**Migration with pre-dumps:**
1. Controller signals agent: "final dump for pod X"
2. Agent runs `criu dump` with `--prev-images-dir` pointing to latest pre-dump
3. Final dump is delta only (pages changed since last pre-dump)
4. Transfer ships the small delta to target agent
5. Target agent reconstructs full checkpoint from pre-dump chain + delta
6. Builds OCI image from reconstructed checkpoint

**Requirements:**
- `hostPID: true` on DaemonSet (to access container PIDs)
- CRIU 4.0 binary on each node
- Access to container's cgroup/namespace paths (via `/proc/<pid>/ns/`)
- CRI-O container metadata (to map pod -> PID)

**Risk:** CRIU pre-dump requires direct PID access, not available through kubelet API.
If this doesn't work from the DaemonSet context, fall back to full checkpoint with
direct transfer only (still achieves ~15s total).

## Architecture

```
                    Source Node                          Target Node
                    +---------+                          +---------+
                    |         |                          |         |
  RabbitMQ <-----  | consumer|  (source, keeps running) |         |
       |            | pod     |                          |         |
       |            |         |                          |         |
       |            +---------+                          +---------+
       |            | ms2m-   |  --- HTTP POST tar --->  | ms2m-   |
       |            | agent   |                          | agent   |
       |            | (DS)    |                          | (DS)    |
       |            +---------+                          +---------+
       |                                                 |         |
       |                                                 | shadow  |
       +-----------------------------------------------> | pod     |
                 (fan-out exchange duplicates msgs)       | (CRIU   |
                                                         | restore)|
                                                         +---------+
```

## Controller Phase Changes

### Pending (no change)
- Look up source pod, detect strategy, record metadata
- For Deployment pods: ownerRef is ReplicaSet -> auto-select ShadowPod

### Checkpointing (minor change)
- If pre-dumps enabled: signal agent for final delta dump instead of full checkpoint
- Otherwise: same kubelet checkpoint API call

### Transferring (major change)
- Transfer Job sends checkpoint tar to target node's ms2m-agent via HTTP
- Agent builds OCI image locally, loads into CRI-O
- Job reports success when agent confirms image loaded

### Restoring (simplified for Deployment)
- ShadowPod path: create `<source>-shadow` pod on target node
- Image already in local CRI-O store (no pull needed, use `imagePullPolicy: Never`)
- No StatefulSet scale-down/up needed
- Wait for shadow pod Running

### Replaying (no change)
- Send START_REPLAY, poll queue depth, wait for drain

### Finalizing (changed for Deployment)
- Send END_REPLAY, delete replay queue
- Patch Deployment pod template: add `nodeAffinity` for target node
- Delete source pod
- Deployment controller creates replacement on target node (using original image, not checkpoint)
- Delete shadow pod once Deployment's replacement is Running
- OR: simpler — just delete source, leave shadow running, accept that Deployment creates
  an extra pod that we immediately delete

## CRD Changes

```yaml
spec:
  # Existing fields unchanged
  sourcePod: "consumer-xyz"
  targetNode: "worker-2"
  migrationStrategy: "ShadowPod"  # auto-detected
  messageQueueConfig: { ... }

  # New optional fields
  transferMode: "Direct"          # "Registry" (default) | "Direct"
  preDumpConfig:
    enabled: true
    intervalSeconds: 30

status:
  # Existing fields unchanged
  phase: "Restoring"
  phaseTimings: { ... }

  # New fields
  deploymentName: "consumer"      # owning Deployment (for Finalizing patch)
  preDumpCount: 5                 # number of pre-dumps taken before migration
```

## Evaluation Matrix

| Configuration | Restoring | Transferring | Checkpointing | Expected Total |
|---|---|---|---|---|
| StatefulSet + Sequential + Registry | 39.1s | 5.9s | 478ms | ~55.7s |
| Deployment + ShadowPod + Registry | ~3-5s | 5.9s | 478ms | ~20s |
| Deployment + ShadowPod + Direct | ~3-5s | ~1-2s | 478ms | ~15s |
| Deployment + ShadowPod + Direct + Pre-dumps | ~3-5s | <0.5s | <50ms | ~5-10s |

Each tested at message rates: 1, 5, 10, 20, 50, 100, 200 msg/s x 10 repetitions.

## Implementation Order

1. **Deployment workload** — lowest risk, biggest impact (39.1s -> 3-5s)
2. **DaemonSet agent + direct transfer** — medium risk, good impact (5.9s -> 1-2s)
3. **Background pre-dumps** — highest risk, requires CRIU direct access

Each optimization is independently valuable and testable.
