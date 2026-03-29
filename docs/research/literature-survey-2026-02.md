# Literature Survey: Container Live Migration in Kubernetes (2022-2026)

Date: 2026-02-16
Context: Optimization research for MS2M Kubernetes controller

---

## 1. Container Live Migration — State of the Art

### Democratizing Container Live Migration (ACM Computing Surveys, Dec 2024)
- **Authors:** Soussi, W.; Gur, G.; Stiller, B.
- **Technique:** Taxonomy of pre-copy, post-copy, and hybrid strategies. Covers traffic redirection and optimization across cloud, fog, MEC.
- **Findings:** Pre-copy = lower downtime but higher data transfer. Post-copy = lower total time. Hybrid combines both.
- **K8s:** Discusses K8s integration as open challenge.
- **Link:** https://dl.acm.org/doi/10.1145/3704436

### State of Container Checkpointing with CRIU (ICSA 2024)
- **Technique:** Multi-case experience report on CRIU for live migration and rapid init.
- **Findings:** CRIU works but needs deeper K8s integration and performance improvements.
- **K8s:** Directly evaluates K8s integration.
- **Link:** https://ieeexplore.ieee.org/iel8/10628106/10628119/10628207.pdf

### K8s Scheduling with Checkpoint/Restore (JSSPP 2025)
- **Authors:** Spisakova, V.; Stoyanov, R.; Hejtmanek, L.; Klusacek, D.; Reber, A.; Bruno, R.
- **Technique:** Interruption-Aware Scheduling integrating transparent C/R with K8s scheduler.
- **K8s:** Directly targets scheduler integration.
- **Link:** https://www.dpss.inesc-id.pt/~rbruno/papers/vspisakova-jsspp25.pdf

### FlexiMigrate (Cluster Computing, Sep 2025)
- **Authors:** Ahmadpanah, S.H.; Mirabi, M.; Sahafi, A.; Erfani, S.H.
- **Technique:** CRIU + SDN + ML-based migration decision engine.
- **Performance:** 46.2% total time reduction, **78.8% downtime reduction**, 50% network overhead reduction.
- **K8s:** Container-level CRIU, heterogeneous environments.
- **Link:** https://link.springer.com/article/10.1007/s10586-025-05548-x

### MDB-KCP: In-Memory DB Persistence (Journal of Cloud Computing, 2024)
- **Authors:** Lee, J.; Kang, H.; Yu, H. et al.
- **Technique:** CRIU checkpoints for in-memory DB persistence in K8s. Avoids CoW overhead.
- **Performance:** 7.1x less downtime vs. main process-based snapshots.
- **K8s:** Built directly on K8s.
- **Link:** https://link.springer.com/article/10.1186/s13677-024-00687-9

### Comprehensive Performance Evaluation (Computing, 2025)
- **Authors:** Feitosa, L. et al.
- **Technique:** Stochastic Petri net models for Cold, PreCopy, PostCopy, Hybrid strategies.
- **Findings:** Cold migration has lower MTT at high arrival rates. PreCopy/Hybrid offer lower downtime.
- **Link:** https://link.springer.com/article/10.1007/s00607-025-01423-0

---

## 2. Shadow Pods / Pre-Staging / Warm Pod Pools

**GAP: No paper explicitly proposes pre-staging target pods for instant state injection.**

### Good Shepherds (ICFEC 2022)
- **Authors:** Souza Junior, P.; Miorandi, D.; Pierre, G.
- **Technique:** DMTCP (not CRIU) to checkpoint entire pods. Target environment prepared before state transfer. DMTCP avoids kernel-version dependency.
- **Performance:** Similar total time to CRIU, **7x shorter downtime**.
- **K8s:** Fully transparent, standard K8s API.
- **Link:** https://inria.hal.science/hal-03587358

### Live Migration of Multi-Container Pods (SEATED/HPDC 2024)
- **Authors:** Puliafito, C.
- **Technique:** Liqo for multi-cluster K8s. Target environment prepared via virtual-node abstraction.
- **K8s:** Fully compliant with standard K8s API.
- **Link:** https://dl.acm.org/doi/10.1145/3660319.3660330

### Cast AI Container Live Migration (Industry, Nov 2024)
- **Technique:** Commercial CRIU-based live migration. Claims zero downtime for stateful workloads.
- **K8s:** Production EKS, GCP/Azure planned.
- **Link:** https://cast.ai/blog/introducing-container-live-migration-zero-downtime-for-stateful-kubernetes-workloads/

