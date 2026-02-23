# IONOS Cloud Evaluation Cluster - Complete Setup Documentation

Last updated: 2026-02-23

## Quick Recreate

```bash
# 1. Provision IONOS VMs + install K8s + deploy everything (single command)
bash eval/infra/deploy.sh

# 2. Set up CRIU 4.0 + patched crun on both workers
ssh root@<WORKER_1_IP> 'bash -s' < eval/infra/setup_criu.sh
ssh root@<WORKER_2_IP> 'bash -s' < eval/infra/setup_criu.sh

# 3. Fix registry DNS on all nodes (after registry is deployed)
REGISTRY_IP=$(KUBECONFIG=eval/infra/kubeconfig kubectl -n registry get svc registry -o jsonpath='{.spec.clusterIP}')
for ip in <CONTROL_PLANE_IP> <WORKER_1_IP> <WORKER_2_IP>; do
  ssh root@$ip "echo '$REGISTRY_IP registry.registry.svc.cluster.local' >> /etc/hosts"
done

# 4. Run evaluation
export KUBECONFIG=eval/infra/kubeconfig
TARGET_NODE=worker-2 bash eval/scripts/run_all_evaluations.sh

# 5. Teardown
bash eval/infra/teardown.sh
```

---

## Cluster Architecture

```
┌──────────────────────────────────────────────────────────────┐
│                    IONOS Datacenter (de/txl)                  │
│                                                              │
│  ┌──────────────────┐  ┌──────────────┐  ┌──────────────┐   │
│  │  control-plane   │  │   worker-1   │  │   worker-2   │   │
│  │  2 CPU / 4GB RAM │  │  2 CPU / 4GB │  │  2 CPU / 4GB │   │
│  │  30GB SSD        │  │  30GB SSD    │  │  30GB SSD    │   │
│  │  Ubuntu 22.04    │  │  Ubuntu 22.04│  │  Ubuntu 22.04│   │
│  │                  │  │              │  │              │   │
│  │  K8s API Server  │  │  CRIU 4.0    │  │  CRIU 4.0    │   │
│  │  etcd            │  │  crun 1.19.1 │  │  crun 1.19.1 │   │
│  │  RabbitMQ        │  │  (patched)   │  │  (patched)   │   │
│  │  Registry        │  │  ms2m-agent  │  │  ms2m-agent  │   │
│  │  ms2m-controller │  │  Workloads   │  │  Workloads   │   │
│  └──────────────────┘  └──────────────┘  └──────────────┘   │
│                                                              │
│  Public LAN with DHCP IPs                                    │
└──────────────────────────────────────────────────────────────┘
```

### Software Stack

| Component | Version | Notes |
|-----------|---------|-------|
| Ubuntu | 22.04 LTS | Kernel 5.15.0-161-generic |
| Kubernetes | v1.31.14 | ContainerCheckpoint beta (default on) |
| CRI-O | v1.31.5 | Container runtime with CRIU support |
| CRIU | 4.0 | Built from source on workers only |
| crun | 1.19.1-dirty | Patched with tcp_close support, built from source |
| Calico | v3.29.2 | CNI plugin, pod subnet 192.168.0.0/16 |
| RabbitMQ | 3.12-management | Message broker for evaluation workloads |
| Registry | registry:2 | In-cluster OCI image registry |

---

## Previous Cluster Details (Torn Down)

- **Datacenter**: ms2m-eval-20260216-201022
- **Datacenter ID**: 56497e75-2297-4af7-9eef-e52230cd7164
- **Location**: de/txl (Berlin)
- **Control-plane IP**: 87.106.89.85
- **Worker-1 IP**: 87.106.88.64
- **Worker-2 IP**: 85.215.138.186
- **Registry ClusterIP**: 10.102.183.52
- **SSH**: `ssh -i ~/.ssh/id_rsa -o IdentitiesOnly=yes root@<IP>`
- **Created**: 2026-02-16
- **Uptime at teardown**: ~7 days

---

## Setup Scripts (all in `eval/infra/`)

### `deploy.sh` - One-Command Full Deployment
The main entry point. Runs all phases sequentially:
1. Provision 3 IONOS VMs via `setup_ionos.sh` (or `--skip-provision` to reuse existing)
2. Install CRI-O v1.31 + Kubernetes v1.31 on all nodes
3. Initialize control plane with kubeadm
4. Join workers to cluster
5. Install Calico CNI
6. Deploy RabbitMQ (in `rabbitmq` namespace, tolerates control-plane taint)
7. Deploy container registry (in `registry` namespace, NodePort service)
8. Apply CRD + RBAC manifests
9. Build Docker images locally, push to in-cluster registry via skopeo
10. Deploy ms2m-controller + ms2m-agent DaemonSet
11. Deploy message producer workload

Flags: `--skip-provision`, `--skip-build`

### `setup_ionos.sh` - VM Provisioning
Creates IONOS datacenter with:
- Location: de/txl (Berlin)
- 3 VCPU servers, each: 2 cores, 4GB RAM, 30GB SSD
- Ubuntu 22.04 boot volumes
- Public LAN with DHCP
- Saves state to `eval/infra/state.env`

