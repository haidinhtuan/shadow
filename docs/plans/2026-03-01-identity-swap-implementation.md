# Local Identity Swap Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** After ShadowPod+StatefulSet migration, swap the orphaned shadow pod for a correctly-named replacement that the StatefulSet can adopt — restoring full lifecycle management with zero traffic interruption.

**Architecture:** When `handleFinalizing` detects ShadowPod+StatefulSet, it enters a sub-phase state machine (tracked via `SwapSubPhase` status field). Each sub-phase is idempotent: PrepareSwap creates a secondary queue for buffering, ReCheckpoint calls the kubelet API on the local node, CreateReplacement builds a pod with the correct StatefulSet name, MiniReplay drains buffered messages, and TrafficSwitch deletes the shadow pod and scales up the StatefulSet for adoption.

**Tech Stack:** Go 1.25+, controller-runtime, kubelet checkpoint API, RabbitMQ (AMQP 0-9-1)

**Scope limitation:** Single-replica StatefulSets only (evaluated and validated across 140 runs). Multi-replica StatefulSets require a different approach because StatefulSet scale-down always removes the highest-ordinal pod — you cannot selectively remove a specific ordinal (e.g., ordinal 0 in a 3-replica set) without scaling to 0. A future enhancement could use `StatefulSet.spec.ordinals` (K8s 1.26+) or partition-based rolling updates.

---

### Task 1: Add CRD Status Fields

**Files:**
- Modify: `api/v1alpha1/types.go:67-107`
- Modify: `api/v1alpha1/deepcopy.go:41-75`
- Modify: `config/crd/bases/migration.ms2m.io_statefulmigrations.yaml:89-191`

**Step 1: Add fields to StatefulMigrationStatus**

In `api/v1alpha1/types.go`, add two fields after `SourceContainers` (line 106):

```go
// SwapSubPhase tracks progress of the local identity swap for ShadowPod+StatefulSet.
// Empty when not performing a swap.
SwapSubPhase string `json:"swapSubPhase,omitempty"`

// ReplacementPod is the name of the correctly-named replacement pod created during identity swap.
ReplacementPod string `json:"replacementPod,omitempty"`
```

**Step 2: Update DeepCopyInto for StatefulMigrationStatus**

No code change needed — `SwapSubPhase` and `ReplacementPod` are plain strings, which are copied by value in the `*out = *in` assignment on line 42 of `deepcopy.go`.

**Step 3: Update the CRD YAML**

In `config/crd/bases/migration.ms2m.io_statefulmigrations.yaml`, add inside `status.properties` (after `targetPod`):

```yaml
              swapSubPhase:
                description: SwapSubPhase tracks progress of the local identity swap
                  for ShadowPod+StatefulSet migrations. Empty when not performing a swap.
                type: string
              replacementPod:
                description: ReplacementPod is the name of the correctly-named replacement
                  pod created during identity swap.
                type: string
```

**Step 4: Run tests to verify nothing breaks**

Run: `go test ./... -count=1`
Expected: All existing tests PASS (new fields are optional/zero-value)

**Step 5: Commit**

```bash
git add api/v1alpha1/types.go config/crd/bases/migration.ms2m.io_statefulmigrations.yaml
git commit -m "Add SwapSubPhase and ReplacementPod status fields for identity swap"
```

---

### Task 2: Write Tests for PrepareSwap Sub-Phase

**Files:**
- Modify: `internal/controller/statefulmigration_controller_test.go`

**Step 1: Write the test for PrepareSwap entry**

Add a new test that sets up a ShadowPod+StatefulSet migration in Finalizing phase with no `SwapSubPhase` set. Verify that after reconciliation, `SwapSubPhase` is set to `"PrepareSwap"` and a secondary swap queue is created (the queue name should be `<queueName>.ms2m-swap`). The migration should NOT complete — it should requeue.

```go
func TestReconcile_Finalizing_ShadowPod_StatefulSet_EntersSwap(t *testing.T) {
	stsReplicas := int32(1)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "consumer",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &stsReplicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "consumer"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "consumer"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "consumer:latest"}}},
			},
		},
	}

	shadowPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "consumer-0-shadow",
			Namespace: "default",
			Labels:    map[string]string{"app": "consumer", "migration.ms2m.io/migration": "mig-swap", "migration.ms2m.io/role": "target"},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-2",
			Containers: []corev1.Container{{Name: "app", Image: "consumer:latest"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "consumer-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "app", Image: "consumer:latest"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	migration := newMigration("mig-swap", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.SourcePod = "consumer-0"
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "consumer-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.StatefulSetName = "consumer"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration, sts, sourcePod, shadowPod)
	mockBroker.Connected = true

	result, err := reconcileOnce(r, ctx, "mig-swap", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-swap", "default")

	// Should NOT complete immediately — should enter swap sub-phases
	if got.Status.Phase == migrationv1alpha1.PhaseCompleted {
		t.Error("expected migration to NOT complete immediately; should enter swap sub-phases")
	}

	// Should have set SwapSubPhase
	if got.Status.SwapSubPhase == "" {
		t.Error("expected SwapSubPhase to be set after first Finalizing reconcile for ShadowPod+StatefulSet")
	}

	// Should requeue
	if !result.Requeue && result.RequeueAfter == 0 {
		t.Error("expected requeue for swap sub-phase processing")
	}

	// Verify swap secondary queue was created
	if _, exists := mockBroker.Queues["orders.ms2m-swap"]; !exists {
		t.Error("expected swap secondary queue 'orders.ms2m-swap' to be created")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestReconcile_Finalizing_ShadowPod_StatefulSet_EntersSwap -v`
