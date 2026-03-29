# MS2M Controller Optimization Approaches

Date: 2026-02-16
Status: Under exploration
Baseline: test-migrate-4 (~55.7s total, Sequential strategy)

---

## Corrected Downtime Analysis

**Key insight**: In MS2M, the source keeps running through Checkpointing, Transferring, and Restoring.
The fan-out exchange duplicates messages to a replay queue. The target replays those messages to
converge to the source's state. At cutover, both have identical state — near-zero downtime.

Our baseline test used **Sequential strategy** (kill source, recreate on target), turning the
entire Restoring phase (39.1s) into downtime. This is a limitation of our StatefulSet handling,
not the MS2M framework.

**Real optimization targets:**
1. Enable ShadowPod strategy for StatefulSets → near-zero service downtime
2. Reduce total migration time (~55.7s) → better SLAs

---

## Approach A: Overlapping Shadow Migration for StatefulSets (Recommended)

**Core idea:** Enable ShadowPod strategy for StatefulSet pods by decoupling pod identity from
StatefulSet lifecycle during migration.

**Mechanism:**
1. Checkpoint source (continues running, processing primary queue)
2. Fan-out exchange duplicates messages to replay queue
3. Transfer checkpoint image to target node
4. Create shadow pod on target (`consumer-0-shadow`)
5. Shadow pod restores from checkpoint, replays buffered messages
6. Source and target converge to identical state
7. Atomic cutover: Update StatefulSet with nodeAffinity → delete shadow → StatefulSet recreates on target

