# MS2M Controller Optimization Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Reduce total migration time from ~55.7s to <15s by switching to Deployment-based workloads, direct node-to-node checkpoint transfer via DaemonSet agent, and (optionally) background CRIU pre-dumps.

**Architecture:** Three layered optimizations, each independently valuable: (1) consumer workload changes from StatefulSet to Deployment, eliminating the 39.1s scale-down/up cycle; (2) a new `ms2m-agent` DaemonSet transfers checkpoint tars directly between nodes via HTTP, bypassing the OCI registry; (3) the agent periodically runs CRIU pre-dumps so the final checkpoint is a tiny delta. The existing StatefulSet+Sequential path is preserved as the evaluation baseline.

**Tech Stack:** Go 1.25+, controller-runtime, Kubernetes client-go, go-containerregistry, net/http (agent), CRIU 4.0, CRI-O 1.31, skopeo

---

## Task 1: Convert Consumer Workload to Deployment

**Files:**
- Modify: `eval/workloads/consumer.yaml`

The consumer currently uses a StatefulSet with a headless Service. For Deployment-based
migration, convert it to a Deployment. The headless Service stays (for DNS), but is now
optional since the consumer connects outbound to RabbitMQ.

No consumer Python code changes are needed — `socket.gethostname()` returns the pod name
for both StatefulSet and Deployment pods, so control queue naming works as-is.

**Step 1: Create a Deployment variant of the consumer manifest**

Create `eval/workloads/consumer-deployment.yaml` (keep `consumer.yaml` as StatefulSet baseline):

```yaml
apiVersion: v1
kind: Service
metadata:
  name: consumer
spec:
  selector:
    app: consumer
  ports:
  - port: 8080
    name: http
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: consumer
  labels:
    app: consumer
spec:
  replicas: 1
  selector:
    matchLabels:
      app: consumer
  template:
    metadata:
      labels:
        app: consumer
    spec:
      containers:
      - name: consumer
        image: python:3.11-slim
        command: ["/bin/bash", "-c"]
        args:
        - |
          pip install -q pika
          cat > /tmp/consumer.py << 'PYEOF'
          # ... (same Python code as consumer.yaml, no changes)
          PYEOF
          exec python3 /tmp/consumer.py
        env:
        - name: PROCESSING_DELAY_MS
          value: "50"
        - name: RABBITMQ_URL
          value: "amqp://guest:guest@rabbitmq.rabbitmq.svc.cluster.local:5672/"
        - name: QUEUE_NAME
          value: "app.events"
        ports:
        - containerPort: 8080
          name: http
```

Key differences from StatefulSet version:
- `kind: Deployment` instead of `kind: StatefulSet`
- No `serviceName` field
- Service is ClusterIP (not headless `clusterIP: None`)
- Everything else identical

**Step 2: Verify it works**

Deploy to cluster and verify consumer pod starts, connects to RabbitMQ, processes messages.

Run: `kubectl apply -f eval/workloads/consumer-deployment.yaml`
Expected: Pod `consumer-<hash>` running, consuming messages.

**Step 3: Commit**

```bash
git add eval/workloads/consumer-deployment.yaml
git commit -m "eval: add Deployment variant of consumer workload"
```

---

## Task 2: Add Deployment Detection to Controller handlePending

**Files:**
- Modify: `internal/controller/statefulmigration_controller.go:142-160` (strategy detection)
- Modify: `internal/controller/statefulmigration_controller.go:178-184` (owner recording)
- Test: `internal/controller/statefulmigration_controller_test.go`

The current auto-detection checks for `StatefulSet` ownerRef and defaults to `ShadowPod`.
Deployment pods have a `ReplicaSet` ownerRef. We need to also record the Deployment name
(via ReplicaSet -> Deployment lookup) for use in Finalizing.

**Step 1: Write the failing test**

```go
func TestReconcile_Pending_DetectsDeploymentStrategy(t *testing.T) {
    // A pod owned by a ReplicaSet (Deployment child) should auto-detect ShadowPod
    // and record the Deployment name in status.
    rs := &appsv1.ReplicaSet{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "consumer-abc123",
            Namespace: "default",
            OwnerReferences: []metav1.OwnerReference{
                {
                    APIVersion: "apps/v1",
                    Kind:       "Deployment",
                    Name:       "consumer",
                    UID:        "deploy-uid-1",
                    Controller: func() *bool { b := true; return &b }(),
                },
            },
        },
        Spec: appsv1.ReplicaSetSpec{
            Selector: &metav1.LabelSelector{
                MatchLabels: map[string]string{"app": "consumer"},
            },
            Template: corev1.PodTemplateSpec{
                ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "consumer"}},
                Spec: corev1.PodSpec{
                    Containers: []corev1.Container{{Name: "consumer", Image: "python:3.11"}},
                },
            },
        },
    }

    sourcePod := &corev1.Pod{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "consumer-abc123-xyz",
            Namespace: "default",
            OwnerReferences: []metav1.OwnerReference{
                {
                    APIVersion: "apps/v1",
                    Kind:       "ReplicaSet",
                    Name:       "consumer-abc123",
                    UID:        "rs-uid-1",
                    Controller: func() *bool { b := true; return &b }(),
                },
            },
        },
        Spec: corev1.PodSpec{
            NodeName:   "node-1",
            Containers: []corev1.Container{{Name: "consumer", Image: "python:3.11"}},
        },
    }

    migration := newMigration("mig-deploy", "")
    migration.Spec.SourcePod = "consumer-abc123-xyz"

    r, _, ctx := setupTest(migration, sourcePod, rs)

    _, err := reconcileOnce(r, ctx, "mig-deploy", "default")
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }

    got := fetchMigration(r, ctx, "mig-deploy", "default")
    if got.Spec.MigrationStrategy != "ShadowPod" {
        t.Errorf("expected strategy ShadowPod, got %q", got.Spec.MigrationStrategy)
    }
    if got.Status.DeploymentName != "consumer" {
        t.Errorf("expected deploymentName %q, got %q", "consumer", got.Status.DeploymentName)
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestReconcile_Pending_DetectsDeploymentStrategy -v`
Expected: FAIL — `DeploymentName` field doesn't exist on status yet.

