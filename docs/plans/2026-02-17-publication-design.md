# Publication Design: Optimizing Stateful Microservice Migration with a Kubernetes Operator

Date: 2026-02-17
Status: Approved
Type: Conference paper (6-10 pages)
Positioning: Extension of [505] (K8s integration paper)

## Working Title

*Optimizing Stateful Microservice Migration with a Kubernetes Operator*

## Contributions

1. A **Kubernetes Operator** implementing the MS2M framework as a Custom Resource
   Definition with a declarative state machine reconciler — replacing the external
   Python migration manager from [505].
2. Two **architectural optimizations**:
   - **ShadowPod strategy**: Deployment-based workloads with shadow pods eliminate
     the 39s StatefulSet restore bottleneck.
   - **Direct node-to-node transfer**: An ms2m-agent DaemonSet transfers checkpoints
     directly between nodes, bypassing the container registry.
3. A **bare-metal evaluation** on IONOS Cloud comparing three migration configurations
   across seven message rates with ten repetitions each (210 total runs).

## Target Venue

Systems/cloud conference (IEEE Cloud, CLOSER, ICSA, CCGrid, or similar).
8 pages target length.

## Paper Structure

### Section 1: Introduction (~1 page)

- Problem: Stateful microservice migration in Kubernetes remains challenging.
  Prior work [505] demonstrated MS2M integration but relied on an external Python
  migration manager and suffered from two performance bottlenecks:
  - StatefulSet restore time (~39s due to identity constraints)
  - Registry-based checkpoint transfer (~6s round-trip)
- Contributions (numbered list, as above)
- Results preview: total migration time reduced from ~55s to ~15s

### Section 2: Background & Prior Work (~1 page)

- MS2M framework recap: 5-phase migration (checkpoint, transfer, restore, replay,
  finalize), message-based state reconstruction, downtime limited to checkpoint
  phase — cite [491]
- K8s integration challenges: FCC (Forensic Container Checkpointing), 4-actor model
  (source node, target node, API server, migration manager), StatefulSet identity
  constraints — cite [505]
- Kubernetes Operator pattern: CRD + controller-runtime + reconcile loop.
  An Operator encodes domain-specific operational knowledge into a K8s-native
  control loop.
- Related work: container migration (Tazzioli et al. [497], Ma et al. [499],
  UMS [495]), checkpoint/restore in K8s, live migration techniques (pre-copy,
  post-copy, hybrid)

### Section 3: System Design (~2.5 pages)

#### 3.1 The StatefulMigration Operator

- CRD spec: `sourcePod`, `targetNode`, `migrationStrategy`, `transferMode`,
  `checkpointImageRepository`, `replayCutoffSeconds`, `messageQueueConfig`
- State machine phases: Pending -> Checkpointing -> Transferring -> Restoring ->
  Replaying -> Finalizing -> Completed (or Failed)
- Reconcile loop design:
  - Idempotent phase handlers (each phase can be re-entered safely)
  - Status conditions and phase timing instrumentation
  - Automatic strategy detection (StatefulSet vs Deployment via ownerRef chain)
- **Figure**: Architecture overview showing controller, CRD, worker nodes with
  ms2m-agent, and interaction with K8s API server

#### 3.2 ShadowPod Migration Strategy

- Problem: StatefulSet pods have unique network identity (stable hostname).
  Kubernetes prohibits two pods with the same StatefulSet identity running
  simultaneously. This forces a sequential stop-recreate cycle during migration,
  causing ~39s of restore time.
- Observation: Message-consuming microservices don't need stable pod identity —
  the broker handles routing. Converting from StatefulSet to Deployment (1 replica)
  removes the identity constraint.
- Solution: ShadowPod strategy for Deployment-owned pods:
  1. Create `<source>-shadow` pod on target node from checkpoint image
  2. Both source and shadow run simultaneously during replay
  3. Shadow replays buffered messages while source handles live traffic
  4. After replay: patch Deployment `nodeAffinity` for target, delete source
  5. Deployment controller creates replacement on target node
  6. Delete shadow pod
- **Figure**: ShadowPod lifecycle (source + shadow concurrent operation)

#### 3.3 Direct Node-to-Node Transfer

- Problem: Registry transfer path (checkpoint tar -> build OCI image -> push to
  registry -> target node pulls) takes ~6s for a 27MB checkpoint