Expected: FAIL — current code completes immediately without entering swap

**Step 3: Commit the failing test**

```bash
git add internal/controller/statefulmigration_controller_test.go
git commit -m "Add failing test for ShadowPod+StatefulSet identity swap entry"
```

---

### Task 3: Implement PrepareSwap Sub-Phase Entry

**Files:**
- Modify: `internal/controller/statefulmigration_controller.go:662-693`

**Step 1: Implement the swap entry in handleFinalizing**

Replace the ShadowPod+StatefulSet block (lines 662-681) in `handleFinalizing`. When `SwapSubPhase` is empty and it's ShadowPod+StatefulSet, enter the swap. When `SwapSubPhase` is non-empty, dispatch to the swap handler.

Replace the current block:
```go
if m.Spec.MigrationStrategy == "ShadowPod" {
	if m.Status.StatefulSetName != "" {
		// ... scale down StatefulSet ...
	} else {
		// ... delete source pod ...
	}
}
```

With:
```go
if m.Spec.MigrationStrategy == "ShadowPod" {
	if m.Status.StatefulSetName != "" {
		// ShadowPod + StatefulSet: perform identity swap to restore StatefulSet ownership
		result, done, err := r.handleIdentitySwap(ctx, m, base)
		if err != nil {
			return result, err
		}
		if !done {
			return result, nil
		}
		// Swap complete — fall through to normal completion
	} else {
		sourcePod := &corev1.Pod{}
		err := r.Get(ctx, types.NamespacedName{Name: m.Spec.SourcePod, Namespace: m.Namespace}, sourcePod)
		if err == nil {
			if delErr := r.Delete(ctx, sourcePod); delErr != nil {
				logger.Error(delErr, "Failed to delete source pod", "pod", m.Spec.SourcePod)
			}
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
}
```

**Step 2: Create the handleIdentitySwap method skeleton**

Add a new method that dispatches on `SwapSubPhase`:

```go
// handleIdentitySwap manages the local identity swap for ShadowPod+StatefulSet
// migrations. It returns (result, done, error) where done=true means the swap
// is complete and the caller should proceed to normal Finalizing completion.
func (r *StatefulMigrationReconciler) handleIdentitySwap(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	switch m.Status.SwapSubPhase {
	case "", "PrepareSwap":
		return r.handleSwapPrepare(ctx, m, base)
	case "ReCheckpoint":
		return r.handleSwapReCheckpoint(ctx, m, base)
	case "CreateReplacement":
		return r.handleSwapCreateReplacement(ctx, m, base)
	case "MiniReplay":
		return r.handleSwapMiniReplay(ctx, m, base)
	case "TrafficSwitch":
		return r.handleSwapTrafficSwitch(ctx, m, base)
	default:
		logger.Error(nil, "Unknown swap sub-phase", "subPhase", m.Status.SwapSubPhase)
		return ctrl.Result{}, true, nil
	}
}
```

**Step 3: Implement handleSwapPrepare**

```go
// handleSwapPrepare creates a secondary queue to buffer messages during the swap.
func (r *StatefulMigrationReconciler) handleSwapPrepare(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	mqCfg := m.Spec.MessageQueueConfig

	// Reconnect to broker if needed (Finalizing may have closed it in a previous attempt)
	if err := r.MsgClient.Connect(ctx, mqCfg.BrokerURL); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("broker connect for swap: %w", err)
	}

	// Create a swap-specific secondary queue for buffering during the identity swap
	swapQueue := mqCfg.QueueName + ".ms2m-swap"
	if _, err := r.MsgClient.CreateSecondaryQueue(ctx, mqCfg.QueueName, mqCfg.ExchangeName, mqCfg.RoutingKey); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("create swap queue: %w", err)
	}
	_ = swapQueue // queue name is derived from convention

	logger.Info("PrepareSwap complete, swap queue created")

	// Transition to ReCheckpoint
	patch := client.MergeFrom(m.DeepCopy())
	m.Status.SwapSubPhase = "ReCheckpoint"
	if err := r.Status().Patch(ctx, m, patch); err != nil {
		return ctrl.Result{}, false, err
	}

	return ctrl.Result{Requeue: true}, false, nil
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/controller/ -run TestReconcile_Finalizing_ShadowPod_StatefulSet_EntersSwap -v`
Expected: PASS

**Step 5: Verify existing tests still pass**

Run: `go test ./internal/controller/ -count=1 -v`
Expected: All PASS. Existing ShadowPod+StatefulSet Finalizing tests (`TestReconcile_Finalizing_ShadowPod_ScalesDownStatefulSet`, `TestReconcile_Finalizing_ShadowPod_MultiReplicaStatefulSet`) will now enter the swap flow instead of completing immediately — these tests need updating in a later task.

**Step 6: Commit**

```bash
git add internal/controller/statefulmigration_controller.go
git commit -m "Implement identity swap entry and PrepareSwap sub-phase"
```

---

### Task 4: Implement ReCheckpoint Sub-Phase