**Step 3: Add DeploymentName field to status**

In `api/v1alpha1/types.go`, add to `StatefulMigrationStatus`:

```go
// DeploymentName is the name of the owning Deployment (for ShadowPod Finalizing)
DeploymentName string `json:"deploymentName,omitempty"`
```

Update `deepcopy.go` if needed (the field is a string, so DeepCopy works automatically).

**Step 4: Implement Deployment detection in handlePending**

In `statefulmigration_controller.go` `handlePending`, after the existing StatefulSet detection
block (lines 143-160), add ReplicaSet -> Deployment lookup:

```go
// Auto-detect migration strategy from ownerReferences if not explicitly set
if m.Spec.MigrationStrategy == "" {
    strategy := "ShadowPod"
    for _, ref := range sourcePod.OwnerReferences {
        if ref.Kind == "StatefulSet" {
            strategy = "Sequential"
            break
        }
    }
    m.Spec.MigrationStrategy = strategy
    // Persist the detected strategy on the spec
    if err := r.Update(ctx, m); err != nil {
        return ctrl.Result{}, err
    }
    // Re-fetch after spec update so status writes use the current resourceVersion
    if err := r.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, m); err != nil {
        return ctrl.Result{}, err
    }
}

// (existing code continues...)

// Record owning workload controller for use during Finalizing
for _, ref := range sourcePod.OwnerReferences {
    if ref.Kind == "StatefulSet" {
        m.Status.StatefulSetName = ref.Name
        break
    }
    if ref.Kind == "ReplicaSet" {
        // Look up the ReplicaSet to find its parent Deployment
        rs := &appsv1.ReplicaSet{}
        if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: m.Namespace}, rs); err == nil {
            for _, rsRef := range rs.OwnerReferences {
                if rsRef.Kind == "Deployment" {
                    m.Status.DeploymentName = rsRef.Name
                    break
                }
            }
        }
        break
    }
}
```

**Step 5: Update CRD YAML**

Add `deploymentName` to `config/crd/bases/migration.ms2m.io_statefulmigrations.yaml` under
`status.properties`.

**Step 6: Run test to verify it passes**

Run: `go test ./internal/controller/ -run TestReconcile_Pending_DetectsDeploymentStrategy -v`
Expected: PASS

**Step 7: Run full test suite**

Run: `make test`
Expected: All existing tests still pass.

**Step 8: Commit**

```bash
git add api/v1alpha1/types.go api/v1alpha1/deepcopy.go \
  internal/controller/statefulmigration_controller.go \
  internal/controller/statefulmigration_controller_test.go \
  config/crd/bases/migration.ms2m.io_statefulmigrations.yaml
git commit -m "feat: detect Deployment owner and record in migration status"
```

---

## Task 3: Update handleFinalizing for Deployment Workloads

**Files:**
- Modify: `internal/controller/statefulmigration_controller.go:609-668` (handleFinalizing)
- Test: `internal/controller/statefulmigration_controller_test.go`

After ShadowPod migration of a Deployment pod, Finalizing must:
1. Send END_REPLAY, delete replay queue (same as now)
2. Patch the Deployment with nodeAffinity for the target node
3. Delete source pod
4. The Deployment controller recreates a replacement on the target node
5. Delete shadow pod once no longer needed

**Step 1: Write the failing test**