### `setup_criu.sh` - CRIU + Patched crun (RUN MANUALLY ON WORKERS)
**This is NOT run by `deploy.sh`** - must be run separately on each worker node after cluster is up.

What it does:
1. Installs build dependencies
2. Builds CRIU 4.0 from source → `/usr/local/sbin/criu`
3. Builds crun 1.19.1 from source with `--with-libcriu`
4. **Patches crun**: adds `criu_set_tcp_close(true)` when tcp_established is set
5. Creates `/usr/local/bin/crun-wrapper` that injects `--tcp-established` for checkpoint/restore
6. Configures CRI-O to use crun-wrapper as default runtime
7. Sets `LD_LIBRARY_PATH=/usr/local/lib` via systemd override for CRI-O
8. Restarts CRI-O

### `install_k8s.sh` - Standalone K8s Install (alternative to deploy.sh)
Older standalone script for K8s installation. Superseded by `deploy.sh`.

### `teardown.sh` / `teardown_ionos.sh` - Cluster Destruction
Deletes the entire IONOS datacenter (cascading delete of all VMs, disks, NICs, LANs).
Removes `state.env` and `kubeconfig`.

---

## Post-Deploy Manual Steps

After `deploy.sh` completes, these steps are needed:

### 1. CRIU Setup on Workers
```bash
source eval/infra/state.env
ssh root@$WORKER_1_IP 'bash -s' < eval/infra/setup_criu.sh
ssh root@$WORKER_2_IP 'bash -s' < eval/infra/setup_criu.sh
```

### 2. Fix Registry DNS (CRI-O uses host DNS, not cluster DNS)
```bash
source eval/infra/state.env
REGISTRY_IP=$(KUBECONFIG=eval/infra/kubeconfig kubectl -n registry get svc registry -o jsonpath='{.spec.clusterIP}')
for ip in $CONTROL_PLANE_IP $WORKER_1_IP $WORKER_2_IP; do
  ssh root@$ip "echo '$REGISTRY_IP registry.registry.svc.cluster.local' >> /etc/hosts"
done
```

### 3. Load Images via skopeo on Workers (for localhost/ references)
If ms2m-agent or other images need `localhost/` prefix:
```bash
# On each worker node:
skopeo copy docker-archive:/tmp/image.tar containers-storage:localhost/image:latest
```
The deploy.sh pushes to the in-cluster registry instead, so controller/transfer images use `registry.registry.svc.cluster.local:5000/` prefix.

### 4. Restart Pods After CRI-O Restart
CRI-O restart kills all running containers. After running `setup_criu.sh`:
```bash
kubectl delete pods --all -A  # Let controllers recreate them
```

---

## Deployed Components

### Namespaces
| Namespace | Components |
|-----------|-----------|
| `ms2m-system` | ms2m-controller (Deployment), ms2m-agent (DaemonSet on workers) |
| `rabbitmq` | RabbitMQ broker (Deployment, headless Service) |
| `registry` | Container registry (Deployment, NodePort Service) |
| `default` | Consumer workload, message-producer, consumer Service |
| `kube-system` | Calico, CoreDNS, etcd, kube-apiserver, kube-proxy, kube-scheduler |

### Docker Images
| Image | Dockerfile | Base | Purpose |
|-------|-----------|------|---------|
| ms2m-controller | `Dockerfile` | distroless | Controller manager |
| checkpoint-transfer | `Dockerfile.transfer` | distroless | Builds OCI image from checkpoint, pushes to registry |
| ms2m-agent | `Dockerfile.agent` | ubuntu:22.04 (has skopeo) | DaemonSet for direct node-to-node transfer |

### CRD: StatefulMigration
- **API**: `migration.ms2m.io/v1alpha1`
- **Key spec fields**: `sourcePod`, `targetNode`, `checkpointImageRepository`, `migrationStrategy` (ShadowPod/empty), `transferMode` (Registry/Direct), `messageQueueConfig`, `replayCutoffSeconds`
- **Phases**: Pending → Checkpointing → Transferring → Restoring → Replaying → Finalizing → Completed (or Failed)

---

## Evaluation Configurations

Three migration configurations were evaluated:

### 1. `statefulset-sequential` (Baseline)
- Consumer: **StatefulSet** (`eval/workloads/consumer.yaml`)
- Strategy: Sequential (source pod killed during restore)
- Transfer: Registry-based
- Downtime: ~27-30s (dominated by ~38s restore phase)

### 2. `statefulset-shadowpod`
- Consumer: **StatefulSet** (`eval/workloads/consumer.yaml`)
- Strategy: ShadowPod (shadow pod created on target; source stays running)
- Transfer: Registry-based
- Downtime: 0ms in steady state

### 3. `deployment-registry`
- Consumer: **Deployment** (`eval/workloads/consumer-deployment.yaml`)
- Strategy: ShadowPod (auto-detected from Deployment owner)
- Transfer: Registry-based
- Downtime: 0ms in steady state