**Files:**
- Modify: `internal/controller/statefulmigration_controller.go`
- Modify: `internal/controller/statefulmigration_controller_test.go`

**Step 1: Write the failing test**

```go
func TestReconcile_Finalizing_Swap_ReCheckpoint(t *testing.T) {
	migration := newMigration("mig-swap-ckpt", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.SourcePod = "consumer-0"
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Spec.TargetNode = "node-2"
	migration.Status.TargetPod = "consumer-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.StatefulSetName = "consumer"
	migration.Status.ContainerName = "app"
	migration.Status.SwapSubPhase = "ReCheckpoint"
	migration.Status.PhaseTimings = map[string]string{}

	shadowPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "consumer-0-shadow",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-2",
			Containers: []corev1.Container{{Name: "app", Image: "consumer:latest"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r, mockBroker, ctx := setupTest(migration, shadowPod)
	mockBroker.Connected = true

	_, err := reconcileOnce(r, ctx, "mig-swap-ckpt", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-swap-ckpt", "default")

	// Should advance to CreateReplacement
	if got.Status.SwapSubPhase != "CreateReplacement" {
		t.Errorf("expected SwapSubPhase %q, got %q", "CreateReplacement", got.Status.SwapSubPhase)
	}

	// CheckpointID should be updated (re-checkpoint of shadow pod)
	if got.Status.CheckpointID == "" {
		t.Error("expected CheckpointID to be set after re-checkpoint")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestReconcile_Finalizing_Swap_ReCheckpoint -v`
Expected: FAIL

**Step 3: Implement handleSwapReCheckpoint**

```go
// handleSwapReCheckpoint triggers a CRIU checkpoint on the shadow pod (same node, no network).
func (r *StatefulMigrationReconciler) handleSwapReCheckpoint(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	// The shadow pod is on the target node (where it was restored to during migration)
	targetNode := m.Spec.TargetNode

	if r.KubeletClient != nil {
		resp, err := r.KubeletClient.Checkpoint(
			ctx,
			targetNode,
			m.Namespace,
			m.Status.TargetPod, // shadow pod name
			m.Status.ContainerName,
		)
		if err != nil {
			return ctrl.Result{}, false, fmt.Errorf("re-checkpoint shadow pod: %w", err)
		}
		if len(resp.Items) > 0 {
			patch := client.MergeFrom(m.DeepCopy())
			m.Status.CheckpointID = resp.Items[0]
			m.Status.SwapSubPhase = "CreateReplacement"
			if err := r.Status().Patch(ctx, m, patch); err != nil {
				return ctrl.Result{}, false, err
			}
		}
	} else {
		// Test/dev fallback
		patch := client.MergeFrom(m.DeepCopy())
		m.Status.CheckpointID = fmt.Sprintf("/var/lib/kubelet/checkpoints/checkpoint-%s.tar", m.Status.TargetPod)
		m.Status.SwapSubPhase = "CreateReplacement"
		if err := r.Status().Patch(ctx, m, patch); err != nil {
			return ctrl.Result{}, false, err
		}
	}

	logger.Info("ReCheckpoint complete", "shadowPod", m.Status.TargetPod, "checkpointID", m.Status.CheckpointID)
	return ctrl.Result{Requeue: true}, false, nil
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/controller/ -run TestReconcile_Finalizing_Swap_ReCheckpoint -v`
Expected: PASS

**Step 5: Commit**

```bash
git add internal/controller/statefulmigration_controller.go internal/controller/statefulmigration_controller_test.go
git commit -m "Implement ReCheckpoint sub-phase for identity swap"
```

---

### Task 5: Implement CreateReplacement Sub-Phase

**Files:**
- Modify: `internal/controller/statefulmigration_controller.go`
- Modify: `internal/controller/statefulmigration_controller_test.go`

**Step 1: Write the failing test**

```go
func TestReconcile_Finalizing_Swap_CreateReplacement(t *testing.T) {
	stsReplicas := int32(0) // Already scaled down during initial migration
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "consumer",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &stsReplicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "consumer"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "consumer"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "consumer:latest", Ports: []corev1.ContainerPort{{ContainerPort: 8080}}}}},
			},
		},
	}

	migration := newMigration("mig-swap-create", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.SourcePod = "consumer-0"
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Spec.TargetNode = "node-2"
	migration.Spec.TransferMode = "Direct"
	migration.Status.TargetPod = "consumer-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.StatefulSetName = "consumer"
	migration.Status.ContainerName = "app"
	migration.Status.SwapSubPhase = "CreateReplacement"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-consumer-0-shadow.tar"
	migration.Status.PhaseTimings = map[string]string{}
	migration.Status.SourcePodLabels = map[string]string{"app": "consumer"}
	migration.Status.SourceContainers = []corev1.Container{{Name: "app", Image: "consumer:latest", Ports: []corev1.ContainerPort{{ContainerPort: 8080}}}}

	r, mockBroker, ctx := setupTest(migration, sts)
	mockBroker.Connected = true

	_, err := reconcileOnce(r, ctx, "mig-swap-create", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the replacement pod was created with the correct name
	replacementPod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: "consumer-0", Namespace: "default"}, replacementPod); err != nil {
		t.Fatalf("replacement pod 'consumer-0' not created: %v", err)
	}

	// Verify it has the app labels (for Service routing and StatefulSet adoption)
	if replacementPod.Labels["app"] != "consumer" {
		t.Errorf("expected label app=consumer, got %v", replacementPod.Labels["app"])
	}

	// Verify it does NOT have a controller ownerRef (so StatefulSet can adopt it)
	for _, ref := range replacementPod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			t.Errorf("replacement pod should not have controller ownerRef, got %v", ref)
		}
	}

	// Verify it's on the target node
	if replacementPod.Spec.NodeName != "node-2" {
		t.Errorf("expected pod on node-2, got %s", replacementPod.Spec.NodeName)
	}

	// Verify ports were copied from source containers
	if len(replacementPod.Spec.Containers) == 0 || len(replacementPod.Spec.Containers[0].Ports) == 0 {
		t.Error("expected container ports to be copied from source")
	}

	// Verify ReplacementPod status was set
	got := fetchMigration(r, ctx, "mig-swap-create", "default")
	if got.Status.ReplacementPod != "consumer-0" {
		t.Errorf("expected ReplacementPod 'consumer-0', got %q", got.Status.ReplacementPod)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestReconcile_Finalizing_Swap_CreateReplacement -v`