```go
func TestReconcile_Finalizing_ShadowPod_PatchesDeployment(t *testing.T) {
    // When a Deployment-owned pod is migrated via ShadowPod, Finalizing should
    // patch the Deployment's pod template with nodeAffinity for the target node.
    deploy := &appsv1.Deployment{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "consumer",
            Namespace: "default",
        },
        Spec: appsv1.DeploymentSpec{
            Replicas: func() *int32 { r := int32(1); return &r }(),
            Selector: &metav1.LabelSelector{
                MatchLabels: map[string]string{"app": "consumer"},
            },
            Template: corev1.PodTemplateSpec{
                ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "consumer"}},
                Spec: corev1.PodSpec{
                    Containers: []corev1.Container{{Name: "consumer", Image: "python:3.11"}},
                },
            },
        },
    }

    sourcePod := &corev1.Pod{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "consumer-abc-xyz",
            Namespace: "default",
        },
        Spec: corev1.PodSpec{
            NodeName:   "node-1",
            Containers: []corev1.Container{{Name: "consumer", Image: "python:3.11"}},
        },
    }

    migration := newMigration("mig-final-deploy", migrationv1alpha1.PhaseFinalizing)
    migration.Spec.SourcePod = "consumer-abc-xyz"
    migration.Spec.MigrationStrategy = "ShadowPod"
    migration.Spec.TargetNode = "node-2"
    migration.Status.TargetPod = "consumer-abc-xyz-shadow"
    migration.Status.SourceNode = "node-1"
    migration.Status.DeploymentName = "consumer"
    migration.Status.PhaseTimings = map[string]string{}

    r, mockBroker, ctx := setupTest(migration, sourcePod, deploy)
    mockBroker.Connected = true

    _, err := reconcileOnce(r, ctx, "mig-final-deploy", "default")
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }

    got := fetchMigration(r, ctx, "mig-final-deploy", "default")
    if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
        t.Errorf("expected phase %q, got %q", migrationv1alpha1.PhaseCompleted, got.Status.Phase)
    }

    // Verify the Deployment was patched with nodeAffinity
    updatedDeploy := &appsv1.Deployment{}
    if err := r.Get(ctx, types.NamespacedName{Name: "consumer", Namespace: "default"}, updatedDeploy); err != nil {
        t.Fatalf("failed to get deployment: %v", err)
    }
    affinity := updatedDeploy.Spec.Template.Spec.Affinity
    if affinity == nil || affinity.NodeAffinity == nil {
        t.Fatal("expected Deployment to have nodeAffinity after migration")
    }

    // Verify source pod was deleted
    pod := &corev1.Pod{}
    if err := r.Get(ctx, types.NamespacedName{Name: "consumer-abc-xyz", Namespace: "default"}, pod); err == nil {
        t.Error("expected source pod to be deleted")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestReconcile_Finalizing_ShadowPod_PatchesDeployment -v`
Expected: FAIL — Deployment is not patched.

**Step 3: Implement Deployment patching in handleFinalizing**

In `handleFinalizing`, after deleting the source pod in ShadowPod strategy, add:

```go
// For Deployment-owned pods, patch the Deployment's pod template with
// nodeAffinity so the replacement pod lands on the target node.
if m.Status.DeploymentName != "" {
    deploy := &appsv1.Deployment{}
    if err := r.Get(ctx, types.NamespacedName{Name: m.Status.DeploymentName, Namespace: m.Namespace}, deploy); err == nil {
        deployPatch := client.MergeFrom(deploy.DeepCopy())
        if deploy.Spec.Template.Spec.Affinity == nil {
            deploy.Spec.Template.Spec.Affinity = &corev1.Affinity{}
        }
        deploy.Spec.Template.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{
            RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
                NodeSelectorTerms: []corev1.NodeSelectorTerm{
                    {
                        MatchExpressions: []corev1.NodeSelectorRequirement{
                            {
                                Key:      "kubernetes.io/hostname",
                                Operator: corev1.NodeSelectorOpIn,
                                Values:   []string{m.Spec.TargetNode},
                            },
                        },
                    },
                },
            },
        }
        if err := r.Patch(ctx, deploy, deployPatch); err != nil {
            logger.Error(err, "Failed to patch Deployment with nodeAffinity", "deployment", m.Status.DeploymentName)
        } else {
            logger.Info("Patched Deployment with nodeAffinity", "deployment", m.Status.DeploymentName, "targetNode", m.Spec.TargetNode)
        }
    } else if !errors.IsNotFound(err) {
        return ctrl.Result{}, err
    }
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/controller/ -run TestReconcile_Finalizing_ShadowPod_PatchesDeployment -v`
Expected: PASS

**Step 5: Run full test suite**

Run: `make test`
Expected: All tests pass.

**Step 6: Commit**

```bash
git add internal/controller/statefulmigration_controller.go \
  internal/controller/statefulmigration_controller_test.go
git commit -m "feat: patch Deployment nodeAffinity during ShadowPod finalization"
```

---

## Task 4: Add TransferMode to CRD and Controller

**Files:**
- Modify: `api/v1alpha1/types.go`
- Modify: `api/v1alpha1/deepcopy.go` (if needed)
- Modify: `config/crd/bases/migration.ms2m.io_statefulmigrations.yaml`
- Test: `internal/controller/statefulmigration_controller_test.go`

Add a `transferMode` field to the spec: `"Registry"` (default, current behavior) or
`"Direct"` (new: HTTP POST to ms2m-agent on target node).

**Step 1: Write the failing test**

```go
func TestReconcile_Transferring_DirectMode_CreatesJob(t *testing.T) {
    migration := newMigration("mig-direct", migrationv1alpha1.PhaseTransferring)
    migration.Spec.TransferMode = "Direct"
    migration.Status.SourceNode = "node-1"
    migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
    migration.Status.ContainerName = "app"
    migration.Status.PhaseTimings = map[string]string{}

    r, _, ctx := setupTest(migration)

    _, err := reconcileOnce(r, ctx, "mig-direct", "default")
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }

    // Verify a transfer job was created with Direct mode args
    job := &batchv1.Job{}
    if err := r.Get(ctx, types.NamespacedName{Name: "mig-direct-transfer", Namespace: "default"}, job); err != nil {
        t.Fatalf("expected transfer job to be created: %v", err)
    }

    // Direct mode job should target the ms2m-agent on target node, not the registry
    args := job.Spec.Template.Spec.Containers[0].Args
    // Args should contain: checkpoint-path, target-agent-url, container-name
    if len(args) < 2 {
        t.Fatal("expected at least 2 args for Direct transfer job")
    }
    // The second arg should be an HTTP URL to the agent, not an OCI image ref
    if args[1] == "" {
        t.Error("expected target agent URL in job args")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestReconcile_Transferring_DirectMode_CreatesJob -v`