**Stateful service assessment:** WORKS CORRECTLY ✓
- Checkpoint captures state S after processing messages 1..N
- Fan-out exchange duplicates messages N+1.. to replay queue
- Source continues processing from primary queue (state evolves: S → S')
- Target restores state S, replays messages N+1.. from replay queue (state evolves: S → S')
- Both converge to identical state — cutover is safe
- Key requirement: fan-out must start BEFORE or AT checkpoint time

**Metrics:**
- Service downtime: Near-zero (just atomic switch)
- Total migration time: ~55s (unchanged)
- K8s compatible: Yes, fully native APIs
- Complexity: Medium (controller logic changes)

**Novelty:** First K8s-native zero-downtime StatefulSet migration with message-level consistency.

---

## Approach B: Pipeline-Parallel Migration with Pre-staging

**Core idea:** Overlap migration phases and pre-stage resources on target to minimize total time.

**Optimizations:**
1. Pre-pull checkpoint image on target node before scale operation
2. Pre-create pod resources (ConfigMaps, ServiceAccount tokens)
3. Pipeline phases: start restore as soon as transfer begins streaming
4. Warm pod pool: skeleton pods pre-created on candidate nodes (image pulled, volumes ready)

**Metrics:**
- Service downtime: Near-zero (combined with Approach A)
- Total migration time: Target <15s (from ~55s)
- K8s compatible: Yes
- Complexity: High (pre-staging infrastructure)

**Novelty:** Co-optimized migration pipeline; warm pod pool concept is unpublished.

---

## Approach C: Lazy-Restore + Message Replay (Highest Novelty, Highest Risk)

**Core idea:** Use CRIU `--lazy-pages` for post-copy restore. Transfer only minimal execution state,
fetch memory pages on-demand via `userfaultfd`.

**Mechanism:**
1. Checkpoint source (non-disruptive)
2. Transfer only process metadata + registers to target (<1MB)
3. Restore immediately using `criu lazy-pages` — process runs instantly
4. Memory pages fetched on-demand from source via CRIU page server
5. MS2M message replay runs in parallel with page loading
6. Source keeps running until all pages transferred

**Metrics:**
- Service downtime: Near-zero
- Total migration time: Potentially <5s
- K8s compatible: Partially — CRIU supports it, CRI-O/crun do not expose it
- Complexity: Very high (bypass K8s checkpoint API, orchestrate CRIU directly)

**Novelty:** Very high — no one has demonstrated post-copy + message replay in K8s.

**Risks:**
- Page fault latency degrades application performance during page loading
- CRI-O may not expose lazy-pages
- Untested with `--tcp-close`
- Must bypass Kubernetes checkpoint API

---

## Approach D: Background Incremental Pre-dumps (NEW — Very High Novelty)

**Core idea:** Run CRIU `pre-dump` periodically on stateful pods during normal operation,
so when migration triggers only a tiny delta needs to be transferred.

**Mechanism:**
1. DaemonSet agent runs periodic `criu pre-dump` on tracked pods (e.g., every 30s)
2. Pre-dumps use soft-dirty bit tracking — only captures changed memory pages
3. Store incremental deltas locally on each node
4. When migration triggers: final blocking dump is tiny (only delta since last pre-dump)
5. Transfer chain: pre-dumps already staged + small final delta = fast migration

**Metrics:**
- Checkpoint time: From ~478ms (full) to potentially <50ms (delta only)
- Transfer size: From ~27MB (full) to potentially <1MB (delta)
- Total migration time: Could drop to <10s
- K8s compatible: Yes (DaemonSet + kubelet checkpoint API)
- Complexity: Medium-high (lifecycle management of pre-dump chains)

**Novelty:** Very high — CRIU supports pre-dump natively but NO K8s controller orchestrates
periodic background pre-dumps. This is "the biggest opportunity" identified in the literature.

**Research analog:** Similar to VM live migration's iterative pre-copy, but proactively
maintained during normal operation rather than only during migration.

**Key references:**
- CRIU iterative migration: https://criu.org/Iterative_migration
- CRIU optimizing pre-dump: https://criu.org/Optimizing_pre_dump_algorithm
- U2CMigration (dirty page prediction): https://link.springer.com/article/10.1007/s11390-025-4583-0

---

## Approach E: Service Mesh Traffic Orchestration (NEW — Genuinely Novel)

**Core idea:** Use Istio VirtualService weight shifting to gradually drain traffic from
source and route to target during migration, with mesh retry/circuit-breaker handling transition.

**Mechanism:**
1. During Restoring: Istio VirtualService routes 100% to source
2. When target is ready: Gradually shift weight (90/10 → 50/50 → 0/100)
3. Mesh handles retries during brief transition gaps
4. No message loss due to fan-out exchange + mesh-level retry
5. Final cutover is invisible to clients

**Metrics:**
- Service downtime: True zero (gradual traffic shift, no hard cutover)
- K8s compatible: Yes (Istio is standard K8s extension)
- Complexity: Medium (requires Istio, VirtualService CRD management)

**Novelty:** Very high — Istio traffic management primitives exist, but nobody has
combined them with live stateful container migration. Existing approaches use
lower-level mechanisms (endpoint manipulation, OvS, iptables, custom CNI).

**Key references:**
- Istio traffic shifting: https://istio.io/latest/docs/tasks/traffic-management/traffic-shifting/
- Cast AI uses custom CNI (not mesh): https://cast.ai/blog/introducing-container-live-migration-zero-downtime-for-stateful-kubernetes-workloads/
- MOSE uses OvS: https://arxiv.org/abs/2506.09159

---

## Approach F: Checkpoint-Free Migration via Event Replay (NEW — Paradigm Shift)

**Core idea:** Skip CRIU entirely. Use the message queue as source of truth.
Start fresh consumer on target, replay all messages from a known offset.
Consumer rebuilds state purely from message history (event sourcing pattern).

**Mechanism:**
1. Record a "snapshot offset" — the current message offset in the primary queue
2. Start fresh consumer on target node
3. Replay all messages from offset 0 (or last snapshot) to current
4. Consumer rebuilds identical state through deterministic message processing
5. When caught up to live offset: atomic cutover

**Metrics:**
- CRIU dependency: None (no checkpoint, no kernel dependency, fully portable)
- Service downtime: Near-zero (source runs during replay)
- Total migration time: Proportional to message history length
- K8s compatible: Fully native
- Complexity: Low (no CRIU, no kernel patches, no custom runtimes)

**Novelty:** Medium-high — event sourcing is well-known, but applying it as a
container migration strategy (replacing CRIU) has not been published.

**Limitations:**
- Requires message retention in broker (RabbitMQ message TTL)
- Replay time can be very long for large histories
- Requires deterministic message processing (same input → same state)
- Can be mitigated with periodic application-level snapshots

**Hybrid variant:** CRIU checkpoint as "fast snapshot" + event replay for delta only.
This combines the speed of CRIU with the portability of event sourcing.

---

## Approach G: Pipelined Streaming Migration (NEW — Engineering Innovation)

**Core idea:** Stream checkpoint data directly from source to target as it's being written.
Overlap all phases into a single pipeline instead of sequential steps.

**Mechanism:**
1. CRIU dump streams pages to CRIU page server (disk-less)
2. Page server streams directly to target node over TCP
3. Target begins restore as pages arrive (no intermediate OCI image)
4. Fan-out exchange buffers messages during streaming
5. When streaming complete: target finishes restore, replays messages

**Metrics:**
- Transfer phase: Eliminated (merged into checkpoint + restore)
- Total migration time: Checkpoint + streaming overhead only (~2-5s estimate)
- K8s compatible: Partially (bypasses OCI registry, uses CRIU page server)
- Complexity: High (requires CRIU page server orchestration)

**Novelty:** High — CRIU page server exists but nobody has built a streaming
pipeline that merges checkpoint → transfer → restore into one operation in K8s.

---

## Approach H: Broker-Native Consumer Handoff (REVISED — Simplifies Replay Plumbing)

**Core idea:** Use the message broker's native consumer failover (RabbitMQ SAC / Kafka KIP-848)
to simplify message routing during migration, replacing MS2M's custom fan-out exchange +
replay queue + START_REPLAY/END_REPLAY protocol.

**IMPORTANT: This does NOT eliminate processing time for buffered messages.**
For stateful services, the target must still process all messages that arrived during migration
to rebuild state. What changes is the *mechanism* — broker-native delivery instead of a custom
replay protocol.

### Why the original claim ("eliminates replay phase") was wrong

SAC handles **message routing**, not **state transfer**. For stateful services:
- The target needs CRIU checkpoint state (SAC doesn't provide this)
- Messages during migration must be processed IN ORDER from the checkpoint state
- In-flight messages at checkpoint time create duplication risk

**The in-flight message problem:**
```
callback(ch, method, properties, body):
    msg = json.loads(body)
    time.sleep(delay_ms / 1000.0)    # ← CRIU could freeze HERE
    state["processed"] += 1           # state not yet updated
    ch.basic_ack(delivery_tag=...)    # ack not yet sent
```
If CRIU freezes mid-callback: checkpoint captures partial state, message is unacked,
broker redelivers it → target processes it twice → corrupted state.

### Revised Mechanism (SAC + Cooperative Quiesce)

1. **Quiesce source**: Send "prepare" signal → source calls `basic_cancel`,
   finishes current message, acks it → clean state boundary
2. **Checkpoint** source at clean boundary (all messages through N processed & acked)
3. Messages N+1, N+2, ... pile up in the primary queue (no active consumer)
4. **Transfer** checkpoint, **restore** target on target node
5. Target registers as consumer on the **primary queue** (no replay queue needed)
6. Target picks up message N+1 from where source left off
7. Buffered messages are processed as normal consumption, not "replay mode"

### What actually improves vs. current MS2M

| | Current MS2M Replay | SAC + Quiesce |
|---|---|---|
| Message routing | Custom fan-out exchange + replay queue | Broker-native (primary queue) |
| State boundary | Implicit (checkpoint mid-execution) | Explicit (quiesce before checkpoint) |
| In-flight risk | Handled by replay from known offset | Eliminated by quiescing |
| Extra infrastructure | Fan-out exchange, replay queue, control messages | None (just SAC) |
| Processing time for buffered msgs | ~10.2s | ~10.2s (unchanged) |
| Protocol complexity | High (START/END_REPLAY, queue depth polling) | Low (just cancel/consume) |

**The real win is architectural simplicity, not speed.** The 10.2s of message processing
doesn't disappear — those messages still need to be consumed. But the custom replay
protocol (fan-out exchange, replay queue, control messages, queue depth polling) is
replaced by standard queue consumption.

### Metrics (corrected)
- Replaying phase overhead: ~0s (no protocol overhead, but messages still take time)
- Message processing time: ~same as before (depends on message rate × migration duration)
- K8s compatible: Yes
- Complexity: Lower than current MS2M replay

### Novelty (corrected)
- Medium — simplification of existing approach, not a fundamentally new technique
- The quiesce-before-checkpoint pattern is known but hasn't been combined with SAC for migration
- Less publishable as standalone contribution; better as part of a combined approach

### Key references
- RabbitMQ SAC: https://www.rabbitmq.com/docs/consumers
- Kafka KIP-848: https://cwiki.apache.org/confluence/display/KAFKA/KIP-848

### Trade-offs
- Pro: Simpler architecture (no fan-out, no replay queue, no control messages)
- Pro: No duplicate processing risk (clean state boundary via quiesce)
- Con: Adds quiesce latency before checkpoint (drain current message ~50ms)
- Con: Broker-specific (different impl for RabbitMQ vs Kafka vs NATS)
- Con: Requires application cooperation (must handle "prepare to freeze" signal)
- Con: SAC requires RabbitMQ 3.8+ with quorum queues for best behavior

---

## Updated Comparison

| Approach | Downtime | Total Time | K8s Native | Novelty | Complexity | Publication |
|----------|----------|------------|------------|---------|------------|-------------|
| A: Shadow StatefulSet | Near-zero | ~55s | Yes | Medium | Medium | Medium |
| B: Pre-staging | Near-zero | <15s | Yes | Medium | High | Medium |
| C: Lazy-restore | Near-zero | <5s | Partial | Very High | Very High | High |
| **D: Background pre-dumps** | Near-zero | **<10s** | **Yes** | **Very High** | **Med-High** | **Very High** |
| **E: Service mesh traffic** | **True zero** | ~55s | **Yes** | **Very High** | Medium | **Very High** |
| F: Event replay (no CRIU) | Near-zero | Variable | Yes | Med-High | Low | Medium |
| G: Streaming pipeline | Near-zero | <5s | Partial | High | High | High |
| H: Broker-native handoff | Near-zero | ~55s | Yes | Medium | Low | Medium |

Note on H: Originally claimed to "eliminate replay phase" — this was incorrect for stateful
services. H simplifies the replay plumbing (no fan-out exchange, no replay queue, no control
messages) but buffered messages still need processing time. The real win is architectural
simplicity, not speed. See revised Approach H section above.

---

## Recommendation (Revised — Implementation Decision)

### Implemented: Three-Phase Optimization

After analysis, we dropped StatefulSet-focused approaches (A) because message-consuming
microservices don't need StatefulSet identity guarantees (databases/brokers that DO use
StatefulSets have their own replication mechanisms). Instead, we implemented three
practical engineering optimizations:

1. **Deployment-based workload** (Restoring: 39.1s → ~3-5s)
   - Consumer runs as Deployment instead of StatefulSet
   - ShadowPod strategy works without identity conflicts
   - No scale-down/up cycle during migration

2. **Direct node-to-node transfer** (Transferring: 5.9s → ~1-2s)
   - ms2m-agent DaemonSet on each worker node
   - Checkpoint tar transferred via HTTP POST, bypassing OCI registry
   - Agent builds OCI image locally and loads into CRI-O

3. **Background pre-dumps** (Checkpointing: 478ms → <50ms, optional/high-risk)
   - CRIU pre-dump with soft-dirty bit tracking
   - Requires direct CRIU access from DaemonSet (not via kubelet API)
   - Falls back to full checkpoint if not feasible

### Evaluation Matrix

| Configuration | Restoring | Transferring | Checkpointing | Expected Total |
|---|---|---|---|---|
| StatefulSet + Sequential + Registry (baseline) | 39.1s | 5.9s | 478ms | ~55.7s |
| Deployment + ShadowPod + Registry | ~3-5s | 5.9s | 478ms | ~20s |
| Deployment + ShadowPod + Direct | ~3-5s | ~1-2s | 478ms | ~15s |
| Deployment + ShadowPod + Direct + Pre-dumps | ~3-5s | <0.5s | <50ms | ~5-10s |

### Discarded Approaches
- **Continuous Migration Readiness**: Always-on shadow replicas. Wasteful — 2x resources
  for a rare event. Nobody does this for migration (DB replicas serve reads + failover).
- **Service mesh traffic (E)**: Istio VirtualService doesn't control queue consumption
  patterns. Not applicable to message-based stateful services.
- **Event replay without CRIU (F)**: RabbitMQ deletes acked messages. Requires message
  retention and periodic snapshots to be practical.
- **StatefulSet ShadowPod (A)**: Solved a problem that rarely exists — message consumers
  don't need StatefulSet identity guarantees.

### Future Research Tracks
- **D** (Background pre-dumps): Biggest remaining optimization, requires cluster validation
- **G** (Streaming pipeline): Could further reduce transfer by piping CRIU directly to target
- **H** (Broker-native handoff): Simplifies replay plumbing, useful as incremental improvement