Expected: FAIL

**Step 3: Implement handleSwapCreateReplacement**

```go
// handleSwapCreateReplacement creates a pod with the correct StatefulSet name
// (e.g., consumer-0) from the re-checkpoint image. The pod has no controller
// ownerRef so the StatefulSet can adopt it later.
func (r *StatefulMigrationReconciler) handleSwapCreateReplacement(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	// The replacement pod gets the original StatefulSet pod name
	replacementName := m.Spec.SourcePod // e.g., "consumer-0"

	// Check if replacement already exists (idempotency)
	existing := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: replacementName, Namespace: m.Namespace}, existing)
	if err == nil {
		// Pod exists — check if it's running
		if existing.Status.Phase == corev1.PodRunning {
			patch := client.MergeFrom(m.DeepCopy())
			m.Status.ReplacementPod = replacementName
			m.Status.SwapSubPhase = "MiniReplay"
			if err := r.Status().Patch(ctx, m, patch); err != nil {
				return ctrl.Result{}, false, err
			}
			return ctrl.Result{Requeue: true}, false, nil
		}
		// Still starting up
		logger.Info("Waiting for replacement pod to become Running", "pod", replacementName, "phase", existing.Status.Phase)
		return ctrl.Result{RequeueAfter: 1 * time.Second}, false, nil
	}
	if !errors.IsNotFound(err) {
		return ctrl.Result{}, false, err
	}

	// Build checkpoint image reference (local, same node)
	var checkpointImage string
	var pullPolicy corev1.PullPolicy
	if m.Spec.TransferMode == "Direct" {
		checkpointImage = fmt.Sprintf("localhost/checkpoint/%s:latest", m.Status.ContainerName)
		pullPolicy = corev1.PullNever
	} else {
		checkpointImage = fmt.Sprintf("%s/%s:checkpoint", m.Spec.CheckpointImageRepository, m.Status.TargetPod)
		pullPolicy = corev1.PullAlways
	}

	// Build containers from source containers captured during Pending
	var containers []corev1.Container
	if len(m.Status.SourceContainers) > 0 {
		for _, c := range m.Status.SourceContainers {
			restored := corev1.Container{
				Name:            c.Name,
				Image:           checkpointImage,
				ImagePullPolicy: pullPolicy,
				Ports:           c.Ports,
			}
			if c.Name != m.Status.ContainerName {
				restored.Image = c.Image
			}
			containers = append(containers, restored)
		}
	} else {
		containers = []corev1.Container{{
			Name:            m.Status.ContainerName,
			Image:           checkpointImage,
			ImagePullPolicy: pullPolicy,
		}}
	}

	// Build labels from source pod labels (for Service routing + StatefulSet adoption)
	// Do NOT include migration labels — this pod should look like a normal StatefulSet pod
	labels := make(map[string]string)
	for k, v := range m.Status.SourcePodLabels {
		labels[k] = v
	}

	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      replacementName,
			Namespace: m.Namespace,
			Labels:    labels,
			// No OwnerReferences — the StatefulSet will adopt this pod
		},
		Spec: corev1.PodSpec{
			NodeName:   m.Spec.TargetNode,
			Containers: containers,
		},
	}

	if err := r.Create(ctx, newPod); err != nil {
		if errors.IsAlreadyExists(err) {
			return ctrl.Result{RequeueAfter: 1 * time.Second}, false, nil
		}
		return ctrl.Result{}, false, fmt.Errorf("create replacement pod: %w", err)
	}

	// Record the replacement pod name
	patch := client.MergeFrom(m.DeepCopy())
	m.Status.ReplacementPod = replacementName
	if err := r.Status().Patch(ctx, m, patch); err != nil {
		return ctrl.Result{}, false, err
	}

	logger.Info("Created replacement pod", "pod", replacementName, "node", m.Spec.TargetNode)
	return ctrl.Result{RequeueAfter: 1 * time.Second}, false, nil
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/controller/ -run TestReconcile_Finalizing_Swap_CreateReplacement -v`
Expected: PASS

**Step 5: Commit**

```bash
git add internal/controller/statefulmigration_controller.go internal/controller/statefulmigration_controller_test.go
git commit -m "Implement CreateReplacement sub-phase for identity swap"
```