Expected: FAIL — `TransferMode` field doesn't exist.

**Step 3: Add TransferMode field**

In `api/v1alpha1/types.go`, add to `StatefulMigrationSpec`:

```go
// TransferMode controls how the checkpoint is moved to the target node.
// "Registry" (default): build OCI image, push to registry, target pulls.
// "Direct": POST checkpoint tar to ms2m-agent on target node via HTTP.
TransferMode string `json:"transferMode,omitempty"`
```

**Step 4: Update handleTransferring for Direct mode**

In `handleTransferring`, when creating the Job, branch on `TransferMode`:

```go
if m.Spec.TransferMode == "Direct" {
    // Direct mode: POST checkpoint tar to ms2m-agent on target node
    agentURL := fmt.Sprintf("http://ms2m-agent.ms2m-system.svc.cluster.local:9443/checkpoint?node=%s&container=%s",
        m.Spec.TargetNode, m.Status.ContainerName)
    job.Spec.Template.Spec.Containers[0].Args = []string{m.Status.CheckpointID, agentURL, m.Status.ContainerName}
    job.Spec.Template.Spec.Containers[0].Image = "checkpoint-transfer:latest"
} else {
    // Registry mode: existing behavior
    imageRef := fmt.Sprintf("%s/%s:checkpoint", m.Spec.CheckpointImageRepository, m.Spec.SourcePod)
    job.Spec.Template.Spec.Containers[0].Args = []string{m.Status.CheckpointID, imageRef, m.Status.ContainerName}
}
```

**Note:** The full direct transfer implementation requires the ms2m-agent (Task 6) and
an updated checkpoint-transfer tool (Task 5). This task only adds the CRD field and
controller branching logic. The transfer Job will be updated to call the agent in Task 5.

**Step 5: Update CRD YAML**

Add `transferMode` to spec properties in the CRD YAML.

**Step 6: Run tests**

Run: `make test`
Expected: All tests pass (both new and existing).

**Step 7: Commit**

```bash
git add api/v1alpha1/types.go config/crd/bases/migration.ms2m.io_statefulmigrations.yaml \
  internal/controller/statefulmigration_controller.go \
  internal/controller/statefulmigration_controller_test.go
git commit -m "feat: add TransferMode field to CRD spec (Registry/Direct)"
```

---

## Task 5: Refactor checkpoint-transfer for Direct Mode

**Files:**
- Modify: `cmd/checkpoint-transfer/main.go`
- Create: `internal/checkpoint/image.go` (shared OCI image building logic)
- Test: `internal/checkpoint/image_test.go`

Extract OCI image building into a shared package. Add a `direct` mode to the
checkpoint-transfer CLI that POSTs the tar to an HTTP endpoint instead of pushing
to a registry.

**Step 1: Write the failing test for shared package**

Create `internal/checkpoint/image_test.go`:

```go
package checkpoint

import (
    "os"
    "path/filepath"
    "testing"
)

func TestBuildCheckpointImage(t *testing.T) {
    // Create a minimal tar file for testing
    tmpDir := t.TempDir()
    tarPath := filepath.Join(tmpDir, "checkpoint.tar")
    if err := os.WriteFile(tarPath, []byte("fake checkpoint data"), 0644); err != nil {
        t.Fatal(err)
    }

    img, err := BuildCheckpointImage(tarPath, "mycontainer")
    if err != nil {
        t.Fatalf("BuildCheckpointImage failed: %v", err)
    }
    if img == nil {
        t.Fatal("expected non-nil image")
    }

    // Verify annotation
    manifest, err := img.Manifest()
    if err != nil {
        t.Fatal(err)
    }
    if manifest.Annotations["io.kubernetes.cri-o.annotations.checkpoint.name"] != "mycontainer" {
        t.Error("expected checkpoint annotation to be set")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/checkpoint/ -run TestBuildCheckpointImage -v`
Expected: FAIL — package doesn't exist.

**Step 3: Create shared checkpoint package**

Create `internal/checkpoint/image.go` by extracting `buildCheckpointImage` from
`cmd/checkpoint-transfer/main.go`:

```go
package checkpoint

import (
    "compress/gzip"
    "fmt"

    v1 "github.com/google/go-containerregistry/pkg/v1"
    "github.com/google/go-containerregistry/pkg/v1/empty"
    "github.com/google/go-containerregistry/pkg/v1/mutate"
    "github.com/google/go-containerregistry/pkg/v1/tarball"
    ocitype "github.com/google/go-containerregistry/pkg/v1/types"
)

// BuildCheckpointImage creates a single-layer OCI image from a CRIU checkpoint tarball.
func BuildCheckpointImage(checkpointPath, containerName string) (v1.Image, error) {
    layer, err := tarball.LayerFromFile(checkpointPath, tarball.WithCompressionLevel(gzip.NoCompression))
    if err != nil {
        return nil, fmt.Errorf("creating layer from checkpoint: %w", err)
    }

    base := mutate.MediaType(empty.Image, ocitype.OCIManifestSchema1)
    base = mutate.ConfigMediaType(base, ocitype.OCIConfigJSON)

    img, err := mutate.AppendLayers(base, layer)
    if err != nil {
        return nil, fmt.Errorf("appending layer to image: %w", err)
    }

    if containerName != "" {
        img = mutate.Annotations(img, map[string]string{
            "io.kubernetes.cri-o.annotations.checkpoint.name": containerName,
        }).(v1.Image)
    }

    return img, nil
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/checkpoint/ -run TestBuildCheckpointImage -v`
Expected: PASS

**Step 5: Update checkpoint-transfer CLI to use shared package and add direct mode**

Rewrite `cmd/checkpoint-transfer/main.go`:

```go
package main

import (
    "bytes"
    "fmt"
    "io"
    "mime/multipart"
    "net/http"
    "os"
    "time"

    "github.com/google/go-containerregistry/pkg/authn"
    "github.com/google/go-containerregistry/pkg/crane"
    "github.com/haidinhtuan/kubernetes-controller/internal/checkpoint"
)

func main() {
    if len(os.Args) < 3 || len(os.Args) > 4 {
        fmt.Fprintf(os.Stderr, "usage: %s <checkpoint-tar-path> <target> [container-name]\n", os.Args[0])
        fmt.Fprintf(os.Stderr, "  target: OCI image ref (registry mode) or http://... URL (direct mode)\n")
        os.Exit(1)
    }

    checkpointPath := os.Args[1]
    target := os.Args[2]
    containerName := ""
    if len(os.Args) == 4 {
        containerName = os.Args[3]
    }

    totalStart := time.Now()

    // Detect mode from target format
    if len(target) > 4 && target[:4] == "http" {
        // Direct mode: POST tar to ms2m-agent
        directTransfer(checkpointPath, target, containerName, totalStart)
    } else {
        // Registry mode: build OCI image and push
        registryTransfer(checkpointPath, target, containerName, totalStart)
    }
}

func directTransfer(checkpointPath, agentURL, containerName string, totalStart time.Time) {
    fmt.Printf("Direct transfer: %s -> %s\n", checkpointPath, agentURL)

    f, err := os.Open(checkpointPath)
    if err != nil {
        fmt.Fprintf(os.Stderr, "error opening checkpoint: %v\n", err)
        os.Exit(1)
    }
    defer f.Close()

    // Build multipart request with tar file and container name
    var buf bytes.Buffer
    writer := multipart.NewWriter(&buf)
    part, err := writer.CreateFormFile("checkpoint", "checkpoint.tar")
    if err != nil {
        fmt.Fprintf(os.Stderr, "error creating form: %v\n", err)
        os.Exit(1)
    }
    if _, err := io.Copy(part, f); err != nil {
        fmt.Fprintf(os.Stderr, "error copying tar: %v\n", err)
        os.Exit(1)
    }
    if containerName != "" {
        _ = writer.WriteField("containerName", containerName)
    }
    writer.Close()

    resp, err := http.Post(agentURL, writer.FormDataContentType(), &buf)
    if err != nil {
        fmt.Fprintf(os.Stderr, "error posting to agent: %v\n", err)
        os.Exit(1)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        body, _ := io.ReadAll(resp.Body)
        fmt.Fprintf(os.Stderr, "agent returned %d: %s\n", resp.StatusCode, body)
        os.Exit(1)
    }

    fmt.Printf("Direct transfer complete (total: %s)\n", time.Since(totalStart))
}

func registryTransfer(checkpointPath, imageRef, containerName string, totalStart time.Time) {
    fmt.Printf("Building checkpoint image from %s\n", checkpointPath)
    buildStart := time.Now()
    img, err := checkpoint.BuildCheckpointImage(checkpointPath, containerName)
    if err != nil {
        fmt.Fprintf(os.Stderr, "error building image: %v\n", err)
        os.Exit(1)
    }
    fmt.Printf("Image built in %s\n", time.Since(buildStart))

    fmt.Printf("Pushing image to %s\n", imageRef)
    pushStart := time.Now()

    opts := []crane.Option{crane.WithAuthFromKeychain(authn.DefaultKeychain)}
    if os.Getenv("INSECURE_REGISTRY") != "" {
        opts = append(opts, crane.Insecure)
    }

    if err := crane.Push(img, imageRef, opts...); err != nil {
        fmt.Fprintf(os.Stderr, "error pushing image: %v\n", err)
        os.Exit(1)
    }

    fmt.Printf("Image pushed in %s (total: %s)\n", time.Since(pushStart), time.Since(totalStart))
}
```

**Step 6: Run all tests**

Run: `make test`
Expected: All tests pass.

**Step 7: Commit**

```bash
git add internal/checkpoint/image.go internal/checkpoint/image_test.go \
  cmd/checkpoint-transfer/main.go
git commit -m "refactor: extract checkpoint image building, add direct transfer mode"
```

---

## Task 6: Build the ms2m-agent DaemonSet

**Files:**
- Create: `cmd/ms2m-agent/main.go`
- Create: `config/daemonset/ms2m-agent.yaml`
- Test: `cmd/ms2m-agent/main_test.go`

