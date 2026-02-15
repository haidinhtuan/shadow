# Lessons Learned: MS2M Controller Development

This document captures key insights discovered during the implementation and end-to-end testing of the MS2M (Message-based Stateful Microservice Migration) Kubernetes controller. Each section describes the problem encountered, why it was surprising or non-obvious, and the solution applied.

---

## Table of Contents

1. [CRIU Checkpoint and Restore on Kubernetes](#1-criu-checkpoint-and-restore-on-kubernetes)
2. [Volume Mount Pitfall: kube-api-access Projected Volumes](#2-volume-mount-pitfall-kube-api-access-projected-volumes)
3. [StatefulSet Identity Conflict](#3-statefulset-identity-conflict)
4. [StatefulSet Controller Race Condition](#4-statefulset-controller-race-condition)
5. [Bare-Metal CRI-O Image Deployment](#5-bare-metal-cri-o-image-deployment)
6. [Controller Pattern: Phase-Based State Machine](#6-controller-pattern-phase-based-state-machine)
7. [Message Queue Replay Strategy](#7-message-queue-replay-strategy)
8. [Testing with envtest](#8-testing-with-envtest)

---

## 1. CRIU Checkpoint and Restore on Kubernetes

### Background

Kubernetes 1.30+ supports forensic container checkpointing via the kubelet API when the `ContainerCheckpoint` feature gate is enabled. The kubelet exposes a POST endpoint that triggers CRIU (Checkpoint/Restore In Userspace) to freeze and serialize a running container's full state.

### How it works

The checkpoint API is accessed through the Kubernetes API server proxy:

```
POST /api/v1/nodes/{node}/proxy/checkpoint/{namespace}/{pod}/{container}
```

In Go, this translates to:

```go
// From internal/kubelet/client.go
result := c.restClient.Post().
    Resource("nodes").
    Name(nodeName).
    SubResource("proxy", "checkpoint", namespace, podName, containerName).
    Do(ctx)
```

The kubelet writes the checkpoint archive (a tarball containing the CRIU image files) to a well-known path on the node filesystem:

```
/var/lib/kubelet/checkpoints/checkpoint-<pod>_<namespace>-<container>-<timestamp>.tar
```

### Transferring the checkpoint

The raw checkpoint tarball is not directly usable as a container image. It must be packaged as a single-layer OCI image and pushed to a container registry. The `checkpoint-transfer` tool handles this using `go-containerregistry`:

```go
// From cmd/checkpoint-transfer/main.go
layer, err := tarball.LayerFromFile(checkpointPath,
    tarball.WithCompressionLevel(gzip.NoCompression))

img, err := mutate.AppendLayers(empty.Image, layer)

img = mutate.Annotations(img, map[string]string{
    "io.kubernetes.cri-o.annotations.checkpoint.name": containerName,
}).(v1.Image)

crane.Push(img, imageRef, opts...)
```

The annotation `io.kubernetes.cri-o.annotations.checkpoint.name` is required for CRI-O to recognize the image as a checkpoint image and restore from it correctly.

### Key insight: What is baked into the checkpoint

When CRIU creates a checkpoint, **everything** about the running process is captured:

- Environment variables
- Process state (memory, registers, instruction pointer)
- File descriptors (open files, sockets)
- Signal handlers

This means that when restoring from a CRIU checkpoint image, you do **not** need to re-specify environment variables, command, or args in the pod spec. They are already embedded in the checkpoint image. Specifying them again can cause conflicts or unexpected behavior.

```go
// From internal/controller/statefulmigration_controller.go -- handleRestoring
// Only copy name, image, and ports from source containers.
// Env vars and other fields are baked into the CRIU checkpoint.
restored := corev1.Container{
    Name:  c.Name,
    Image: checkpointImage,
    Ports: c.Ports,
}
```

---

## 2. Volume Mount Pitfall: kube-api-access Projected Volumes

### The problem

Kubernetes automatically injects a `kube-api-access-*` projected volume into every pod. This volume provides the service account token, CA certificate, and namespace file that pods use to authenticate with the API server. The volume mount name includes a random suffix that is unique per pod:

```
kube-api-access-fpt2k
kube-api-access-9xm4n
```

When copying container specs from a source pod to build the target pod, naively copying `volumeMounts` causes the target pod to reference a volume name that does not exist on the new pod.

### The error

```
Pod "consumer-0" is invalid: spec.containers[0].volumeMounts[0].name:
    Not found: "kube-api-access-fpt2k"
```

### The solution

Only copy the fields you actually need from the source container: `Name`, `Image`, and `Ports`. Let Kubernetes handle automatic volume injection on the new pod.

```go
// WRONG: copying the full container spec
containers = sourcePod.Spec.Containers  // includes volumeMounts!

// RIGHT: selectively copy only what we need
for _, c := range sourceContainers {
    restored := corev1.Container{
        Name:  c.Name,
        Image: checkpointImage,
        Ports: c.Ports,
        // Do NOT copy: VolumeMounts, Env, Command, Args
        // VolumeMounts: auto-injected names won't match
        // Env/Command/Args: baked into the CRIU checkpoint
    }
    containers = append(containers, restored)
}
```

### Why ports are the exception

Ports are purely declarative metadata in Kubernetes -- they do not affect the container's actual listening behavior (which is restored from the checkpoint). However, Kubernetes Services use port definitions for endpoint matching, so they must be present for service discovery to work after migration.

---

## 3. StatefulSet Identity Conflict

### The problem

Kubernetes enforces a strict invariant: no two pods managed by the same StatefulSet can share an ordinal identity simultaneously. A StatefulSet pod named `consumer-0` occupies the ordinal slot `0`. You cannot create another pod named `consumer-0` while the first one exists.

This fundamentally conflicts with the "ShadowPod" migration strategy, where a shadow pod runs alongside the source during the replay phase. The ShadowPod strategy works for standalone pods (by naming the target `consumer-0-shadow`), but for StatefulSet-owned pods, the target must have the same name (`consumer-0`) to maintain identity continuity.

### Why deleting the pod is not enough

A naive approach is to delete the source pod, then create the target pod with the same name. But this creates a race condition (see next section): the StatefulSet controller watches for pod deletions and immediately recreates the missing pod on the original node, defeating the purpose of migration.

### The solution: Sequential migration strategy

The controller auto-detects whether a source pod is owned by a StatefulSet by inspecting `ownerReferences`:

```go
// From internal/controller/statefulmigration_controller.go -- handlePending
for _, ref := range sourcePod.OwnerReferences {
    if ref.Kind == "StatefulSet" {
        strategy = "Sequential"
        break
    }
}
```

The Sequential strategy scales the StatefulSet to 0 replicas before creating the target pod, ensuring no identity conflict occurs.

---

## 4. StatefulSet Controller Race Condition

### The problem

If you delete a StatefulSet-managed pod while the StatefulSet still has `replicas > 0`, the StatefulSet controller notices the pod count is below the desired count and immediately recreates the pod -- often within milliseconds. This creates a tight reconciliation loop:

1. Migration controller deletes `consumer-0`
2. StatefulSet controller recreates `consumer-0` on the original node
3. Migration controller sees `consumer-0` still exists, deletes again
4. Repeat indefinitely

This was observed during E2E testing as one of the baseline issues.

### The solution: Scale first, then wait

The correct approach is to scale the StatefulSet to 0 replicas **first**, then let the StatefulSet controller handle the pod deletion as part of its scale-down logic. The migration controller must not delete the pod itself.

```go
// From internal/controller/statefulmigration_controller.go -- handleRestoring
if m.Status.OriginalReplicas == 0 && m.Status.StatefulSetName != "" {
    sts := &appsv1.StatefulSet{}
    stsErr := r.Get(ctx, types.NamespacedName{
        Name: m.Status.StatefulSetName, Namespace: m.Namespace,
    }, sts)

    if stsErr == nil && sts.Spec.Replicas != nil && *sts.Spec.Replicas > 0 {
        // Save original replica count for later restoration
        patch := client.MergeFrom(m.DeepCopy())
        m.Status.OriginalReplicas = *sts.Spec.Replicas
        _ = r.Status().Patch(ctx, m, patch)

        // Scale to zero
        newReplicas := int32(0)
        stsPatch := client.MergeFrom(sts.DeepCopy())
        sts.Spec.Replicas = &newReplicas
        if err := r.Patch(ctx, sts, stsPatch); err != nil {
            return r.failMigration(ctx, m, fmt.Sprintf(
                "scale down StatefulSet %q: %v", m.Status.StatefulSetName, err))
        }
    }
}

// Wait for the StatefulSet controller to process scale-down.
// Do NOT delete the pod ourselves to avoid the race.
return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
```

After migration completes, the finalizing phase restores the StatefulSet to its original replica count:

```go
// From handleFinalizing
if m.Spec.MigrationStrategy == "Sequential" && m.Status.StatefulSetName != "" {
    sts := &appsv1.StatefulSet{}
    if err := r.Get(ctx, ...); err == nil {
        stsPatch := client.MergeFrom(sts.DeepCopy())
        sts.Spec.Replicas = &m.Status.OriginalReplicas
        r.Patch(ctx, sts, stsPatch)
    }
}
```

### Design principle

Never fight another controller. If a higher-level controller (StatefulSet, Deployment) owns a resource, work with its API (scale, update) rather than directly manipulating the resources it manages.

---

## 5. Bare-Metal CRI-O Image Deployment

### The problem

On bare-metal Kubernetes clusters using CRI-O as the container runtime (not Docker), the standard `docker load` command does not work. CRI-O has its own container storage backend and does not share Docker's image store.

### The pipeline

To deploy locally-built images to a CRI-O cluster without a remote registry, the following pipeline is used:

```bash
# 1. Build the image locally with Docker
docker build -t ms2m-controller:latest -f Dockerfile .

# 2. Export the image as a tar archive
docker save ms2m-controller:latest -o /tmp/ms2m-controller.tar

# 3. Copy the tar to the cluster node
scp /tmp/ms2m-controller.tar root@node:/tmp/

# 4. Import into CRI-O's container storage using skopeo
skopeo copy \
    docker-archive:/tmp/ms2m-controller.tar \
    containers-storage:docker.io/ms2m-controller:latest
```

The key tool is `skopeo`, which can convert between different image transport formats. The `containers-storage:` prefix tells skopeo to write the image into the local CRI-O/containers storage backend.

### Alternative: In-cluster registry

For the evaluation infrastructure, we deploy a local container registry inside the cluster and use skopeo to push images to it:

```bash
# Push to in-cluster registry via skopeo
skopeo copy --dest-tls-verify=false \
    docker-archive:/tmp/ms2m-controller.tar \
    docker://${REGISTRY_IP}:5000/ms2m-controller:latest
```

This approach requires CRI-O to be configured with the registry as an insecure registry:

```ini
# /etc/crio/crio.conf.d/10-insecure-registry.conf
[crio.image]
insecure_registries = ["registry.registry.svc.cluster.local:5000"]
```

### Critical: All nodes must have the image

When using `containers-storage:` (direct node import without a registry), the image must be imported on **every node** that might need to pull it. The scheduler can place pods on any eligible node, and CRI-O will fail with `ImagePullBackOff` if the image is not in local storage. Using an in-cluster registry avoids this problem.

---

## 6. Controller Pattern: Phase-Based State Machine

### The pattern

The reconcile loop implements a state machine where the `StatefulMigration` resource's `status.phase` field determines which handler runs:

```
Pending -> Checkpointing -> Transferring -> Restoring -> Replaying -> Finalizing -> Completed
```

Each phase handler returns one of:
- `ctrl.Result{Requeue: true}` -- phase completed synchronously, process the next phase immediately
- `ctrl.Result{RequeueAfter: duration}` -- waiting for an external condition (job completion, pod readiness), check back later
- `ctrl.Result{}` -- terminal state, no more work needed
- An error -- transient failure, the controller-runtime will requeue with backoff

### Phase chaining

When a handler completes synchronously (`Requeue: true` with no delay), the reconcile loop re-fetches the resource and immediately processes the next phase in the same reconcile call. This avoids unnecessary round-trips through the work queue:

```go
// From Reconcile()
for {
    switch migration.Status.Phase {
    case migrationv1alpha1.PhasePending:
        result, err = r.handlePending(ctx, migration)
    case migrationv1alpha1.PhaseCheckpointing:
        result, err = r.handleCheckpointing(ctx, migration)
    // ...
    }

    // If error or delayed requeue, return to work queue
    if err != nil || result.RequeueAfter > 0 {
        return result, err
    }
    // If no requeue, terminal state
    if !result.Requeue {
        return result, nil
    }
    // Synchronous completion: re-fetch and continue loop
    if fetchErr := r.Get(ctx, req.NamespacedName, migration); fetchErr != nil {
        return ctrl.Result{}, client.IgnoreNotFound(fetchErr)
    }
}
```

In practice, `Pending -> Checkpointing` chains synchronously in one reconcile call, while `Transferring` breaks the chain because it must wait for a Job to complete.

### Use MergeFrom patches for status updates

Always use `client.MergeFrom` patches instead of full status updates to avoid conflicts with concurrent writes. This is especially important when the reconciler is running alongside other controllers that may modify the same resource:

```go
base := m.DeepCopy()              // snapshot BEFORE modifications
m.Status.Phase = newPhase         // modify the in-memory copy
m.Status.PhaseTimings["..."] = .. // add timing data
r.Status().Patch(ctx, m, client.MergeFrom(base))  // send only the diff
```

### Capture source pod data early

During the Pending phase, the controller captures all information it will need from the source pod (labels, containers, ownerReferences, node name) and stores it in the migration status:

```go
// From handlePending
m.Status.SourcePodLabels = sourcePod.Labels
m.Status.SourceContainers = sourcePod.Spec.Containers

for _, ref := range sourcePod.OwnerReferences {
    if ref.Kind == "StatefulSet" {
        m.Status.StatefulSetName = ref.Name
        break
    }
}
```

This is critical for the Sequential strategy, where the source pod is deleted before the target pod is created. Without this early capture, the restore phase would have no way to recover the source pod's labels, container ports, or owning StatefulSet name.

---

## 7. Message Queue Replay Strategy

### The challenge

During migration, messages continue arriving at the original queue. If the source pod is stopped and the target pod starts consuming, messages published between the checkpoint and the restore are lost.

### The solution: Fan-out duplication with a shadow queue

The controller sets up a fan-out exchange that duplicates every message to both the primary queue and a secondary "replay" queue:

```go
// From internal/messaging/rabbitmq.go -- CreateSecondaryQueue

// Declare a fanout exchange
r.ch.ExchangeDeclare(exchangeName, "fanout", true, false, false, false, nil)

// Declare the secondary replay queue
secondaryQueue := primaryQueue + ".ms2m-replay"
r.ch.QueueDeclare(secondaryQueue, true, false, false, false, nil)

// Bind BOTH queues to the fanout exchange
r.ch.QueueBind(primaryQueue, "", exchangeName, false, nil)
r.ch.QueueBind(secondaryQueue, "", exchangeName, false, nil)
```

The fan-out is established during the Checkpointing phase, before the CRIU checkpoint is taken. This ensures that every message arriving after the checkpoint timestamp has a copy in the replay queue.

### Replay lifecycle

1. **Checkpointing phase**: Controller creates the secondary queue with fan-out binding. New messages now go to both queues.

2. **Restoring phase**: Target pod starts from the checkpoint image. It initially has no knowledge of the replay queue.

3. **Replaying phase**: Controller sends a `START_REPLAY` control message to the target pod via a dedicated control queue (`ms2m.control.<pod-name>`). The target pod switches from consuming the primary queue to consuming the replay queue.

4. **Monitoring**: The controller polls the replay queue depth. When the queue drains to 0, or the `replayCutoffSeconds` threshold is exceeded, the migration proceeds to finalization.

5. **Finalizing phase**: Controller sends `END_REPLAY`. The target pod switches back to the primary queue. The secondary queue is unbound and deleted.

```go
// Tear down: unbind secondary, delete it, leave primary intact
r.ch.QueueUnbind(secondaryQueue, "", exchangeName, nil)
r.ch.QueueDelete(secondaryQueue, false, false, false)
```

### Consumer-side implementation

The consumer application must implement a control message listener that dynamically switches its consumption source:

```python
# From eval/workloads/consumer.yaml (simplified)
def check_control_messages(ch, ctrl_queue):
    while True:
        method, props, body = ch.basic_get(queue=ctrl_queue, auto_ack=True)
        if method is None:
            break
        msg = json.loads(body)
        if msg['type'] == 'START_REPLAY':
            replay_state['active'] = True
            replay_state['queue'] = msg['payload']['queue']
        elif msg['type'] == 'END_REPLAY':
            replay_state['active'] = False
            replay_state['queue'] = None

# In the main loop, periodically check for control messages
# and switch consumer if replay state changes
consume_queue = replay_state['queue'] if replay_state['active'] else primary_queue
```

---

## 8. Testing with envtest

### What envtest provides

The `controller-runtime/pkg/envtest` package spins up a real Kubernetes API server (and etcd) in-process. It provides:

- A real API server with full validation, admission webhooks, and storage
- Support for custom CRDs loaded from YAML files
- A `rest.Config` for creating real clients

### What envtest does NOT provide

Critically, envtest does **not** run any built-in controllers:

- No StatefulSet controller -- scaling a StatefulSet to 0 will not delete pods
- No Deployment controller -- creating a Deployment will not create ReplicaSets or Pods
- No scheduler -- pods stay in `Pending` forever (no `status.phase` updates)
- No kubelet -- containers are never actually started

This means:

- Pods must have their `Status.Phase` set manually in tests to simulate running pods
- StatefulSet scale-down must be simulated by manually deleting the pod after the test scales down the StatefulSet
- Job completion must be simulated by manually setting `Status.Succeeded = 1`

### Test architecture

The project uses two testing approaches:

**Unit tests with fake client** -- For most reconciler logic. The `fake.NewClientBuilder()` creates a fast, in-memory client with full control over state:

```go
// From internal/controller/statefulmigration_controller_test.go
func setupTest(objs ...client.Object) (*StatefulMigrationReconciler, ...) {
    scheme := testScheme()
    cb := fake.NewClientBuilder().
        WithScheme(scheme).
        WithStatusSubresource(&migrationv1alpha1.StatefulMigration{})
    if len(objs) > 0 {
        cb = cb.WithObjects(objs...)
    }
    fakeClient := cb.Build()

    mockBroker := messaging.NewMockBrokerClient()

    reconciler := &StatefulMigrationReconciler{
        Client:    fakeClient,
        Scheme:    scheme,
        MsgClient: mockBroker,
    }
    return reconciler, mockBroker, context.Background()
}
```

**Integration tests with envtest** -- For validating real API interactions (CRD schema validation, status subresource behavior, RBAC). These tests are skipped if envtest binaries are not installed:

```go
// From internal/controller/suite_test.go
func TestMain(m *testing.M) {
    testEnv = &envtest.Environment{
        CRDDirectoryPaths: []string{
            filepath.Join("..", "..", "config", "crd", "bases"),
        },
    }
    cfg, err = testEnv.Start()
    if err != nil {
        // envtest binaries might not be installed; unit tests still run
        cfg = nil
        testEnv = nil
    }
    // ...
}

// Integration tests guard against missing envtest
func TestIntegration_CreateMigration_SetsPending(t *testing.T) {
    if cfg == nil {
        t.Skip("envtest not available, skipping integration test")
    }
    // ...
}
```

### Simulating StatefulSet controller behavior in tests

Since envtest does not run the StatefulSet controller, the test for Sequential migration manually simulates the controller's scale-down behavior:

```go
// From TestReconcile_Restoring_Sequential_ScalesDownStatefulSet

// First reconcile: scales down StatefulSet and waits
result, err := reconcileOnce(r, ctx, "mig-seq-restore", "default")

// Verify StatefulSet was scaled to 0
updatedSts := &appsv1.StatefulSet{}
r.Get(ctx, ..., updatedSts)
// *updatedSts.Spec.Replicas == 0

// Second reconcile: pod still exists, controller waits
result, err = reconcileOnce(r, ctx, "mig-seq-restore", "default")

// Simulate StatefulSet controller deleting the pod
pod := &corev1.Pod{}
r.Get(ctx, types.NamespacedName{Name: "myapp-0", ...}, pod)
r.Delete(ctx, pod)

// Third reconcile: pod is gone, creates target pod
result, err = reconcileOnce(r, ctx, "mig-seq-restore", "default")
```

### Mock broker for message queue testing

The `MockBrokerClient` provides full control over message queue behavior without a real RabbitMQ instance:

```go
// Set up queue with messages to simulate replay lag
mockBroker.SetQueueDepth("orders.ms2m-replay", 100)

// Verify control messages were sent
for _, msg := range mockBroker.ControlMessages {
    if msg.Type == messaging.ControlStartReplay { ... }
    if msg.Type == messaging.ControlEndReplay { ... }
}

// Inject errors to test failure handling
mockBroker.ConnectErr = fmt.Errorf("connection refused")
```

---

## Summary of Recurring Themes

1. **Never fight other controllers.** The StatefulSet controller, ReplicaSet controller, and scheduler are all running in a production cluster. Work with their APIs (scale, cordon) rather than deleting resources they manage.

2. **Kubernetes auto-injects more than you think.** Service account tokens, projected volumes, environment variables from ConfigMaps/Secrets -- all of these are added to pods automatically and have unique-per-pod identifiers. Be selective about what you copy from a source pod.

3. **CRIU captures everything.** Environment variables, open files, process memory -- it is all in the checkpoint. The restore pod spec should be minimal: just the checkpoint image, the container name, and ports for service discovery.

4. **Test what the real cluster will do, not what envtest does.** envtest is useful for API validation but it does not run controllers or schedulers. Always supplement envtest tests with E2E tests on a real cluster.

5. **Container runtimes matter.** CRI-O and containerd have different image storage backends, different checkpoint support, and different tooling. What works with Docker locally may not work on a CRI-O cluster in production.