---

### Task 6: Implement MiniReplay Sub-Phase

**Files:**
- Modify: `internal/controller/statefulmigration_controller.go`
- Modify: `internal/controller/statefulmigration_controller_test.go`

**Step 1: Write the failing test for queue drained**

```go
func TestReconcile_Finalizing_Swap_MiniReplay_Drained(t *testing.T) {
	migration := newMigration("mig-swap-replay", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.SourcePod = "consumer-0"
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "consumer-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.StatefulSetName = "consumer"
	migration.Status.ContainerName = "app"
	migration.Status.SwapSubPhase = "MiniReplay"
	migration.Status.ReplacementPod = "consumer-0"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.Connected = true
	// Swap queue is empty — replay is complete
	mockBroker.SetQueueDepth("orders.ms2m-swap", 0)

	_, err := reconcileOnce(r, ctx, "mig-swap-replay", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-swap-replay", "default")

	// Should advance to TrafficSwitch
	if got.Status.SwapSubPhase != "TrafficSwitch" {
		t.Errorf("expected SwapSubPhase %q, got %q", "TrafficSwitch", got.Status.SwapSubPhase)
	}

	// Should have sent START_REPLAY to replacement pod
	found := false
	for _, msg := range mockBroker.ControlMessages {
		if msg.TargetPod == "consumer-0" && msg.Type == messaging.ControlStartReplay {
			found = true
		}
	}
	if !found {
		t.Error("expected START_REPLAY control message to replacement pod 'consumer-0'")
	}
}
```

**Step 2: Write test for queue not yet drained**

```go
func TestReconcile_Finalizing_Swap_MiniReplay_Pending(t *testing.T) {
	migration := newMigration("mig-swap-replay-pending", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.SourcePod = "consumer-0"
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "consumer-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.StatefulSetName = "consumer"
	migration.Status.ContainerName = "app"
	migration.Status.SwapSubPhase = "MiniReplay"
	migration.Status.ReplacementPod = "consumer-0"
	migration.Status.PhaseTimings = map[string]string{
		"Swap.MiniReplay.start": time.Now().Format(time.RFC3339),
	}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.Connected = true
	// Queue still has messages
	mockBroker.SetQueueDepth("orders.ms2m-swap", 5)

	result, err := reconcileOnce(r, ctx, "mig-swap-replay-pending", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-swap-replay-pending", "default")

	// Should stay in MiniReplay
	if got.Status.SwapSubPhase != "MiniReplay" {
		t.Errorf("expected SwapSubPhase %q, got %q", "MiniReplay", got.Status.SwapSubPhase)
	}

	// Should requeue with delay
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter > 0 while queue not drained")
	}
}
```

**Step 3: Run tests to verify they fail**

Run: `go test ./internal/controller/ -run "TestReconcile_Finalizing_Swap_MiniReplay" -v`
Expected: FAIL

**Step 4: Implement handleSwapMiniReplay**

```go
// handleSwapMiniReplay sends START_REPLAY to the replacement pod and monitors
// the swap queue depth. Once drained, transitions to TrafficSwitch.
func (r *StatefulMigrationReconciler) handleSwapMiniReplay(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	ensurePhaseTimings(m)

	// Send START_REPLAY on first entry (track with a phase timing key)
	if _, ok := m.Status.PhaseTimings["Swap.MiniReplay.start"]; !ok {
		patch := client.MergeFrom(m.DeepCopy())
		m.Status.PhaseTimings["Swap.MiniReplay.start"] = time.Now().Format(time.RFC3339)

		swapQueue := m.Spec.MessageQueueConfig.QueueName + ".ms2m-swap"
		payload := map[string]interface{}{
			"queue": swapQueue,
		}
		if err := r.MsgClient.SendControlMessage(ctx, m.Status.ReplacementPod, messaging.ControlStartReplay, payload); err != nil {
			return ctrl.Result{}, false, fmt.Errorf("send START_REPLAY to replacement: %w", err)
		}
		_ = r.Status().Patch(ctx, m, patch)
	}

	// Poll the swap queue depth
	swapQueue := m.Spec.MessageQueueConfig.QueueName + ".ms2m-swap"
	depth, err := r.MsgClient.GetQueueDepth(ctx, swapQueue)
	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("get swap queue depth: %w", err)
	}

	logger.Info("Swap replay queue depth", "queue", swapQueue, "depth", depth)

	if depth == 0 {
		// Queue drained — transition to TrafficSwitch
		patch := client.MergeFrom(m.DeepCopy())
		delete(m.Status.PhaseTimings, "Swap.MiniReplay.start")
		m.Status.SwapSubPhase = "TrafficSwitch"
		if err := r.Status().Patch(ctx, m, patch); err != nil {
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{Requeue: true}, false, nil
	}

	// Still draining
	return ctrl.Result{RequeueAfter: 1 * time.Second}, false, nil
}
```

**Step 5: Run tests to verify they pass**

Run: `go test ./internal/controller/ -run "TestReconcile_Finalizing_Swap_MiniReplay" -v`
Expected: Both PASS

**Step 6: Commit**

```bash
git add internal/controller/statefulmigration_controller.go internal/controller/statefulmigration_controller_test.go
git commit -m "Implement MiniReplay sub-phase for identity swap"
```

---