The agent runs on every worker node and:
1. Listens on port 9443 for checkpoint tar uploads via HTTP POST
2. Receives the tar, builds an OCI image using the shared checkpoint package
3. Loads the image into CRI-O's local store via `skopeo copy`

**Step 1: Write the agent test**

Create `cmd/ms2m-agent/main_test.go`:

```go
package main

import (
    "bytes"
    "io"
    "mime/multipart"
    "net/http"
    "net/http/httptest"
    "os"
    "path/filepath"
    "testing"
)

func TestHandleCheckpointUpload(t *testing.T) {
    // Create a temp dir for storage
    tmpDir := t.TempDir()
    handler := &checkpointHandler{storageDir: tmpDir, skipLoad: true}

    // Create a fake checkpoint tar
    tarData := []byte("fake tar content for testing")

    var buf bytes.Buffer
    writer := multipart.NewWriter(&buf)
    part, _ := writer.CreateFormFile("checkpoint", "checkpoint.tar")
    part.Write(tarData)
    writer.WriteField("containerName", "mycontainer")
    writer.Close()

    req := httptest.NewRequest(http.MethodPost, "/checkpoint", &buf)
    req.Header.Set("Content-Type", writer.FormDataContentType())
    rr := httptest.NewRecorder()

    handler.ServeHTTP(rr, req)

    if rr.Code != http.StatusOK {
        t.Errorf("expected 200, got %d: %s", rr.Code, rr.Body.String())
    }

    // Verify tar was written to storage
    entries, _ := os.ReadDir(tmpDir)
    if len(entries) == 0 {
        t.Error("expected checkpoint tar to be stored")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./cmd/ms2m-agent/ -run TestHandleCheckpointUpload -v`
Expected: FAIL — package doesn't exist.

**Step 3: Implement the agent**

Create `cmd/ms2m-agent/main.go`:

```go
package main

import (
    "fmt"
    "io"
    "net/http"
    "os"
    "os/exec"
    "path/filepath"
    "time"

    "github.com/haidinhtuan/kubernetes-controller/internal/checkpoint"
    "github.com/google/go-containerregistry/pkg/crane"
    "github.com/google/go-containerregistry/pkg/v1/layout"
)

type checkpointHandler struct {
    storageDir string
    skipLoad   bool // for testing: skip skopeo load
}

func (h *checkpointHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }

    // Parse multipart form (max 500MB)
    if err := r.ParseMultipartForm(500 << 20); err != nil {
        http.Error(w, fmt.Sprintf("parse form: %v", err), http.StatusBadRequest)
        return
    }

    file, _, err := r.FormFile("checkpoint")
    if err != nil {
        http.Error(w, fmt.Sprintf("get file: %v", err), http.StatusBadRequest)
        return
    }
    defer file.Close()

    containerName := r.FormValue("containerName")

    // Write tar to local storage
    tarPath := filepath.Join(h.storageDir, fmt.Sprintf("checkpoint-%d.tar", time.Now().UnixNano()))
    out, err := os.Create(tarPath)
    if err != nil {
        http.Error(w, fmt.Sprintf("create file: %v", err), http.StatusInternalServerError)
        return
    }
    if _, err := io.Copy(out, file); err != nil {
        out.Close()
        http.Error(w, fmt.Sprintf("write file: %v", err), http.StatusInternalServerError)
        return
    }
    out.Close()

    fmt.Printf("Received checkpoint tar: %s (%s)\n", tarPath, containerName)

    // Build OCI image from tar
    img, err := checkpoint.BuildCheckpointImage(tarPath, containerName)
    if err != nil {
        http.Error(w, fmt.Sprintf("build image: %v", err), http.StatusInternalServerError)
        return
    }

    // Save as OCI layout for skopeo to load
    layoutDir := tarPath + "-oci"
    if err := os.MkdirAll(layoutDir, 0755); err != nil {
        http.Error(w, fmt.Sprintf("mkdir: %v", err), http.StatusInternalServerError)
        return
    }
    layoutPath, err := layout.Write(layoutDir, img)
    if err != nil {
        http.Error(w, fmt.Sprintf("write layout: %v", err), http.StatusInternalServerError)
        return
    }
    _ = layoutPath

    if !h.skipLoad {
        // Load into CRI-O's container storage via skopeo
        imageTag := fmt.Sprintf("localhost/checkpoint/%s:latest", containerName)
        cmd := exec.Command("skopeo", "copy",
            "oci:"+layoutDir,
            "containers-storage:"+imageTag)
        output, err := cmd.CombinedOutput()
        if err != nil {
            http.Error(w, fmt.Sprintf("skopeo copy: %v: %s", err, output), http.StatusInternalServerError)
            return
        }
        fmt.Printf("Loaded image into CRI-O: %s\n", imageTag)
    }

    // Cleanup tar and OCI layout
    os.Remove(tarPath)
    os.RemoveAll(layoutDir)

    w.WriteHeader(http.StatusOK)
    fmt.Fprintf(w, "checkpoint loaded successfully")
}

func main() {
    storageDir := os.Getenv("STORAGE_DIR")
    if storageDir == "" {
        storageDir = "/var/lib/ms2m/incoming"
    }
    os.MkdirAll(storageDir, 0755)

    handler := &checkpointHandler{storageDir: storageDir}

    port := os.Getenv("PORT")
    if port == "" {
        port = "9443"
    }

    fmt.Printf("ms2m-agent listening on :%s\n", port)
    if err := http.ListenAndServe(":"+port, handler); err != nil {
        fmt.Fprintf(os.Stderr, "server error: %v\n", err)
        os.Exit(1)
    }
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./cmd/ms2m-agent/ -run TestHandleCheckpointUpload -v`
Expected: PASS