### K8s Checkpoint/Restore Working Group (Jan 2026)
- New WG crafting KEP for pod-level checkpointing. Current support is container-level only. Restore into new pod NOT supported in K8s API — only at container engine level.
- **Link:** https://www.kubernetes.dev/blog/2026/01/21/introducing-checkpoint-restore-wg/

---

## 3. Lazy Restore / Post-Copy Container Migration

### Practicable Live Container Migrations in HPC (J. Systems Architecture, Jul 2024)
- **Authors:** Guitart, J.
- **Technique:** Diskless, iterative (pre-copy AND post-copy) using CRIU's `--lazy-pages` + `userfaultfd`. Handles TCP connections.
- **Performance:** "Low application downtime and memory/disk usage."
- **K8s:** HPC containers, not K8s-integrated but uses standard CRIU.
- **Link:** https://www.sciencedirect.com/science/article/pii/S1383762124000948

### CRIU Lazy Migration (Native Feature)
- Post-copy via `userfaultfd` (Linux 4.11+). Process restored immediately, pages fetched on demand.
- Performance: ~0.25s for small memory, ~0.5s for 1GB.
- **Links:** https://criu.org/Lazy_migration, https://criu.org/Userfaultfd

### U2CMigration (JCST, Nov 2025)
- **Authors:** Peng, Y.; Xu, F.; Wei, Z.Q. et al.
- **Technique:** Two-phase predictive memory analysis for dirty pages. Optimizes pre-copy iterations.
- **Performance:** 26.1-47.9% duration reduction, **21.3-32.6% downtime reduction**.
- **Link:** https://link.springer.com/article/10.1007/s11390-025-4583-0

**GAP: No paper demonstrates orchestrated post-copy within Kubernetes.**

---

## 4. Message-Aware / Stateful Microservice Migration

### MS2M + FCC (arXiv, Sep 2025) — Our Work
- **Technique:** MS2M + K8s FCC. Secondary queue buffers messages during migration. Cutoff mechanism.
- **Performance:** **96.986% downtime reduction** vs cold migration.
- **K8s:** Built on K8s FCC (beta in 1.30+).
- **Link:** https://arxiv.org/abs/2509.05794

### MOSE (IEEE TNSM, Jun 2025)
- **Authors:** Calagna, A.; Yu, Y.; Giaccone, P.; Chiasserini, C.F.
- **Technique:** Orchestration framework for stateful edge migration. UAV autopilot validation.
- **Performance:** **77% downtime reduction** vs SotA.
- **Link:** https://arxiv.org/abs/2506.09159

**MS2M is the ONLY published approach with explicit message queue buffering/replay in K8s.**

---

## 5. Comparison Table

| Approach | Year | Downtime | Technique | K8s Native? |
|----------|------|----------|-----------|-------------|
| MS2M + FCC | 2025 | ~96.99% reduction | CRIU checkpoint + message replay | Yes (FCC beta) |
| FlexiMigrate | 2025 | ~78.8% reduction | CRIU + SDN + ML | Container-level |
| MOSE | 2025 | ~77% reduction | Edge orchestration | Edge containers |
| Good Shepherds | 2022 | 7x shorter | DMTCP full-pod | Yes (standard API) |
| Cast AI CLM | 2024 | Claims zero | Commercial CRIU | Yes (EKS) |
| U2CMigration | 2025 | 21-33% reduction | Predictive dirty pages | CRIU prototype |

---

## 6. CRIU Feature Availability

| Feature | CRIU | runc | crun | CRI-O | Kubernetes |
|---------|------|------|------|-------|------------|
| Lazy pages (post-copy) | Stable (since ~3.5) | Supported | Not native CLI | Not supported | Not supported |
| Pre-dump (iterative) | Stable | Supported | Not native CLI | Not supported | Not supported |
| Page server (disk-less) | Stable | Via CRIU config | Via CRIU config | Not supported | Not supported |
| Basic checkpoint/restore | Stable | Full support | Supported (libcriu) | Single-shot | Beta in 1.30 |

---

## 7. Additional Findings (Round 2 Research)

### Hybrid Pre/Post-Copy Migration
- **Dynamic Hybrid-copy** (ScienceDirect, 2020): Iterative convergence factor to auto-switch pre→post-copy
- **Good Shepherds / MyceDrive** (ICFEC 2022): Evaluated hybrid in K8s pods, but no adaptive switching
- **GAP:** No K8s operator implements adaptive hybrid switching