### Task 7: Implement TrafficSwitch Sub-Phase

**Files:**
- Modify: `internal/controller/statefulmigration_controller.go`
- Modify: `internal/controller/statefulmigration_controller_test.go`

**Step 1: Write the failing test**

```go
func TestReconcile_Finalizing_Swap_TrafficSwitch(t *testing.T) {
	stsReplicas := int32(0)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "consumer",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &stsReplicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "consumer"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "consumer"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "consumer:latest"}}},
			},
		},
	}

	shadowPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "consumer-0-shadow",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-2",
			Containers: []corev1.Container{{Name: "app", Image: "consumer:latest"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	replacementPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "consumer-0",
			Namespace: "default",
			Labels:    map[string]string{"app": "consumer"},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-2",
			Containers: []corev1.Container{{Name: "app", Image: "consumer:latest"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	migration := newMigration("mig-swap-switch", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.SourcePod = "consumer-0"
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "consumer-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.StatefulSetName = "consumer"
	migration.Status.ContainerName = "app"
	migration.Status.SwapSubPhase = "TrafficSwitch"
	migration.Status.ReplacementPod = "consumer-0"
	migration.Status.OriginalReplicas = 1
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration, sts, shadowPod, replacementPod)
	mockBroker.Connected = true

	_, err := reconcileOnce(r, ctx, "mig-swap-switch", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-swap-switch", "default")

	// Migration should complete
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected phase %q, got %q", migrationv1alpha1.PhaseCompleted, got.Status.Phase)
	}

	// SwapSubPhase should be cleared
	if got.Status.SwapSubPhase != "" {
		t.Errorf("expected SwapSubPhase cleared, got %q", got.Status.SwapSubPhase)
	}

	// Shadow pod should be deleted
	deletedPod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: "consumer-0-shadow", Namespace: "default"}, deletedPod); !errors.IsNotFound(err) {
		t.Error("expected shadow pod to be deleted")
	}

	// StatefulSet should be scaled back to original replicas
	updatedSts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: "consumer", Namespace: "default"}, updatedSts); err != nil {
		t.Fatalf("failed to get StatefulSet: %v", err)
	}
	if *updatedSts.Spec.Replicas != 1 {
		t.Errorf("expected StatefulSet replicas 1, got %d", *updatedSts.Spec.Replicas)
	}

	// END_REPLAY should have been sent to replacement pod
	found := false
	for _, msg := range mockBroker.ControlMessages {
		if msg.TargetPod == "consumer-0" && msg.Type == messaging.ControlEndReplay {
			found = true
		}
	}
	if !found {
		t.Error("expected END_REPLAY control message to replacement pod 'consumer-0'")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestReconcile_Finalizing_Swap_TrafficSwitch -v`
Expected: FAIL

**Step 3: Implement handleSwapTrafficSwitch**

```go
// handleSwapTrafficSwitch deletes the shadow pod, sends END_REPLAY to the
// replacement, scales up the StatefulSet for adoption, and cleans up the swap queue.
func (r *StatefulMigrationReconciler) handleSwapTrafficSwitch(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	// Send END_REPLAY to the replacement pod
	if err := r.MsgClient.SendControlMessage(ctx, m.Status.ReplacementPod, messaging.ControlEndReplay, nil); err != nil {
		logger.Error(err, "Failed to send END_REPLAY to replacement pod, continuing anyway")
	}

	// Delete the swap secondary queue
	swapQueue := m.Spec.MessageQueueConfig.QueueName + ".ms2m-swap"
	if err := r.MsgClient.DeleteSecondaryQueue(ctx, swapQueue, m.Spec.MessageQueueConfig.QueueName, m.Spec.MessageQueueConfig.ExchangeName); err != nil {
		logger.Error(err, "Failed to delete swap queue, continuing anyway")
	}

	// Delete the shadow pod
	shadowPod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: m.Status.TargetPod, Namespace: m.Namespace}, shadowPod); err == nil {
		if err := r.Delete(ctx, shadowPod); err != nil {
			logger.Error(err, "Failed to delete shadow pod", "pod", m.Status.TargetPod)
		} else {
			logger.Info("Deleted shadow pod", "pod", m.Status.TargetPod)
		}
	} else if !errors.IsNotFound(err) {
		return ctrl.Result{}, false, err
	}

	// Scale up StatefulSet to original replica count for adoption
	if m.Status.OriginalReplicas > 0 {
		sts := &appsv1.StatefulSet{}
		if err := r.Get(ctx, types.NamespacedName{Name: m.Status.StatefulSetName, Namespace: m.Namespace}, sts); err == nil {
			if sts.Spec.Replicas == nil || *sts.Spec.Replicas < m.Status.OriginalReplicas {
				stsPatch := client.MergeFrom(sts.DeepCopy())
				sts.Spec.Replicas = &m.Status.OriginalReplicas
				if err := r.Patch(ctx, sts, stsPatch); err != nil {
					logger.Error(err, "Failed to scale up StatefulSet", "statefulset", m.Status.StatefulSetName)
				} else {
					logger.Info("Scaled up StatefulSet for adoption", "statefulset", m.Status.StatefulSetName, "replicas", m.Status.OriginalReplicas)
				}
			}
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, false, err
		}
	}

	// Update the TargetPod to point to the replacement (it's the final pod)
	patch := client.MergeFrom(m.DeepCopy())
	m.Status.TargetPod = m.Status.ReplacementPod
	m.Status.SwapSubPhase = ""
	if err := r.Status().Patch(ctx, m, patch); err != nil {
		return ctrl.Result{}, false, err
	}

	logger.Info("TrafficSwitch complete, identity swap finished", "replacementPod", m.Status.ReplacementPod)
	return ctrl.Result{}, true, nil
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/controller/ -run TestReconcile_Finalizing_Swap_TrafficSwitch -v`
Expected: PASS