**Step 5: Create DaemonSet manifest**

Create `config/daemonset/ms2m-agent.yaml`:

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ms2m-agent
  namespace: ms2m-system
spec:
  selector:
    matchLabels:
      app: ms2m-agent
  template:
    metadata:
      labels:
        app: ms2m-agent
    spec:
      hostPID: true
      containers:
      - name: agent
        image: ms2m-agent:latest
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 9443
          name: http
        env:
        - name: STORAGE_DIR
          value: "/var/lib/ms2m/incoming"
        securityContext:
          privileged: true
        volumeMounts:
        - name: checkpoints
          mountPath: /var/lib/kubelet/checkpoints
          readOnly: true
        - name: ms2m-storage
          mountPath: /var/lib/ms2m
        - name: containers-storage
          mountPath: /var/lib/containers/storage
      volumes:
      - name: checkpoints
        hostPath:
          path: /var/lib/kubelet/checkpoints
      - name: ms2m-storage
        hostPath:
          path: /var/lib/ms2m
      - name: containers-storage
        hostPath:
          path: /var/lib/containers/storage
---
apiVersion: v1
kind: Service
metadata:
  name: ms2m-agent
  namespace: ms2m-system
spec:
  selector:
    app: ms2m-agent
  ports:
  - port: 9443
    targetPort: 9443
    name: http
```

**Step 6: Add Dockerfile and Makefile targets**

Add to Makefile:
```makefile
agent-build:
	go build -o bin/ms2m-agent ./cmd/ms2m-agent

agent-docker-build:
	docker build -f Dockerfile.agent -t ms2m-agent:latest .
```

Create `Dockerfile.agent`:
```dockerfile
FROM golang:1.25 AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o ms2m-agent ./cmd/ms2m-agent

