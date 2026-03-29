# MS2M Controller: Finalize, Deploy, Test, Evaluate

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix all broken code, deploy on IONOS Cloud, run end-to-end tests, execute the full evaluation, and collect migration timing data.

**Architecture:** Fix 9 code-level bugs (Dockerfile, RBAC, container name, volume mounts, OwnerRefs, CRIU restore, replay timeout, exchange name), fix infrastructure scripts (kubeadm feature gates, CRI-O CRIU config, cross-namespace URLs), then deploy to a 3-node IONOS cluster and run 70 evaluation runs (7 message rates x 10 reps).

**Tech Stack:** Go 1.24, controller-runtime, CRI-O + CRIU, Kubernetes 1.28, IONOS Cloud (ionosctl), RabbitMQ 3.12, Docker/buildah.

---

## Task 1: Fix Dockerfile and Delete Old main.go

The root `main.go` is an old placeholder pod-watcher. The real entrypoint is `cmd/main.go`. The Dockerfile still references the old file and only copies `main.go` instead of the full source tree.

**Files:**
- Delete: `main.go` (root, the old pod-watcher placeholder)
- Modify: `Dockerfile`

**Step 1: Delete the old main.go**

```bash
rm main.go
```

**Step 2: Rewrite the Dockerfile**

Replace the entire `Dockerfile` with:

```dockerfile
FROM golang:1.24 AS builder
WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager ./cmd/main.go

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
```

**Step 3: Verify the Docker build succeeds**

```bash
docker build -t ms2m-controller:latest .
```

Expected: Build succeeds, image created.

**Step 4: Verify the Go build still works**

```bash
go build -o bin/manager ./cmd/main.go
```

Expected: Binary created at `bin/manager`.

**Step 5: Run all tests**

```bash
go test ./...
```

Expected: All tests pass.

**Step 6: Commit**

```bash
git add -A
git commit -m "fix: update Dockerfile to build from cmd/main.go and remove old placeholder"
```

---

## Task 2: Fix RBAC Permissions

The `config/rbac/role.yaml` is missing permissions for `batch/jobs` (transfer Job), `apps/statefulsets` (Sequential strategy), and `core/nodes` (node lookup). The controller will fail at runtime without these.

**Files:**
- Modify: `config/rbac/role.yaml`

**Step 1: Add missing RBAC rules**

Add these rules to the end of the `rules` array in `config/rbac/role.yaml`:

```yaml
- apiGroups:
  - batch
  resources:
  - jobs
  verbs:
  - create
  - delete
  - get
  - list
  - watch
- apiGroups:
  - ""
  resources:
  - nodes
  verbs:
  - get
  - list
- apiGroups:
  - apps
  resources:
  - statefulsets
  - statefulsets/scale
  verbs:
  - get
  - list
  - patch
  - update
  - watch
```

**Step 2: Verify YAML is valid**

```bash
python3 -c "import yaml; yaml.safe_load(open('config/rbac/role.yaml'))"
```

Expected: No errors.

**Step 3: Commit**

```bash
git add config/rbac/role.yaml
git commit -m "fix: add missing RBAC permissions for jobs, nodes, and statefulsets"
```

---

## Task 3: Add ContainerName Field and Auto-Detection

The controller hardcodes `m.Spec.SourcePod` as the container name for the checkpoint API call (line 178 of controller). This is wrong — pod names and container names are different things. We need to either let the user specify it or auto-detect from the source pod.

**Files:**
- Modify: `api/v1alpha1/types.go` — add `ContainerName` to spec and status
- Modify: `api/v1alpha1/deepcopy.go` — no change needed (string fields are value-copied)
- Modify: `config/crd/bases/migration.ms2m.io_statefulmigrations.yaml` — add field
- Modify: `internal/controller/statefulmigration_controller.go` — auto-detect and use

**Step 1: Add ContainerName to types.go**

In `StatefulMigrationSpec`, after the `SourcePod` field, add:

```go
// ContainerName is the name of the container to checkpoint.
// If empty, defaults to the first container in the source pod.
ContainerName string `json:"containerName,omitempty"`
```

In `StatefulMigrationStatus`, after `TargetPod`, add:

```go
// ContainerName is the resolved container name (from spec or auto-detected)
ContainerName string `json:"containerName,omitempty"`
```

**Step 2: Update CRD YAML**

In `config/crd/bases/migration.ms2m.io_statefulmigrations.yaml`, add under `spec.properties` (after `checkpointImageRepository`):

```yaml
              containerName:
                description: ContainerName is the name of the container to checkpoint.
                  If empty, defaults to the first container in the source pod.
                type: string
```

And under `status.properties` (after `checkpointID`):

```yaml
              containerName:
                description: ContainerName is the resolved container name
                type: string
```

**Step 3: Update handlePending to auto-detect container name**

In `handlePending` in `statefulmigration_controller.go`, after the `m.Status.SourceNode = sourcePod.Spec.NodeName` line, add container name resolution:

```go
// Resolve container name
containerName := m.Spec.ContainerName
if containerName == "" && len(sourcePod.Spec.Containers) > 0 {
    containerName = sourcePod.Spec.Containers[0].Name
}
if containerName == "" {
    return r.failMigration(ctx, m, "could not determine container name for source pod")
}
m.Status.ContainerName = containerName
```

**Step 4: Update handleCheckpointing to use resolved container name**

Replace line 178 (`m.Spec.SourcePod, // container name typically matches pod basename`) with:

```go
m.Status.ContainerName,
```

So the full Checkpoint call becomes:

```go
resp, err := r.KubeletClient.Checkpoint(
    ctx,
    m.Status.SourceNode,
    m.Namespace,
    m.Spec.SourcePod,
    m.Status.ContainerName,
)
```

**Step 5: Run tests**

```bash
go test ./...
```

Expected: All tests pass (existing tests don't set ContainerName, so it defaults to empty which is fine for the mock path).

**Step 6: Commit**

```bash
git add api/v1alpha1/types.go config/crd/bases/migration.ms2m.io_statefulmigrations.yaml internal/controller/statefulmigration_controller.go
git commit -m "feat: add ContainerName field with auto-detection from source pod"
```

---

## Task 4: Fix Transfer Job (Volume Mount, OwnerReference, Container Name)

The transfer Job has three problems:
1. No `hostPath` volume mount — the container can't access `/var/lib/kubelet/checkpoints/` on the node
2. No `OwnerReference` — deleting the migration CR leaves orphaned Jobs
3. Doesn't pass container name to the transfer binary (needed for CRIU annotation in Task 5)

**Files:**
- Modify: `internal/controller/statefulmigration_controller.go` — `handleTransferring` function

**Step 1: Update the Job spec in handleTransferring**

Replace the Job creation block (the `job := &batchv1.Job{...}` section, approximately lines 217-243) with:

```go
imageRef := fmt.Sprintf("%s/%s:checkpoint", m.Spec.CheckpointImageRepository, m.Spec.SourcePod)
trueVal := true
job := &batchv1.Job{
    ObjectMeta: metav1.ObjectMeta{
        Name:      jobName,
        Namespace: m.Namespace,
        Labels: map[string]string{
            "migration.ms2m.io/migration": m.Name,
            "migration.ms2m.io/phase":     "transferring",
        },
        OwnerReferences: []metav1.OwnerReference{
            {
                APIVersion: migrationv1alpha1.GroupVersion.String(),
                Kind:       "StatefulMigration",
                Name:       m.Name,
                UID:        m.UID,
                Controller: &trueVal,
            },
        },
    },
    Spec: batchv1.JobSpec{
        Template: corev1.PodTemplateSpec{
            Spec: corev1.PodSpec{
                RestartPolicy: corev1.RestartPolicyNever,
                NodeSelector: map[string]string{
                    "kubernetes.io/hostname": m.Status.SourceNode,
                },
                Containers: []corev1.Container{
                    {
                        Name:  "checkpoint-transfer",
                        Image: "checkpoint-transfer:latest",
                        Args:  []string{m.Status.CheckpointID, imageRef, m.Status.ContainerName},
                        VolumeMounts: []corev1.VolumeMount{
                            {
                                Name:      "checkpoints",
                                MountPath: "/var/lib/kubelet/checkpoints",
                                ReadOnly:  true,
                            },
                        },
                    },
                },
                Volumes: []corev1.Volume{
                    {
                        Name: "checkpoints",
                        VolumeSource: corev1.VolumeSource{
                            HostPath: &corev1.HostPathVolumeSource{
                                Path: "/var/lib/kubelet/checkpoints",
                            },
                        },
                    },
                },
            },
        },
    },
}
```

**Step 2: Run tests**

```bash
go test ./internal/controller/ -run TestReconcile_Transferring -v
```

Expected: All Transferring tests pass. The existing tests don't check volume mounts or OwnerReferences, so they should still pass.

**Step 3: Commit**

```bash
git add internal/controller/statefulmigration_controller.go
git commit -m "fix: add hostPath volume mount and OwnerReference to transfer Job"
```

---

## Task 5: Fix checkpoint-transfer Binary (CRIU Annotation, 3rd Arg)

The transfer binary needs to:
1. Accept an optional 3rd argument (container name)
2. Add the `io.kubernetes.cri-o.annotations.checkpoint.name` annotation to the OCI image so CRI-O recognizes it as a checkpoint image

**Files:**
- Modify: `cmd/checkpoint-transfer/main.go`

**Step 1: Update main.go to accept container name and add annotation**

Replace the entire `cmd/checkpoint-transfer/main.go` with:

```go
package main

import (
	"fmt"
	"os"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// buildCheckpointImage creates a single-layer OCI image from a CRIU checkpoint tarball.
// It adds the CRI-O checkpoint annotation so the runtime recognizes it as restorable.
func buildCheckpointImage(checkpointPath, containerName string) (v1.Image, error) {
	layer, err := tarball.LayerFromFile(checkpointPath)
	if err != nil {
		return nil, fmt.Errorf("creating layer from checkpoint: %w", err)
	}

	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return nil, fmt.Errorf("appending layer to image: %w", err)
	}

	// Add CRI-O checkpoint annotation so the runtime restores instead of starting fresh
	if containerName != "" {
		img = mutate.Annotations(img, map[string]string{
			"io.kubernetes.cri-o.annotations.checkpoint.name": containerName,
		}).(v1.Image)
	}

	return img, nil
}

func main() {
	if len(os.Args) < 3 || len(os.Args) > 4 {
		fmt.Fprintf(os.Stderr, "usage: %s <checkpoint-tar-path> <image-ref> [container-name]\n", os.Args[0])
		os.Exit(1)
	}

	checkpointPath := os.Args[1]
	imageRef := os.Args[2]
	containerName := ""
	if len(os.Args) == 4 {
		containerName = os.Args[3]
	}

	fmt.Printf("Building checkpoint image from %s\n", checkpointPath)
	img, err := buildCheckpointImage(checkpointPath, containerName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building image: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Pushing image to %s\n", imageRef)
	if err := crane.Push(img, imageRef, crane.WithAuthFromKeychain(authn.DefaultKeychain)); err != nil {
		fmt.Fprintf(os.Stderr, "error pushing image: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Checkpoint image pushed successfully")
}
```

**Step 2: Update test to match new signature**

In `cmd/checkpoint-transfer/main_test.go`, update `TestBuildCheckpointImage_InvalidPath`:

```go
func TestBuildCheckpointImage_InvalidPath(t *testing.T) {
	_, err := buildCheckpointImage("/nonexistent/path.tar", "test-container")
	if err == nil {
		t.Error("expected error for nonexistent path")
	}
}
```

And update `TestBuildCheckpointImage_ValidTar`:

```go
func TestBuildCheckpointImage_ValidTar(t *testing.T) {
	// ... existing setup code to create tmpFile ...
	img, err := buildCheckpointImage(tmpFile.Name(), "my-container")
	// ... rest of test ...
}
```

**Step 3: Run tests**

```bash
go test ./cmd/checkpoint-transfer/ -v
```

Expected: Both tests pass.

**Step 4: Commit**

```bash
git add cmd/checkpoint-transfer/
git commit -m "feat: add CRIU checkpoint annotation to transfer OCI image"
```

---

## Task 6: Fix Restore Phase (OwnerRef, Container Name, Labels)

The restore phase has three issues:
1. No OwnerReference on the created shadow pod
2. Doesn't copy labels from source pod (needed for Service selector matching)
3. For Sequential strategy, can't look up source pod (already deleted) — needs container name from status

**Files:**
- Modify: `internal/controller/statefulmigration_controller.go` — `handleRestoring` function

**Step 1: Rewrite the pod creation block in handleRestoring**

Replace the pod creation section (from `checkpointImage := ...` through `if err := r.Create(ctx, newPod)`) with:

```go
checkpointImage := fmt.Sprintf("%s/%s:checkpoint", m.Spec.CheckpointImageRepository, m.Spec.SourcePod)

// Determine container name: use status (resolved in Pending), fall back to spec
containerName := m.Status.ContainerName
if containerName == "" {
    containerName = "app"
}

// Copy labels from source pod if available (for Service selector matching)
podLabels := map[string]string{
    "migration.ms2m.io/migration": m.Name,
    "migration.ms2m.io/role":      "target",
}
if sourcePod.Name != "" {
    for k, v := range sourcePod.Labels {
        podLabels[k] = v
    }
    // Override migration labels (in case source had conflicting ones)
    podLabels["migration.ms2m.io/migration"] = m.Name
    podLabels["migration.ms2m.io/role"] = "target"
}

trueVal := true
newPod := &corev1.Pod{
    ObjectMeta: metav1.ObjectMeta{
        Name:      targetPodName,
        Namespace: m.Namespace,
        Labels:    podLabels,
        Annotations: map[string]string{
            "migration.ms2m.io/checkpoint-image": checkpointImage,
        },
        OwnerReferences: []metav1.OwnerReference{
            {
                APIVersion: migrationv1alpha1.GroupVersion.String(),
                Kind:       "StatefulMigration",
                Name:       m.Name,
                UID:        m.UID,
                Controller: &trueVal,
            },
        },
    },
    Spec: corev1.PodSpec{
        NodeName: m.Spec.TargetNode,
        Containers: []corev1.Container{
            {
                Name:  containerName,
                Image: checkpointImage,
            },
        },
    },
}
```

**Step 2: Run tests**

```bash
go test ./internal/controller/ -run TestReconcile_Restoring -v
```

Expected: All Restoring tests pass.

**Step 3: Commit**

```bash
git add internal/controller/statefulmigration_controller.go
git commit -m "fix: add OwnerReference and source labels to restored pod"
```

---

## Task 7: Implement ReplayCutoffSeconds and Fix Exchange Name

Two small fixes in different phases:

1. **ReplayCutoffSeconds**: The spec field exists but the replaying handler only checks `depth == 0`. If the queue never fully drains, the migration hangs forever. Add a timeout based on `ReplayCutoffSeconds`.

2. **Exchange name derivation**: `DeleteSecondaryQueue` derives exchange name as `baseName + ".fanout"` instead of using the actual exchange name from the CR spec. Fix by passing it through.

**Files:**
- Modify: `internal/messaging/client.go` — update `BrokerClient` interface
- Modify: `internal/messaging/rabbitmq.go` — update `DeleteSecondaryQueue` signature
- Modify: `internal/messaging/mock.go` — update mock
- Modify: `internal/controller/statefulmigration_controller.go` — replay timeout + pass exchange name

**Step 1: Update BrokerClient interface**

In `internal/messaging/client.go`, change `DeleteSecondaryQueue` signature to:

```go
DeleteSecondaryQueue(ctx context.Context, secondaryQueue, primaryQueue, exchangeName string) error
```

**Step 2: Update RabbitMQ implementation**

In `internal/messaging/rabbitmq.go`, update `DeleteSecondaryQueue`:

```go
func (r *RabbitMQClient) DeleteSecondaryQueue(_ context.Context, secondaryQueue, primaryQueue, exchangeName string) error {
	if err := r.ch.QueueUnbind(primaryQueue, "", exchangeName, nil); err != nil {
		return fmt.Errorf("unbind primary queue %q from %q: %w", primaryQueue, exchangeName, err)
	}

	if _, err := r.ch.QueueDelete(secondaryQueue, false, false, false); err != nil {
		return fmt.Errorf("delete queue %q: %w", secondaryQueue, err)
	}

	if err := r.ch.ExchangeDelete(exchangeName, false, false); err != nil {
		return fmt.Errorf("delete exchange %q: %w", exchangeName, err)
	}

	return nil
}
```

Remove the `strings` import and the `baseName`/`exchangeName` derivation lines.

**Step 3: Update MockBrokerClient**

In `internal/messaging/mock.go`, update `DeleteSecondaryQueue`:

```go
func (m *MockBrokerClient) DeleteSecondaryQueue(_ context.Context, secondaryQueue, primaryQueue, exchangeName string) error {
```

(Just add the `exchangeName` parameter, rest stays the same.)

**Step 4: Update handleFinalizing to pass exchange name**

In `handleFinalizing` in the controller, change the `DeleteSecondaryQueue` call from:

```go
if err := r.MsgClient.DeleteSecondaryQueue(ctx, secondaryQueue, m.Spec.MessageQueueConfig.QueueName); err != nil {
```

to:

```go
if err := r.MsgClient.DeleteSecondaryQueue(ctx, secondaryQueue, m.Spec.MessageQueueConfig.QueueName, m.Spec.MessageQueueConfig.ExchangeName); err != nil {
```

**Step 5: Add replay timeout to handleReplaying**

In `handleReplaying`, after the queue depth check (`if depth == 0 {`), add a timeout check before the "still draining" return. Insert this block before `return ctrl.Result{RequeueAfter: 2 * time.Second}, nil`:

```go
// Check if we've exceeded the replay cutoff timeout
if m.Spec.ReplayCutoffSeconds > 0 {
    if startStr, ok := m.Status.PhaseTimings["Replaying.start"]; ok {
        if startTime, parseErr := time.Parse(time.RFC3339, startStr); parseErr == nil {
            elapsed := time.Since(startTime)
            cutoff := time.Duration(m.Spec.ReplayCutoffSeconds) * time.Second
            if elapsed > cutoff {
                logger.Info("Replay cutoff reached, proceeding to finalization",
                    "elapsed", elapsed, "cutoff", cutoff, "remainingDepth", depth)
                var duration time.Duration
                duration = elapsed
                delete(m.Status.PhaseTimings, "Replaying.start")
                r.recordPhaseTiming(m, "Replaying", duration)
                return r.transitionPhase(ctx, m, migrationv1alpha1.PhaseFinalizing)
            }
        }
    }
}
```

**Step 6: Run tests**

```bash
go test ./...
```

Expected: All tests pass. The mock already handles the new parameter.

**Step 7: Commit**

```bash
git add internal/messaging/ internal/controller/statefulmigration_controller.go
git commit -m "fix: implement replay cutoff timeout and pass exchange name to DeleteSecondaryQueue"
```

---

## Task 8: Update Tests for All Fixes

Add and update tests to cover the fixes from Tasks 1-7.

**Files:**
- Modify: `internal/controller/statefulmigration_controller_test.go`

**Step 1: Update newMigration helper to include ContainerName**

In the `newMigration` function, after `Phase: phase`, add:

```go
ContainerName: "app",
```

So the Status section becomes:

```go
Status: migrationv1alpha1.StatefulMigrationStatus{
    Phase:         phase,
    ContainerName: "app",
},
```

**Step 2: Add test for OwnerReference on Transfer Job**

```go
func TestReconcile_Transferring_JobHasOwnerReference(t *testing.T) {
	migration := newMigration("mig-xfer-owner", migrationv1alpha1.PhaseTransferring)
	migration.Status.SourceNode = "node-1"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.PhaseTimings = map[string]string{}
	// Set UID so OwnerReference can reference it
	migration.UID = "test-uid-123"

	r, _, ctx := setupTest(migration)

	_, err := reconcileOnce(r, ctx, "mig-xfer-owner", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: "mig-xfer-owner-transfer", Namespace: "default"}, job); err != nil {
		t.Fatalf("expected transfer job to be created: %v", err)
	}

	if len(job.OwnerReferences) == 0 {
		t.Fatal("expected OwnerReference on transfer job")
	}
	if job.OwnerReferences[0].Name != "mig-xfer-owner" {
		t.Errorf("expected OwnerReference name %q, got %q", "mig-xfer-owner", job.OwnerReferences[0].Name)
	}
}
```

**Step 3: Add test for Transfer Job volume mount**

```go
func TestReconcile_Transferring_JobHasVolumeMount(t *testing.T) {
	migration := newMigration("mig-xfer-vol", migrationv1alpha1.PhaseTransferring)
	migration.Status.SourceNode = "node-1"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.PhaseTimings = map[string]string{}

	r, _, ctx := setupTest(migration)

	_, err := reconcileOnce(r, ctx, "mig-xfer-vol", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: "mig-xfer-vol-transfer", Namespace: "default"}, job); err != nil {
		t.Fatalf("expected transfer job: %v", err)
	}

	containers := job.Spec.Template.Spec.Containers
	if len(containers) == 0 || len(containers[0].VolumeMounts) == 0 {
		t.Fatal("expected volume mount on transfer job container")
	}
	if containers[0].VolumeMounts[0].MountPath != "/var/lib/kubelet/checkpoints" {
		t.Errorf("expected mountPath %q, got %q", "/var/lib/kubelet/checkpoints", containers[0].VolumeMounts[0].MountPath)
	}

	volumes := job.Spec.Template.Spec.Volumes
	if len(volumes) == 0 || volumes[0].HostPath == nil {
		t.Fatal("expected hostPath volume on transfer job")
	}
}
```

**Step 4: Add test for ContainerName auto-detection**

```go
func TestReconcile_Pending_DetectsContainerName(t *testing.T) {
	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "my-app-container", Image: "myapp:latest"},
			},
		},
	}

	migration := newMigration("mig-cname", migrationv1alpha1.PhasePending)
	// Clear ContainerName to test auto-detection
	migration.Spec.ContainerName = ""
	migration.Status.ContainerName = ""

	r, _, ctx := setupTest(migration, sourcePod)

	_, err := reconcileOnce(r, ctx, "mig-cname", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-cname", "default")
	if got.Status.ContainerName != "my-app-container" {
		t.Errorf("expected containerName %q, got %q", "my-app-container", got.Status.ContainerName)
	}
}
```

**Step 5: Add test for ReplayCutoffSeconds timeout**

```go
func TestReconcile_Replaying_CutoffTimeout(t *testing.T) {
	migration := newMigration("mig-cutoff", migrationv1alpha1.PhaseReplaying)
	migration.Status.TargetPod = "myapp-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Spec.ReplayCutoffSeconds = 1 // 1 second timeout
	migration.Status.PhaseTimings = map[string]string{
		// Set start time in the past (2 seconds ago)
		"Replaying.start": time.Now().Add(-2 * time.Second).Format(time.RFC3339),
	}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.Connected = true
	// Queue still has messages (not drained)
	secondaryQ := migration.Spec.MessageQueueConfig.QueueName + ".ms2m-replay"
	mockBroker.SetQueueDepth(secondaryQ, 50)

	_, err := reconcileOnce(r, ctx, "mig-cutoff", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-cutoff", "default")
	// Should have transitioned to Finalizing despite queue not being drained
	if got.Status.Phase != migrationv1alpha1.PhaseFinalizing {
		t.Errorf("expected phase %q after cutoff, got %q", migrationv1alpha1.PhaseFinalizing, got.Status.Phase)
	}
}
```

**Step 6: Run all tests**

```bash
go test ./... -v -count=1
```

Expected: All tests pass including the new ones.

**Step 7: Commit**

```bash
git add internal/controller/statefulmigration_controller_test.go
git commit -m "test: add tests for OwnerRef, volume mount, container name, and replay cutoff"
```

---

## Task 9: Fix Infrastructure Scripts

Multiple issues in the IONOS infrastructure scripts prevent actual deployment:
1. `install_k8s.sh` doesn't enable `ContainerCheckpoint` feature gate on the API server
2. `install_k8s.sh` doesn't configure CRI-O CRIU support (`enable_criu_support = true`)
3. Workload RabbitMQ URLs use `rabbitmq:5672` but RabbitMQ is in the `rabbitmq` namespace — needs FQDN
4. `install_k8s.sh` doesn't configure insecure registry for CRI-O
5. Missing step to deploy the controller and workloads
6. `config/manager/manager.yaml` needs correct image reference and ServiceAccount/ClusterRoleBinding

**Files:**
- Modify: `eval/infra/install_k8s.sh`
- Modify: `eval/workloads/producer.yaml`
- Modify: `eval/workloads/consumer.yaml`
- Modify: `config/manager/manager.yaml`
- Create: `config/rbac/service_account.yaml`
- Create: `config/rbac/role_binding.yaml`
- Modify: `eval/scripts/run_evaluation.sh`

**Step 1: Fix install_k8s.sh — kubeadm config with feature gate**

Replace the kubeadm init block (Step 2 in the script) with a version that uses a config file:

In the `run_on "$CONTROL_PLANE_IP" bash -s <<'REMOTE_SCRIPT'` block for Step 2, replace:

```bash
kubeadm init \
    --pod-network-cidr=192.168.0.0/16 \
    --cri-socket=unix:///var/run/crio/crio.sock \
    --upload-certs
```

with:

```bash
cat > /tmp/kubeadm-config.yaml <<'KUBEADM_CONFIG'
apiVersion: kubeadm.k8s.io/v1beta3
kind: InitConfiguration
nodeRegistration:
  criSocket: unix:///var/run/crio/crio.sock
---
apiVersion: kubeadm.k8s.io/v1beta3
kind: ClusterConfiguration
networking:
  podSubnet: 192.168.0.0/16
apiServer:
  extraArgs:
    feature-gates: ContainerCheckpoint=true
KUBEADM_CONFIG

kubeadm init --config /tmp/kubeadm-config.yaml --upload-certs
```

**Step 2: Fix install_k8s.sh — CRI-O CRIU support and insecure registry**

In the `install_node_packages` function, right before `systemctl enable kubelet`, add:

```bash
# Enable CRIU support in CRI-O
mkdir -p /etc/crio/crio.conf.d
cat > /etc/crio/crio.conf.d/10-criu.conf <<CRIO_CONF
[crio.runtime]
enable_criu_support = true
CRIO_CONF

# Configure insecure registry for in-cluster checkpoint images
cat > /etc/crio/crio.conf.d/10-insecure-registry.conf <<CRIO_REG
[crio.image]
insecure_registries = ["registry.registry.svc.cluster.local:5000", "10.0.0.0/8"]
CRIO_REG

systemctl restart crio
```

**Step 3: Fix workload RabbitMQ URLs**

In `eval/workloads/producer.yaml`, change all occurrences of:

```yaml
value: "amqp://guest:guest@rabbitmq:5672/"
```

to:

```yaml
value: "amqp://guest:guest@rabbitmq.rabbitmq.svc.cluster.local:5672/"
```

Do the same in `eval/workloads/consumer.yaml`.

**Step 4: Fix run_evaluation.sh broker URL**

In `eval/scripts/run_evaluation.sh`, change:

```yaml
    brokerUrl: amqp://guest:guest@rabbitmq:5672/
```

to:

```yaml
    brokerUrl: amqp://guest:guest@rabbitmq.rabbitmq.svc.cluster.local:5672/
```

**Step 5: Create ServiceAccount and ClusterRoleBinding**

Create `config/rbac/service_account.yaml`:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: controller-manager
  namespace: ms2m-system
```

Create `config/rbac/role_binding.yaml`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: manager-rolebinding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: manager-role
subjects:
- kind: ServiceAccount
  name: controller-manager
  namespace: ms2m-system
```

**Step 6: Update manager.yaml with correct namespace and image placeholder**

Replace `config/manager/manager.yaml` with:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: ms2m-system
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller-manager
  namespace: ms2m-system
  labels:
    control-plane: controller-manager
spec:
  selector:
    matchLabels:
      control-plane: controller-manager
  replicas: 1
  template:
    metadata:
      labels:
        control-plane: controller-manager
    spec:
      serviceAccountName: controller-manager
      terminationGracePeriodSeconds: 10
      containers:
      - name: manager
        image: MS2M_CONTROLLER_IMAGE
        command: ["/manager"]
        args: ["--leader-elect"]
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8081
          initialDelaySeconds: 15
          periodSeconds: 20
        readinessProbe:
          httpGet:
            path: /readyz
            port: 8081
          initialDelaySeconds: 5
          periodSeconds: 10
        resources:
          limits:
            cpu: 500m
            memory: 128Mi
          requests:
            cpu: 10m
            memory: 64Mi
```

**Step 7: Add deployment steps to install_k8s.sh**

Add a new Step 8 at the end of `install_k8s.sh` (before the final echo block):

```bash
# ------------------------------------------------------------------
# Step 8: Deploy controller and evaluation workloads
# ------------------------------------------------------------------
echo "[8/8] Deploying MS2M controller and workloads..."

# Deploy ServiceAccount and ClusterRoleBinding
SA_FILE="config/rbac/service_account.yaml"
RB_FILE="config/rbac/role_binding.yaml"
MGR_FILE="config/manager/manager.yaml"

for f in "$SA_FILE" "$RB_FILE" "$MGR_FILE"; do
    if [[ -f "$f" ]]; then
        copy_to "$CONTROL_PLANE_IP" "$f" "/tmp/$(basename $f)"
        run_on "$CONTROL_PLANE_IP" "kubectl apply -f /tmp/$(basename $f)"
    fi
done

# Deploy evaluation workloads
for wl in eval/workloads/*.yaml; do
    copy_to "$CONTROL_PLANE_IP" "$wl" "/tmp/$(basename $wl)"
    run_on "$CONTROL_PLANE_IP" "kubectl apply -f /tmp/$(basename $wl)"
done

echo "  Controller and workloads deployed."
```

Update the step count headers from `[N/7]` to `[N/8]` throughout the script.

**Step 8: Commit**

```bash
git add eval/ config/
git commit -m "fix: enable CRIU in kubeadm/CRI-O, fix cross-namespace URLs, add deployment manifests"
```

---

## Task 10: Build and Push Docker Images

Build both Docker images (controller + transfer) and push to a container registry accessible from the IONOS cluster.

**Prerequisites:**
- Docker installed locally
- Access to a container registry (Docker Hub or GitHub Container Registry)

**Step 1: Build the controller image**

```bash
IMG=ghcr.io/haidinhtuan/ms2m-controller:latest
docker build -t $IMG -f Dockerfile .
```

Expected: Build succeeds.

**Step 2: Build the transfer image**

```bash
TRANSFER_IMG=ghcr.io/haidinhtuan/checkpoint-transfer:latest
docker build -t $TRANSFER_IMG -f Dockerfile.transfer .
```

Expected: Build succeeds.

**Step 3: Log in to the registry**

```bash
echo $GITHUB_TOKEN | docker login ghcr.io -u haidinhtuan --password-stdin
```

**Step 4: Push both images**

```bash
docker push ghcr.io/haidinhtuan/ms2m-controller:latest
docker push ghcr.io/haidinhtuan/checkpoint-transfer:latest
```

**Step 5: Update manifests with actual image references**

In `config/manager/manager.yaml`, replace `MS2M_CONTROLLER_IMAGE` with:
```
ghcr.io/haidinhtuan/ms2m-controller:latest
```

In `internal/controller/statefulmigration_controller.go`, in `handleTransferring`, replace:
```go
Image: "checkpoint-transfer:latest",
```
with:
```go
Image: "ghcr.io/haidinhtuan/checkpoint-transfer:latest",
```

**Step 6: Rebuild and push controller (with updated transfer image reference)**

```bash
docker build -t ghcr.io/haidinhtuan/ms2m-controller:latest -f Dockerfile .
docker push ghcr.io/haidinhtuan/ms2m-controller:latest
```

**Step 7: Commit**

```bash
git add config/manager/manager.yaml internal/controller/statefulmigration_controller.go
git commit -m "chore: set container image references for deployment"
```

---

## Task 11: Provision IONOS Infrastructure and Deploy

Set up the 3-node Kubernetes cluster on IONOS Cloud, install all dependencies, and deploy the MS2M controller and evaluation workloads.

**Prerequisites:**
- `ionosctl` CLI installed and authenticated (use Hai Dinh Tuan / Tuan Hai Dinh token, NOT profitbricks)
- SSH key at `~/.ssh/id_rsa.pub`
- Docker images pushed (Task 10)

**Step 1: Provision IONOS VMs**

```bash
bash eval/infra/setup_ionos.sh
```

Expected: 3 VMs created (control-plane, worker-1, worker-2). State saved to `eval/infra/state.env`.

Wait 3-5 minutes for VMs to boot.

**Step 2: Install Kubernetes and deploy everything**

```bash
bash eval/infra/install_k8s.sh
```

Expected: K8s cluster initialized, Calico CNI installed, RabbitMQ deployed, registry deployed, CRD/RBAC applied, controller deployed, workloads deployed.

**Step 3: Verify the cluster**

```bash
export KUBECONFIG=$(pwd)/eval/infra/kubeconfig
kubectl get nodes
```

Expected: 3 nodes in `Ready` state.

**Step 4: Verify all pods are running**

```bash
kubectl get pods -A
```

Expected: controller-manager, rabbitmq, registry, producer, consumer pods all Running.

**Step 5: Verify CRD is installed**

```bash
kubectl get crd statefulmigrations.migration.ms2m.io
```

Expected: CRD exists.

**Step 6: Verify CRIU works**

SSH into a worker node and test CRIU:

```bash
source eval/infra/state.env
ssh root@$WORKER_1_IP "criu check"
```

Expected: "Looks good." output.

---

## Task 12: End-to-End Testing

Run a manual migration to verify the entire flow works before running the full evaluation.

**Prerequisites:** Task 11 complete, cluster running, workloads deployed.

**Step 1: Verify consumer is processing messages**

```bash
export KUBECONFIG=$(pwd)/eval/infra/kubeconfig
kubectl logs consumer-0 --tail=5
```

Expected: "Processed N messages" log lines.

**Step 2: Identify the target node**

```bash
# Consumer is on one node, migrate to the other worker
CONSUMER_NODE=$(kubectl get pod consumer-0 -o jsonpath='{.spec.nodeName}')
echo "Consumer is on: $CONSUMER_NODE"

# Pick the other worker
TARGET_NODE=$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep -v "$CONSUMER_NODE" | head -1)
echo "Target node: $TARGET_NODE"
```

**Step 3: Create a test migration**

```bash
cat <<EOF | kubectl apply -f -
apiVersion: migration.ms2m.io/v1alpha1
kind: StatefulMigration
metadata:
  name: test-migration-1
spec:
  sourcePod: consumer-0
  targetNode: $TARGET_NODE
  checkpointImageRepository: registry.registry.svc.cluster.local:5000/checkpoints
  replayCutoffSeconds: 120
  messageQueueConfig:
    queueName: app.events
    brokerUrl: amqp://guest:guest@rabbitmq.rabbitmq.svc.cluster.local:5672/
    exchangeName: app.fanout
EOF
```

**Step 4: Monitor the migration phases**

```bash
# Watch the migration status
kubectl get statefulmigration test-migration-1 -w

# In another terminal, check controller logs
kubectl logs -n ms2m-system deployment/controller-manager -f
```

Expected phases in order: `Pending → Checkpointing → Transferring → Restoring → Replaying → Finalizing → Completed`

**Step 5: Debug if any phase fails**

```bash
# Check migration status details
kubectl get statefulmigration test-migration-1 -o yaml

# Check transfer job
kubectl get jobs -l migration.ms2m.io/migration=test-migration-1

# Check shadow pod
kubectl get pods -l migration.ms2m.io/migration=test-migration-1
```

Common issues:
- **Checkpointing fails**: Check kubelet logs on source node, verify ContainerCheckpoint feature gate
- **Transferring fails**: Check transfer job logs, verify checkpoint file exists, verify registry accessible
- **Restoring fails**: Check pod events, verify checkpoint image in registry, verify CRI-O CRIU support
- **Replaying hangs**: Check RabbitMQ queue depth, verify control queue exists

**Step 6: Verify migration succeeded**

```bash
kubectl get statefulmigration test-migration-1 -o jsonpath='{.status.phaseTimings}' | jq .
```

Expected: Phase timings for each phase.

**Step 7: Clean up test migration**

```bash
kubectl delete statefulmigration test-migration-1
```

---

## Task 13: Run Evaluation and Collect Data

Execute the full evaluation: 7 message rates x 10 repetitions = 70 migration runs.

**Prerequisites:** Task 12 passed (end-to-end migration works).

**Step 1: Set environment variables**

```bash
export KUBECONFIG=$(pwd)/eval/infra/kubeconfig

# Get the target node (the worker NOT running consumer-0)
CONSUMER_NODE=$(kubectl get pod consumer-0 -o jsonpath='{.spec.nodeName}')
export TARGET_NODE=$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep -v "$CONSUMER_NODE" | head -1)

export CHECKPOINT_REPO="registry.registry.svc.cluster.local:5000/checkpoints"
```

**Step 2: Run the evaluation**

```bash
bash eval/scripts/run_evaluation.sh
```

Expected: Runs for ~2-3 hours. Produces CSV at `eval/results/migration-metrics-<timestamp>.csv`.

Monitor progress:
```bash
tail -f eval/results/migration-metrics-*.csv
```

**Step 3: Collect any remaining metrics**

```bash
bash eval/scripts/collect_metrics.sh eval/results/collected-metrics.csv
```

**Step 4: Download results**

The results are in `eval/results/`. Copy the CSV files for analysis.

```bash
ls -la eval/results/
```

**Step 5: Tear down infrastructure (when done)**

```bash
bash eval/infra/teardown_ionos.sh
```

Expected: All IONOS resources deleted.

---

## Dependency Graph

```
Task 1 (Dockerfile) ─────────────┐
Task 2 (RBAC) ───────────────────┤
Task 3 (ContainerName) ──┬───────┤
Task 4 (Transfer Job) ───┘  ┌────┤
Task 5 (Transfer Binary) ───┘    ├─→ Task 8 (Tests) ─→ Task 9 (Infra) ─→ Task 10 (Build) ─→ Task 11 (Deploy) ─→ Task 12 (E2E) ─→ Task 13 (Eval)
Task 6 (Restore) ────────────────┤
Task 7 (Replay+Exchange) ────────┘
```

**Parallel groups:**
- Group 1: Tasks 1, 2, 5, 7 (fully independent)
- Group 2: Tasks 3, 4, 6 (Task 4 depends on Task 3 for ContainerName in status)
- Sequential: Tasks 8 → 9 → 10 → 11 → 12 → 13