**Step 5: Commit**

```bash
git add internal/controller/statefulmigration_controller.go internal/controller/statefulmigration_controller_test.go
git commit -m "Implement TrafficSwitch sub-phase for identity swap"
```

---

### Task 8: Update Existing ShadowPod+StatefulSet Tests

**Files:**
- Modify: `internal/controller/statefulmigration_controller_test.go`

The existing tests `TestReconcile_Finalizing_ShadowPod_ScalesDownStatefulSet` and `TestReconcile_Finalizing_ShadowPod_MultiReplicaStatefulSet` (lines 2084-2217) expect ShadowPod+StatefulSet to complete immediately with a scale-down. Now they enter the swap flow instead. Update these tests to reflect the new behavior.

**Step 1: Update `TestReconcile_Finalizing_ShadowPod_ScalesDownStatefulSet`**

Change the test to verify it enters the swap sub-phases instead of completing immediately. The migration should NOT go to Completed on the first reconcile — it should set `SwapSubPhase`.

```go
func TestReconcile_Finalizing_ShadowPod_ScalesDownStatefulSet(t *testing.T) {
	// This test now verifies that ShadowPod+StatefulSet enters the identity swap
	// instead of just scaling down and completing.
	stsReplicas := int32(1)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "consumer",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &stsReplicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "consumer"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "consumer"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "consumer:latest"}}},
			},
		},
	}

	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "consumer-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "app", Image: "consumer:latest"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	migration := newMigration("mig-final-shadow-sts", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.SourcePod = "consumer-0"
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "consumer-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.StatefulSetName = "consumer"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration, sts, sourcePod)
	mockBroker.Connected = true

	_, err := reconcileOnce(r, ctx, "mig-final-shadow-sts", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-final-shadow-sts", "default")

	// Should enter swap sub-phases, not complete immediately
	if got.Status.Phase == migrationv1alpha1.PhaseCompleted {
		t.Error("ShadowPod+StatefulSet should enter identity swap, not complete immediately")
	}
	if got.Status.SwapSubPhase == "" {
		t.Error("expected SwapSubPhase to be set")
	}
}
```

**Step 2: Update `TestReconcile_Finalizing_ShadowPod_MultiReplicaStatefulSet`**

Similar update — verify swap entry instead of immediate scale-down completion.

```go
func TestReconcile_Finalizing_ShadowPod_MultiReplicaStatefulSet(t *testing.T) {
	stsReplicas := int32(3)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "consumer",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &stsReplicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "consumer"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "consumer"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "consumer:latest"}}},
			},
		},
	}

	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "consumer-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "app", Image: "consumer:latest"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	migration := newMigration("mig-final-shadow-multi", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.SourcePod = "consumer-0"
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "consumer-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.StatefulSetName = "consumer"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration, sts, sourcePod)
	mockBroker.Connected = true

	_, err := reconcileOnce(r, ctx, "mig-final-shadow-multi", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-final-shadow-multi", "default")

	// Should enter swap sub-phases, not complete immediately
	if got.Status.Phase == migrationv1alpha1.PhaseCompleted {
		t.Error("ShadowPod+StatefulSet should enter identity swap, not complete immediately")
	}
	if got.Status.SwapSubPhase == "" {
		t.Error("expected SwapSubPhase to be set")
	}
}
```

**Step 3: Run all Finalizing tests**

Run: `go test ./internal/controller/ -run TestReconcile_Finalizing -v`
Expected: All PASS

**Step 4: Run full test suite**

Run: `go test ./internal/controller/ -count=1`
Expected: All PASS

**Step 5: Commit**

```bash
git add internal/controller/statefulmigration_controller_test.go
git commit -m "Update ShadowPod+StatefulSet tests for identity swap behavior"
```

---

### Task 9: End-to-End Swap Test

**Files:**
- Modify: `internal/controller/statefulmigration_controller_test.go`

Write a test that exercises the full swap lifecycle: start in Finalizing with ShadowPod+StatefulSet, reconcile through all sub-phases (PrepareSwap → ReCheckpoint → CreateReplacement → MiniReplay → TrafficSwitch), and verify the final state is Completed with the replacement pod owned by the StatefulSet.

**Step 1: Write the end-to-end test**