- Solution: ms2m-agent DaemonSet on every worker node
  - Transfer Job on source node reads checkpoint from
    `/var/lib/kubelet/checkpoints/`
  - POSTs tar directly to target node's ms2m-agent via HTTP (port 9443)
  - Target agent builds OCI image locally, loads into CRI-O via `skopeo copy`
  - Shadow pod uses `imagePullPolicy: Never` — image already in local store
- **Figure**: Side-by-side comparison of registry vs direct transfer flow
- CRD field: `transferMode: Direct` (vs default `Registry`)

### Section 4: Evaluation (~2.5 pages)

#### 4.1 Experimental Setup

- **Infrastructure**: IONOS Cloud bare-metal cluster
  - 3 nodes: 1 control-plane + 2 workers
  - Hardware specs (from IONOS provisioning)
  - Ubuntu, K8s version, CRI-O, CRIU
- **Workload**: Go-based consumer microservice processing RabbitMQ messages
  - Producer sends at configurable rate
  - Consumer maintains in-memory state (message counter, running statistics)
- **Configurations** (3):
  1. `statefulset-sequential`: StatefulSet + Sequential strategy + Registry transfer (baseline, comparable to [505])
  2. `deployment-registry`: Deployment + ShadowPod + Registry transfer
  3. `deployment-direct`: Deployment + ShadowPod + Direct transfer
- **Parameters**:
  - Message rates: 1, 4, 7, 10, 13, 16, 19 msg/s
  - Repetitions: 10 per rate per configuration
  - Total: 210 runs (70 per configuration)
  - Replay cutoff: 120 seconds

#### 4.2 Results

- **Total migration time** (primary metric):
  - Box plots across 7 message rates for all 3 configurations
  - Expected: statefulset-sequential ~55s, deployment-registry ~20s,
    deployment-direct ~15s
- **Phase-by-phase breakdown** (table or stacked bar):
  - Checkpointing: ~0.5s (same across all configs)
  - Transfer: ~6s (registry) vs ~1-2s (direct)
  - Restore: ~39s (StatefulSet) vs ~3-5s (ShadowPod)
  - Replay: scales with message rate (same across configs)
  - Finalize: ~1-2s
- **Key findings**:
  1. ShadowPod eliminates the StatefulSet restore bottleneck (~87% reduction)
  2. Direct transfer cuts transfer time by ~70%
  3. Combined optimizations: ~73% reduction in total migration time
  4. Replay time scales linearly with message rate (consistent with [505])
  5. Zero message loss maintained across all configurations

#### 4.3 Discussion

- Comparison with [505] results:
  - GCE VMs vs bare-metal: impact on checkpoint/transfer times
  - Python migration manager vs Go operator: overhead comparison
  - Java/Spring Boot consumer vs Go consumer: checkpoint size differences
- Trade-offs:
  - ShadowPod requires Deployment (not applicable to StatefulSet workloads
    that genuinely need stable identity)
  - Direct transfer requires DaemonSet deployment on all worker nodes
  - Operator adds complexity vs simple script (justified by declarative model,
    observability, retry semantics)
- Threats to validity:
  - Single workload type (message consumer)
  - Two-node cluster (minimal network topology)
  - Specific CRIU/CRI-O versions

### Section 5: Related Work (~0.5 page)

- Container migration in Kubernetes (FCC, checkpoint/restore)
- Kubernetes Operators for complex lifecycle management
- Edge/fog migration approaches
- Comparison table: MS2M Operator vs related systems

### Section 6: Conclusion (~0.5 page)

- Summary of contributions: Operator + ShadowPod + Direct transfer
- Key result: ~73% reduction in migration time
- Future work:
  - CRIU pre-dumps for incremental checkpointing (further reducing checkpoint
    and transfer phases)
  - Multi-container pod migration
  - Integration with Kubernetes scheduler for automatic migration triggering
  - Support for additional workload types beyond message consumers

## Figures (estimated 5-6)

1. Architecture overview (controller + CRD + nodes + agent)
2. State machine diagram (7 phases)
3. ShadowPod lifecycle (source + shadow concurrent operation)
4. Registry vs Direct transfer comparison
5. Total migration time box plots (3 configs x 7 rates)
6. Phase breakdown comparison (stacked bar or table)

## Data Dependencies

- Evaluation results from ongoing IONOS cluster run (210 runs across 3 configs)
- Results at `/root/eval/results/*.csv` on control-plane (87.106.89.85)

## Prior Publications Referenced

- [491]: Original MS2M framework (Ch10 of dissertation)
- [505]: MS2M Kubernetes integration (Ch11 of dissertation)