### Service Mesh Traffic Orchestration
- Istio VirtualService supports TCP/HTTP weight shifting between destinations
- Cast AI uses custom CNI (not mesh); MOSE uses OvS (not mesh)
- **GAP:** Nobody has combined Istio traffic management with live stateful migration

### Predictive / Proactive Migration
- **Low-Latency Proactive Migration** (IEEE ICMC 2024): RL-based, edge/meta computing
- **U2CMigration** (JCST 2025): Attention-based dirty page prediction, 26-48% duration reduction
- **Predictive Checkpoint with LSTM** (Sustainability/MDPI 2022): LSTM for dirty page prediction
- **GAP:** No K8s controller does proactive pre-staging with prediction

### Background Incremental Pre-dumps
- CRIU `pre-dump` + `--track-mem` + soft-dirty bit is stable and well-tested
- Research shows 3.27x faster checkpoint, 69.2% smaller data with dirty-page tracking
- **GAP:** No K8s controller orchestrates periodic background pre-dumps — BIGGEST OPPORTUNITY

### RDMA Live Migration
- **MigrRDMA** (ACM SIGCOMM 2025): Software indirection layer for RDMA migration, 0.7-12.1ms downtime
- **MigrOS** (USENIX ATC 2021): OS-level transparent RDMA migration
- Not directly applicable but indirection concept is interesting

### Event Sourcing / CDC Approaches
- MS2M is already the primary published work combining message replay with checkpoint
- Kafka Streams state stores use changelog topics for state reconstruction
- **GAP:** CDC + CRIU + message replay hybrid has never been attempted

---

## 8. Critical Findings (Round 3 Research — Adjacent Domains)

### Broker-Native Consumer Handoff (GAME CHANGER)
- **Kafka KIP-848** (GA in Kafka 4.0, Mar 2025): Server-side incremental rebalance. Adding consumer
  to group rebalances only affected partitions. 5s vs 103s for 900-partition rebalance.
  Link: https://cwiki.apache.org/confluence/display/KAFKA/KIP-848
- **RabbitMQ Single Active Consumer (SAC)**: Only one consumer active per queue, automatic failover
  to backup consumer. Combined with federation for cross-node handoff.
  Link: https://www.rabbitmq.com/docs/consumers
- **Impact on MS2M**: Could ELIMINATE the Replaying phase entirely for RabbitMQ workloads by
  registering the target as backup SAC consumer before checkpointing.

### CRIU Disk-less Migration (Page Server)
- Direct memory streaming source → target over TCP, no disk I/O, no registry
- Page server runs on destination, receives pages via network into tmpfs
- With `--auto-dedup` for incremental migration deduplication
- Link: https://criu.org/Disk-less_migration

### Lazy-Pulling Checkpoint Images (SOCI/Nydus)
- SOCI (AWS): External zTOC index enables seeking into compressed layers
- Nydus (CNCF): Content-addressable chunks with optional P2P distribution
- Restore starts BEFORE image fully downloaded
- Novel when applied to checkpoint images (nobody has done this)

### Speculative Page Prefetching
- HSG-LM: Predict which pages process will access first, transfer those first
- During post-copy, prefetch nearby pages alongside faulted page
- Reduces total page faults during lazy restore

### K8s C/R Working Group KEP (Strategic)
- WG (Jan 2026) crafting KEP for scheduler-integrated C/R
- MS2M should align as message-aware extension of native K8s C/R
- Link: https://www.kubernetes.dev/blog/2026/01/21/introducing-checkpoint-restore-wg/

### In-Place Pod Resource Update (K8s 1.35 GA)
- KEP-1287: CPU/memory can be changed without pod restart
- Precedent for mutable pod specs; future KEP could extend to node reassignment

---

## 9. Key Research Gaps (Final)

1. **No warm pod pool / pre-staging** in any published work
2. **No post-copy (lazy restore) in Kubernetes** — only HPC
3. **No combined message-aware + optimized C/R** — MS2M is unique
4. **No K8s controller orchestrates background pre-dumps** — biggest opportunity
5. **No service mesh integration for migration traffic** — Istio primitives unused
6. **No broker-native handoff for container migration** — KIP-848/SAC unused for this
7. **No zero-downtime StatefulSet migration** — pod identity constraints unsolved
8. **No lazy-pulling applied to checkpoint images** — SOCI/Nydus untapped
4. **No zero-downtime StatefulSet migration** — identity constraints unsolved
5. **K8s restore API gap** — restore into new pod not in K8s API (only container engine level)