```go
func TestReconcile_Finalizing_ShadowPod_StatefulSet_FullSwap(t *testing.T) {
	stsReplicas := int32(1)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "consumer",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &stsReplicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "consumer"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "consumer"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "consumer:latest", Ports: []corev1.ContainerPort{{ContainerPort: 8080}}}}},
			},
		},
	}

	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "consumer-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "app", Image: "consumer:latest", Ports: []corev1.ContainerPort{{ContainerPort: 8080}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	shadowPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "consumer-0-shadow",
			Namespace: "default",
			Labels:    map[string]string{"app": "consumer", "migration.ms2m.io/migration": "mig-e2e-swap", "migration.ms2m.io/role": "target"},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-2",
			Containers: []corev1.Container{{Name: "app", Image: "consumer:latest"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	migration := newMigration("mig-e2e-swap", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.SourcePod = "consumer-0"
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Spec.TargetNode = "node-2"
	migration.Spec.TransferMode = "Direct"
	migration.Status.TargetPod = "consumer-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.StatefulSetName = "consumer"
	migration.Status.ContainerName = "app"
	migration.Status.OriginalReplicas = 1
	migration.Status.SourcePodLabels = map[string]string{"app": "consumer"}
	migration.Status.SourceContainers = []corev1.Container{{Name: "app", Image: "consumer:latest", Ports: []corev1.ContainerPort{{ContainerPort: 8080}}}}
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration, sts, sourcePod, shadowPod)
	mockBroker.Connected = true

	// Reconcile loop: drive through all sub-phases
	// Allow up to 20 iterations to prevent infinite loops
	for i := 0; i < 20; i++ {
		result, err := reconcileOnce(r, ctx, "mig-e2e-swap", "default")
		if err != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, err)
		}

		got := fetchMigration(r, ctx, "mig-e2e-swap", "default")

		// If CreateReplacement just created the pod, we need to mark it as Running
		// (fake client doesn't simulate pod startup)
		if got.Status.SwapSubPhase == "CreateReplacement" || got.Status.SwapSubPhase == "MiniReplay" {
			pod := &corev1.Pod{}
			if err := r.Get(ctx, types.NamespacedName{Name: "consumer-0", Namespace: "default"}, pod); err == nil {
				if pod.Status.Phase != corev1.PodRunning {
					pod.Status.Phase = corev1.PodRunning
					_ = r.Status().Update(ctx, pod)
				}
			}
		}

		// If MiniReplay, ensure swap queue is drained
		if got.Status.SwapSubPhase == "MiniReplay" {
			mockBroker.SetQueueDepth("orders.ms2m-swap", 0)
		}

		if got.Status.Phase == migrationv1alpha1.PhaseCompleted {
			// Verify final state
			if got.Status.TargetPod != "consumer-0" {
				t.Errorf("expected final TargetPod 'consumer-0', got %q", got.Status.TargetPod)
			}
			if got.Status.SwapSubPhase != "" {
				t.Errorf("expected SwapSubPhase cleared, got %q", got.Status.SwapSubPhase)
			}

			// Shadow pod should be deleted
			deletedPod := &corev1.Pod{}
			if err := r.Get(ctx, types.NamespacedName{Name: "consumer-0-shadow", Namespace: "default"}, deletedPod); !errors.IsNotFound(err) {
				t.Error("expected shadow pod to be deleted")
			}

			// Replacement pod should exist
			replacementPod := &corev1.Pod{}
			if err := r.Get(ctx, types.NamespacedName{Name: "consumer-0", Namespace: "default"}, replacementPod); err != nil {
				t.Fatalf("replacement pod should exist: %v", err)
			}
			if replacementPod.Labels["app"] != "consumer" {
				t.Error("replacement pod should have app=consumer label")
			}

			// StatefulSet should be scaled back to 1
			updatedSts := &appsv1.StatefulSet{}
			if err := r.Get(ctx, types.NamespacedName{Name: "consumer", Namespace: "default"}, updatedSts); err != nil {
				t.Fatalf("failed to get StatefulSet: %v", err)
			}
			if *updatedSts.Spec.Replicas != 1 {
				t.Errorf("expected StatefulSet replicas 1, got %d", *updatedSts.Spec.Replicas)
			}

			return // Success
		}

		if !result.Requeue && result.RequeueAfter == 0 {
			t.Fatalf("iteration %d: no requeue and not completed, phase=%s subPhase=%s",
				i, got.Status.Phase, got.Status.SwapSubPhase)
		}
	}

	t.Fatal("swap did not complete within 20 iterations")
}
```

**Step 2: Run the test**

Run: `go test ./internal/controller/ -run TestReconcile_Finalizing_ShadowPod_StatefulSet_FullSwap -v`
Expected: PASS (all sub-phase implementations from Tasks 3-7 are in place)

**Step 3: Run full test suite**

Run: `go test ./... -count=1`
Expected: All PASS

**Step 4: Commit**

```bash
git add internal/controller/statefulmigration_controller_test.go
git commit -m "Add end-to-end test for ShadowPod+StatefulSet identity swap"
```

---

### Task 10: Update Documentation and Final Cleanup

**Files:**
- Modify: `README.md`
- Modify: `docs/plans/2026-03-01-statefulset-readoption-design.md`

**Step 1: Update README**

Update the ShadowPod+StatefulSet section in `README.md` to document that the identity swap now restores full StatefulSet ownership after migration. Remove the "orphaned" caveat.

**Step 2: Update design doc**

Mark the "Future Work" section as implemented. Update the "Current State" section to reflect that both Sequential and ShadowPod strategies now restore StatefulSet ownership.

**Step 3: Run tests one final time**

Run: `go test ./... -count=1`
Expected: All PASS

**Step 4: Commit**

```bash
git add README.md docs/plans/2026-03-01-statefulset-readoption-design.md
git commit -m "Update documentation for StatefulSet identity swap"
```