### Evaluation Parameters
- **Message rates**: 10, 20, 40, 60, 80, 100, 120 msg/s
- **Repetitions**: 10 per rate per configuration
- **Processing delay**: 10ms per message
- **Replay cutoff**: 120 seconds
- **Probe interval**: 10ms HTTP probe to consumer:8080

---

## Evaluation Data Inventory

### CURRENT DATA (used for dissertation/paper)

These are the curated, final datasets at `eval/results/`:

| File | Content | Rows | Status |
|------|---------|------|--------|
| `sequential-70.csv` | StatefulSet Sequential, 7 rates x 10 reps | 70 | FINAL - used in paper |
| `shadowpod-70.csv` | StatefulSet ShadowPod, 7 rates x 10 reps | 70 | FINAL - used in paper |
| `deployment-registry-70.csv` | Deployment Registry, 7 rates x 10 reps | 70 | FINAL - used in paper |

These 210 runs (70 per config) are the authoritative evaluation data.

### OLD/DEBUG DATA (not used in paper)

Archived at `eval/results/raw-cluster-archive/` (79MB, gitignored):

| Path on cluster | Description | Why not used |
|-----------------|-------------|--------------|
| `results/old-delay50/` | Early runs with 50ms processing delay | Wrong processing delay |
| `results/old-bad-hostname/` | Runs before hostname fix | Bug: CRIU UTS namespace issue |
| `eval/results/downtime-metrics-*.csv` | Early downtime measurement experiments | Iterating on measurement methodology |
| `eval/results/eval-metrics-20260218-090249.csv` | 9-run validation (3 configs x 3 rates) | Subset used to validate methodology, not full eval |
| `eval/results/eval-metrics-20260218-094238.csv` | Full 210-run eval (first attempt) | Superseded by curated 70-run files |
| `eval/eval/results/` | V2 reruns, outlier reruns, various fixes | Debug/rerun iterations |
| `logs/` | Evaluation script logs | Debug only |

### Validation Test Data
Documented in memory file `validation-test-data.md`. Key findings:
- Sequential baseline: ~27-30s downtime, ~38s restore phase
- ShadowPod (both): 0ms downtime in steady state
- Restore: ShadowPod reduces from ~38s to ~2-4s
- Transfer: ~5-6s consistently
- Replay: scales linearly with msg rate, capped at 120s cutoff

---

## Critical Technical Notes

### CRIU UTS Namespace Issue (FIXED)
After CRIU restore, `socket.gethostname()` returns the **checkpointed** hostname (source pod), not the shadow pod hostname. The consumer was fixed to read `/etc/hostname` instead (bind-mounted by CRI-O with correct hostname).

### crun tcp_close Patch
Stock crun doesn't call `criu_set_tcp_close()`. The patch adds this so TCP connections are properly closed during checkpoint (otherwise CRIU errors on established connections). The crun-wrapper also injects `--tcp-established` automatically.

### CRI-O Signature Policy
Remove any `signature_policy` line from CRI-O config — it blocks checkpoint image restore.

### Image Pull Policies
- **Always** for checkpoint images (prevent stale cache from previous migrations)
- **IfNotPresent** for pre-loaded images via skopeo containers-storage
- **Never** for Direct transfer mode (ms2m-agent loads locally)

### Registry DNS
CRI-O uses host DNS, not cluster DNS. Add registry ClusterIP to `/etc/hosts` on all nodes.

---

## Environment Variables for Evaluation

```bash
export KUBECONFIG=eval/infra/kubeconfig
export TARGET_NODE=worker-2
export NAMESPACE=default
export CHECKPOINT_REPO=registry.registry.svc.cluster.local:5000/checkpoints
export MSG_RATES="10 20 40 60 80 100 120"
export REPETITIONS=10
export CONFIGURATION=statefulset-sequential  # or statefulset-shadowpod, deployment-registry
export PROCESSING_DELAY_MS=10
```

---

## Evaluation Scripts (all in `eval/scripts/`)

| Script | Purpose |
|--------|---------|
| `run_all_evaluations.sh` | Master runner: all 3 configs sequentially |
| `run_evaluation.sh` | Baseline evaluation (statefulset-sequential) |
| `run_optimized_evaluation.sh` | Configurable: any config via CONFIGURATION env |
| `run_full_evaluation.sh` | Full eval with checkpoint cleanup between runs |
| `run_downtime_measurement.sh` | Advanced downtime measurement with HTTP probe pod |
| `collect_metrics.sh` | Post-eval metrics extraction from CRs |

---

## Cost / Billing

- **Provider**: IONOS Cloud (Hai Dinh Tuan account)
- **3 VCPU servers**: 2 cores, 4GB RAM, 30GB SSD each
- **Location**: de/txl (Berlin)
- **Important**: Always teardown after evaluation to avoid ongoing charges
- **Teardown command**: `bash eval/infra/teardown.sh`