FROM ubuntu:22.04
RUN apt-get update && apt-get install -y skopeo && rm -rf /var/lib/apt/lists/*
COPY --from=builder /workspace/ms2m-agent /usr/local/bin/ms2m-agent
ENTRYPOINT ["ms2m-agent"]
```

**Step 7: Run all tests**

Run: `make test`
Expected: All tests pass.

**Step 8: Commit**

```bash
git add cmd/ms2m-agent/ config/daemonset/ Dockerfile.agent Makefile
git commit -m "feat: add ms2m-agent DaemonSet for direct checkpoint transfer"
```

---

## Task 7: Wire Direct Transfer End-to-End in Controller

**Files:**
- Modify: `internal/controller/statefulmigration_controller.go` (handleTransferring, handleRestoring)
- Test: `internal/controller/statefulmigration_controller_test.go`

Update handleTransferring to create the right Job args for Direct mode, and
handleRestoring to use `imagePullPolicy: Never` when the image was loaded locally.

**Step 1: Write the failing test for Direct mode image pull policy**

```go
func TestReconcile_Restoring_DirectMode_UsesNeverPullPolicy(t *testing.T) {
    migration := newMigration("mig-restore-direct", migrationv1alpha1.PhaseRestoring)
    migration.Spec.MigrationStrategy = "ShadowPod"
    migration.Spec.TransferMode = "Direct"
    migration.Status.SourceNode = "node-1"
    migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
    migration.Status.ContainerName = "app"
    migration.Status.PhaseTimings = map[string]string{}

    sourcePod := &corev1.Pod{
        ObjectMeta: metav1.ObjectMeta{Name: "myapp-0", Namespace: "default"},
        Spec: corev1.PodSpec{
            NodeName:   "node-1",
            Containers: []corev1.Container{{Name: "app", Image: "myapp:latest"}},
        },
    }

    r, _, ctx := setupTest(migration, sourcePod)

    _, err := reconcileOnce(r, ctx, "mig-restore-direct", "default")
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }

    // Verify shadow pod has imagePullPolicy: Never (image is pre-loaded by agent)
    targetPod := &corev1.Pod{}
    if err := r.Get(ctx, types.NamespacedName{Name: "myapp-0-shadow", Namespace: "default"}, targetPod); err != nil {
        t.Fatalf("expected shadow pod: %v", err)
    }
    if targetPod.Spec.Containers[0].ImagePullPolicy != corev1.PullNever {
        t.Errorf("expected PullNever for Direct mode, got %v", targetPod.Spec.Containers[0].ImagePullPolicy)
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestReconcile_Restoring_DirectMode_UsesNeverPullPolicy -v`
Expected: FAIL — still uses PullAlways.

**Step 3: Implement Direct mode handling in handleRestoring**

In handleRestoring, when building the container spec, check TransferMode:

```go
pullPolicy := corev1.PullAlways
if m.Spec.TransferMode == "Direct" {
    pullPolicy = corev1.PullNever
    // Direct mode: image tag matches what ms2m-agent loaded
    checkpointImage = fmt.Sprintf("localhost/checkpoint/%s:latest", m.Status.ContainerName)
}

// ... then use pullPolicy when building containers:
restored := corev1.Container{
    Name:            c.Name,
    Image:           checkpointImage,
    ImagePullPolicy: pullPolicy,
    Ports:           c.Ports,
}
```

**Step 4: Update handleTransferring Job creation for Direct mode**

The Job for Direct mode should use the checkpoint-transfer binary in direct mode:

```go
if m.Spec.TransferMode == "Direct" {
    agentURL := fmt.Sprintf("http://ms2m-agent.ms2m-system.svc.cluster.local:9443/checkpoint")
    containerSpec.Args = []string{m.Status.CheckpointID, agentURL, m.Status.ContainerName}
} else {
    imageRef := fmt.Sprintf("%s/%s:checkpoint", m.Spec.CheckpointImageRepository, m.Spec.SourcePod)
    containerSpec.Args = []string{m.Status.CheckpointID, imageRef, m.Status.ContainerName}
}
```

**Step 5: Run tests**

Run: `make test`
Expected: All tests pass.

**Step 6: Commit**

```bash
git add internal/controller/statefulmigration_controller.go \
  internal/controller/statefulmigration_controller_test.go
git commit -m "feat: wire Direct transfer mode through controller Transferring and Restoring"
```

---

## Task 8: Update Evaluation Scripts

**Files:**
- Modify: `eval/scripts/run_evaluation.sh`
- Create: `eval/scripts/run_optimized_evaluation.sh`

Create evaluation scripts that test all four configurations from the design doc.

**Step 1: Create optimized evaluation script**

This script runs migration tests for the Deployment + ShadowPod + Direct configuration.
Details depend on the existing `run_evaluation.sh` structure — adapt accordingly.

Key differences from baseline evaluation:
- Deploy consumer as Deployment (not StatefulSet)
- Set `transferMode: Direct` in StatefulMigration CR
- Deploy ms2m-agent DaemonSet before testing
- Collect same timing metrics for comparison

**Step 2: Commit**

```bash
git add eval/scripts/run_optimized_evaluation.sh
git commit -m "eval: add optimized evaluation script for Deployment + Direct transfer"
```

---

## Task 9: Background Pre-dumps (Optional / Higher Risk)

**Files:**
- Modify: `cmd/ms2m-agent/main.go` (add pre-dump handler)
- Create: `cmd/ms2m-agent/predump.go` (pre-dump logic)
- Modify: `api/v1alpha1/types.go` (add PreDumpConfig)
- Test: `cmd/ms2m-agent/predump_test.go`

This task is optional and higher risk because it requires:
- `hostPID: true` to access container PIDs
- Direct CRIU binary invocation (not through kubelet API)
- CRI-O metadata parsing to map pod -> container -> PID

**Implementation depends on cluster testing.** The approach:

1. Agent watches for pods with `migration.ms2m.io/predump-enabled: "true"` annotation
2. Uses `crictl inspect` to get the container's PID from CRI-O
3. Runs `criu pre-dump --tree <pid> --images-dir <dir> --track-mem --shell-job`
4. Stores pre-dump chain locally
5. On migration trigger: controller signals agent for final dump with `--prev-images-dir`

**Risk mitigation:** If CRIU pre-dump doesn't work from the DaemonSet context
(PID namespace, permission, or capability issues), skip this task entirely.
The first two optimizations already deliver ~15s total migration time.

**Step 1: Prototype on cluster**

SSH into a worker node and manually test:
```bash
# Get container PID from CRI-O
PID=$(crictl inspect <container-id> | jq '.info.pid')

# Run pre-dump
mkdir -p /var/lib/ms2m/predumps/test
criu pre-dump --tree $PID --images-dir /var/lib/ms2m/predumps/test --track-mem --shell-job

# Verify pre-dump worked
ls -la /var/lib/ms2m/predumps/test/
```

If this works, proceed with implementation. If not, document the failure and skip.

**Step 2-8: Implementation follows same TDD pattern as Tasks 5-6**

---

## Summary

| Task | Description | Impact | Risk |
|------|-------------|--------|------|
| 1 | Consumer Deployment variant | Enables ShadowPod without StatefulSet overhead | Low |
| 2 | Deployment detection in handlePending | Records Deployment name for Finalizing | Low |
| 3 | Deployment patching in handleFinalizing | Ensures replacement lands on target node | Low |
| 4 | TransferMode CRD field | Branching point for Registry vs Direct | Low |
| 5 | Refactor checkpoint-transfer + direct mode | Shared OCI package, HTTP POST transfer | Medium |
| 6 | ms2m-agent DaemonSet | Receives checkpoint, loads into CRI-O | Medium |
| 7 | Wire Direct mode end-to-end | Controller creates correct Jobs and Pods | Medium |
| 8 | Evaluation scripts | Test all configurations | Low |
| 9 | Background pre-dumps (optional) | Reduce checkpoint to delta-only | High |

**Estimated sequence:** Tasks 1-3 can be done first (biggest impact, lowest risk).
Tasks 4-7 form a unit (direct transfer pipeline). Task 8 ties everything together.
Task 9 is optional and should only be attempted after Tasks 1-8 are validated on cluster.
