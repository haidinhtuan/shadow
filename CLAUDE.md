# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

MS2M (Message-based Stateful Microservice Migration) Kubernetes controller. Orchestrates live migration of stateful pods by checkpointing in-memory state via CRIU/FCC, transferring it as an OCI image, restoring on a target node, and replaying buffered messages for zero-loss handoff.

The controller manages a `StatefulMigration` custom resource through a 5-phase state machine: Pending → Checkpointing → Transferring → Restoring → Replaying → Finalizing → Completed (or Failed).

## Build & Development Commands

```bash
make build          # Build binary to bin/manager (from cmd/main.go)
make run            # Run controller locally (uses ~/.kube/config)
make test           # Format, vet, then run all tests with coverage
make fmt            # go fmt ./...
make vet            # go vet ./...
make docker-build   # Run tests then build Docker image (IMG=controller:latest)
make docker-push    # Push Docker image
go run main.go      # Run the legacy pod-watcher (root main.go, NOT the controller)
```

Single test: `go test ./internal/controller/ -run TestName -v`

## Architecture

### Two Entry Points

- **`cmd/main.go`** — The real controller manager. Uses `controller-runtime`, registers the CRD scheme, sets up the `StatefulMigrationReconciler`, and starts the manager with health/ready probes and optional leader election.
- **`main.go`** (root) — Legacy pod-watcher that lists pods in a loop using raw `client-go`. Not part of the controller. Will likely be removed.

### CRD & API (`api/v1alpha1/`)

- **Group**: `migration.ms2m.io`, **Version**: `v1alpha1`
- **Kind**: `StatefulMigration` — spec includes `sourcePod`, `targetNode`, `checkpointImageRepository`, `replayCutoffSeconds`, `messageQueueConfig`
- **Status**: tracks `phase`, `sourceNode`, `checkpointID`, `targetPod`, and standard `conditions`
- `deepcopy.go` is hand-written (not generated via controller-gen)

### Controller (`internal/controller/`)

`StatefulMigrationReconciler` implements a state machine with phase-specific handlers (`handlePending`, `handleCheckpointing`, etc.). Currently all phase handlers are stubs with TODO placeholders — the scaffolding is complete but real logic (Kubelet checkpoint API calls, OCI image building, message replay) is not yet implemented.

### Deployment Artifacts

- **`config/`** — Kubebuilder-style manifests: CRD definition, manager deployment, RBAC ClusterRole (includes `nodes/proxy` for checkpoint API access)
- **`manifests/`** — Simpler deployment manifests for the legacy pod-watcher (separate RBAC, ServiceAccount)
- **`terraform/`** — GKE cluster provisioning with `UBUNTU_CONTAINERD` nodes (required for CRIU support)

### Current Implementation Status

Phase 1 (Foundation) is complete: project scaffolding, CRD definition, reconciler skeleton with state machine. Phases 2-4 (checkpoint orchestration, state transfer, message replay) are all TODO stubs. See `PLAN.md` for the full roadmap.

## Key Dependencies

- `sigs.k8s.io/controller-runtime` — controller framework (manager, reconciler, scheme registration)
- `k8s.io/client-go` — Kubernetes client (used by both entry points)
- `k8s.io/apimachinery` — API types, runtime, scheme
- Go 1.25+, Kubernetes v1.25+ (for FCC alpha support)

## Infrastructure

GKE cluster requires `UBUNTU_CONTAINERD` image type for CRIU checkpointing support. Terraform provisions with preemptible `e2-medium` nodes. Deploy with:
```bash
cd terraform && terraform init && terraform apply -var="project_id=$PROJECT_ID"
```

## Reference Documents

- `PLAN.md` — Implementation roadmap with all 5 migration phases, testing strategy, and evaluation criteria
- `instruction/instruction` — MS2M framework integration guide (architectural actors, 5-phase procedure, technical prerequisites)
- `Dissertation_Final.pdf` — Full academic dissertation on the MS2M framework
