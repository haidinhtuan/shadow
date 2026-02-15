package controller

import (
	"context"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/haidinhtuan/kubernetes-controller/api/v1alpha1"
	"github.com/haidinhtuan/kubernetes-controller/internal/messaging"
)

// testScheme builds a scheme with all types needed by the controller tests.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = migrationv1alpha1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	return s
}

// newMigration creates a StatefulMigration in the given phase with sensible defaults.
func newMigration(name string, phase migrationv1alpha1.Phase) *migrationv1alpha1.StatefulMigration {
	return &migrationv1alpha1.StatefulMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: migrationv1alpha1.StatefulMigrationSpec{
			SourcePod:                 "myapp-0",
			TargetNode:                "node-2",
			CheckpointImageRepository: "registry.example.com/checkpoints",
			ReplayCutoffSeconds:       5,
			MessageQueueConfig: migrationv1alpha1.MessageQueueConfig{
				QueueName:    "orders",
				BrokerURL:    "amqp://localhost:5672",
				ExchangeName: "orders.fanout",
				RoutingKey:   "orders.new",
			},
		},
		Status: migrationv1alpha1.StatefulMigrationStatus{
			Phase: phase,
		},
	}
}

// setupTest creates a reconciler backed by a fake client and mock broker.
// The provided objects are seeded into the fake client.
func setupTest(objs ...client.Object) (*StatefulMigrationReconciler, *messaging.MockBrokerClient, context.Context) {
	scheme := testScheme()
	cb := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&migrationv1alpha1.StatefulMigration{})
	if len(objs) > 0 {
		cb = cb.WithObjects(objs...)
	}
	fakeClient := cb.Build()

	mockBroker := messaging.NewMockBrokerClient()

	reconciler := &StatefulMigrationReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		MsgClient: mockBroker,
		// KubeletClient is nil for most tests; set it explicitly where needed
	}

	return reconciler, mockBroker, context.Background()
}

func reconcileOnce(r *StatefulMigrationReconciler, ctx context.Context, name, ns string) (ctrl.Result, error) {
	return r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
	})
}

func fetchMigration(r *StatefulMigrationReconciler, ctx context.Context, name, ns string) *migrationv1alpha1.StatefulMigration {
	m := &migrationv1alpha1.StatefulMigration{}
	_ = r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, m)
	return m
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestReconcile_EmptyPhase_TransitionsToPending(t *testing.T) {
	migration := newMigration("mig-1", "")
	r, _, ctx := setupTest(migration)

	result, err := reconcileOnce(r, ctx, "mig-1", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Requeue {
		t.Error("expected Requeue to be true after transitioning to Pending")
	}

	got := fetchMigration(r, ctx, "mig-1", "default")
	if got.Status.Phase != migrationv1alpha1.PhasePending {
		t.Errorf("expected phase %q, got %q", migrationv1alpha1.PhasePending, got.Status.Phase)
	}
}

func TestReconcile_Completed_NoAction(t *testing.T) {
	migration := newMigration("mig-done", migrationv1alpha1.PhaseCompleted)
	r, _, ctx := setupTest(migration)

	result, err := reconcileOnce(r, ctx, "mig-done", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Error("expected Requeue to be false for completed migration")
	}

	got := fetchMigration(r, ctx, "mig-done", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected phase to remain Completed, got %q", got.Status.Phase)
	}
}

func TestReconcile_Failed_NoAction(t *testing.T) {
	migration := newMigration("mig-fail", migrationv1alpha1.PhaseFailed)
	r, _, ctx := setupTest(migration)

	result, err := reconcileOnce(r, ctx, "mig-fail", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Error("expected Requeue to be false for failed migration")
	}
}

func TestReconcile_Pending_SetsSourceNode(t *testing.T) {
	// The source pod must exist for handlePending to look it up.
	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
	}

	migration := newMigration("mig-pending", migrationv1alpha1.PhasePending)
	r, _, ctx := setupTest(migration, sourcePod)

	result, err := reconcileOnce(r, ctx, "mig-pending", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Requeue {
		t.Error("expected Requeue after Pending -> Checkpointing transition")
	}

	got := fetchMigration(r, ctx, "mig-pending", "default")
	if got.Status.SourceNode != "node-1" {
		t.Errorf("expected sourceNode %q, got %q", "node-1", got.Status.SourceNode)
	}
	if got.Status.StartTime == nil {
		t.Error("expected startTime to be set")
	}
	if got.Status.Phase != migrationv1alpha1.PhaseCheckpointing {
		t.Errorf("expected phase %q, got %q", migrationv1alpha1.PhaseCheckpointing, got.Status.Phase)
	}
}

func TestReconcile_Pending_DetectsStatefulSetStrategy(t *testing.T) {
	// Pod owned by a StatefulSet should auto-detect "Sequential" strategy.
	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "myapp",
					UID:        "abc-123",
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
	}

	migration := newMigration("mig-ss", migrationv1alpha1.PhasePending)
	// Leave MigrationStrategy empty so it gets auto-detected
	migration.Spec.MigrationStrategy = ""

	r, _, ctx := setupTest(migration, sourcePod)

	_, err := reconcileOnce(r, ctx, "mig-ss", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-ss", "default")
	if got.Spec.MigrationStrategy != "Sequential" {
		t.Errorf("expected strategy %q, got %q", "Sequential", got.Spec.MigrationStrategy)
	}
}

func TestReconcile_Pending_DefaultsShadowPodStrategy(t *testing.T) {
	// Pod NOT owned by a StatefulSet should default to ShadowPod.
	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
	}

	migration := newMigration("mig-shadow", migrationv1alpha1.PhasePending)
	migration.Spec.MigrationStrategy = ""

	r, _, ctx := setupTest(migration, sourcePod)

	_, err := reconcileOnce(r, ctx, "mig-shadow", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-shadow", "default")
	if got.Spec.MigrationStrategy != "ShadowPod" {
		t.Errorf("expected strategy %q, got %q", "ShadowPod", got.Spec.MigrationStrategy)
	}
}

func TestReconcile_Pending_SourcePodNotFound(t *testing.T) {
	// No source pod seeded -- should fail the migration.
	migration := newMigration("mig-nopod", migrationv1alpha1.PhasePending)
	r, _, ctx := setupTest(migration)

	_, err := reconcileOnce(r, ctx, "mig-nopod", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-nopod", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseFailed {
		t.Errorf("expected phase Failed when source pod missing, got %q", got.Status.Phase)
	}
}

func TestReconcile_MissingResource_NoError(t *testing.T) {
	// Reconciling a migration that doesn't exist should be a no-op.
	r, _, ctx := setupTest()

	result, err := reconcileOnce(r, ctx, "nonexistent", "default")
	if err != nil {
		t.Fatalf("unexpected error for missing resource: %v", err)
	}
	if result.Requeue {
		t.Error("expected no requeue for missing resource")
	}
}

func TestRecordPhaseTiming(t *testing.T) {
	migration := newMigration("mig-timing", migrationv1alpha1.PhasePending)
	r, _, _ := setupTest(migration)

	r.recordPhaseTiming(migration, "Checkpointing", 1234*1e6) // 1234ms
	if migration.Status.PhaseTimings == nil {
		t.Fatal("expected PhaseTimings to be initialized")
	}
	if migration.Status.PhaseTimings["Checkpointing"] != "1.234s" {
		t.Errorf("expected timing %q, got %q", "1.234s", migration.Status.PhaseTimings["Checkpointing"])
	}
}

func TestReconcile_Replaying_QueueDrained(t *testing.T) {
	// When queue depth is 0, replaying should transition to finalizing.
	migration := newMigration("mig-replay", migrationv1alpha1.PhaseReplaying)
	migration.Status.TargetPod = "myapp-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration)
	// Queue is already drained (depth 0 is default)
	mockBroker.Connected = true

	result, err := reconcileOnce(r, ctx, "mig-replay", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-replay", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseFinalizing {
		t.Errorf("expected phase %q, got %q", migrationv1alpha1.PhaseFinalizing, got.Status.Phase)
	}

	// Should have sent START_REPLAY
	found := false
	for _, msg := range mockBroker.ControlMessages {
		if msg.Type == messaging.ControlStartReplay {
			found = true
		}
	}
	if !found {
		t.Error("expected START_REPLAY control message to have been sent")
	}

	_ = result
}

func TestReconcile_Replaying_QueueNotDrained(t *testing.T) {
	// When queue depth > 0, should requeue.
	migration := newMigration("mig-replay2", migrationv1alpha1.PhaseReplaying)
	migration.Status.TargetPod = "myapp-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.Connected = true
	// Simulate messages still in the queue
	secondaryQ := migration.Spec.MessageQueueConfig.QueueName + ".ms2m-replay"
	mockBroker.SetQueueDepth(secondaryQ, 100)

	result, err := reconcileOnce(r, ctx, "mig-replay2", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-replay2", "default")
	// Should remain in Replaying since queue is not drained
	if got.Status.Phase != migrationv1alpha1.PhaseReplaying {
		t.Errorf("expected phase to remain %q, got %q", migrationv1alpha1.PhaseReplaying, got.Status.Phase)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter to be set when queue is not drained")
	}
}

func TestReconcile_Finalizing_CompletesWithShadowPod(t *testing.T) {
	// The source pod needs to exist so it can be deleted during finalization.
	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
	}

	migration := newMigration("mig-final", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "myapp-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration, sourcePod)
	mockBroker.Connected = true

	_, err := reconcileOnce(r, ctx, "mig-final", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-final", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected phase %q, got %q", migrationv1alpha1.PhaseCompleted, got.Status.Phase)
	}

	// Verify END_REPLAY was sent
	found := false
	for _, msg := range mockBroker.ControlMessages {
		if msg.Type == messaging.ControlEndReplay {
			found = true
		}
	}
	if !found {
		t.Error("expected END_REPLAY control message to have been sent")
	}

	// Verify broker connection was closed
	if mockBroker.Connected {
		t.Error("expected broker connection to be closed after finalization")
	}

	// Verify the source pod was deleted (ShadowPod strategy)
	pod := &corev1.Pod{}
	podErr := r.Get(ctx, types.NamespacedName{Name: "myapp-0", Namespace: "default"}, pod)
	if podErr == nil {
		t.Error("expected source pod to be deleted in ShadowPod strategy")
	}
}

func TestReconcile_Finalizing_SequentialSkipsSourceDelete(t *testing.T) {
	// In Sequential strategy, the source pod was already deleted during restore,
	// so finalization should not try to delete it.
	migration := newMigration("mig-final-seq", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.MigrationStrategy = "Sequential"
	migration.Status.TargetPod = "myapp-0"
	migration.Status.SourceNode = "node-1"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.Connected = true

	_, err := reconcileOnce(r, ctx, "mig-final-seq", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-final-seq", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected phase %q, got %q", migrationv1alpha1.PhaseCompleted, got.Status.Phase)
	}
}

func TestReconcile_Transferring_JobNotComplete(t *testing.T) {
	// When the transfer job exists but isn't done, should requeue.
	migration := newMigration("mig-xfer", migrationv1alpha1.PhaseTransferring)
	migration.Status.SourceNode = "node-1"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.PhaseTimings = map[string]string{}

	// Create the job in a non-complete state
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mig-xfer-transfer",
			Namespace: "default",
		},
		Status: batchv1.JobStatus{
			Succeeded: 0,
		},
	}

	r, _, ctx := setupTest(migration, job)

	result, err := reconcileOnce(r, ctx, "mig-xfer", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-xfer", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseTransferring {
		t.Errorf("expected phase to remain %q, got %q", migrationv1alpha1.PhaseTransferring, got.Status.Phase)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter to be set while job is in progress")
	}
}

func TestReconcile_Transferring_JobComplete(t *testing.T) {
	migration := newMigration("mig-xfer2", migrationv1alpha1.PhaseTransferring)
	migration.Status.SourceNode = "node-1"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.PhaseTimings = map[string]string{}

	// Create the job in a complete state
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mig-xfer2-transfer",
			Namespace: "default",
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
		},
	}

	r, _, ctx := setupTest(migration, job)

	result, err := reconcileOnce(r, ctx, "mig-xfer2", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-xfer2", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseRestoring {
		t.Errorf("expected phase %q, got %q", migrationv1alpha1.PhaseRestoring, got.Status.Phase)
	}
	if !result.Requeue {
		t.Error("expected Requeue after transitioning to Restoring")
	}
}

func TestReconcile_Transferring_CreatesJob(t *testing.T) {
	// When the transfer job doesn't exist yet, it should be created.
	migration := newMigration("mig-xfer3", migrationv1alpha1.PhaseTransferring)
	migration.Status.SourceNode = "node-1"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.PhaseTimings = map[string]string{}

	r, _, ctx := setupTest(migration)

	result, err := reconcileOnce(r, ctx, "mig-xfer3", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The job should have been created and we're requeuing to check it
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter to be set after creating the transfer job")
	}

	// Verify the job was created
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: "mig-xfer3-transfer", Namespace: "default"}, job); err != nil {
		t.Fatalf("expected transfer job to be created: %v", err)
	}
	if job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != "node-1" {
		t.Error("expected job to be scheduled on the source node")
	}
}

func TestReconcile_Restoring_ShadowPod_CreatesPod(t *testing.T) {
	migration := newMigration("mig-restore", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.SourceNode = "node-1"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.PhaseTimings = map[string]string{}

	// Source pod must exist to copy container spec from
	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
	}

	r, _, ctx := setupTest(migration, sourcePod)

	result, err := reconcileOnce(r, ctx, "mig-restore", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Target pod should have been created; since it's not Running yet, we requeue
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter while waiting for target pod to become Running")
	}

	// Verify shadow pod was created
	targetPod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: "myapp-0-shadow", Namespace: "default"}, targetPod); err != nil {
		t.Fatalf("expected shadow pod to be created: %v", err)
	}
	if targetPod.Spec.NodeName != "node-2" {
		t.Errorf("expected target pod on %q, got %q", "node-2", targetPod.Spec.NodeName)
	}
}

func TestReconcile_Restoring_ShadowPod_PodRunning(t *testing.T) {
	migration := newMigration("mig-restore2", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.SourceNode = "node-1"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.PhaseTimings = map[string]string{}

	// Source pod
	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
	}

	// Shadow pod already exists and is Running
	shadowPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0-shadow",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "node-2",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	r, _, ctx := setupTest(migration, sourcePod, shadowPod)

	result, err := reconcileOnce(r, ctx, "mig-restore2", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-restore2", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseReplaying {
		t.Errorf("expected phase %q, got %q", migrationv1alpha1.PhaseReplaying, got.Status.Phase)
	}
	if got.Status.TargetPod != "myapp-0-shadow" {
		t.Errorf("expected targetPod %q, got %q", "myapp-0-shadow", got.Status.TargetPod)
	}
	if !result.Requeue {
		t.Error("expected Requeue after transition to Replaying")
	}
}

// ---------------------------------------------------------------------------
// Integration tests (require envtest)
// ---------------------------------------------------------------------------

func TestIntegration_CreateMigration_SetsPending(t *testing.T) {
	if cfg == nil {
		t.Skip("envtest not available, skipping integration test")
	}

	// Create a real client from the envtest config
	k8sClient, err := client.New(cfg, client.Options{Scheme: testScheme()})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx := context.Background()

	// Create a migration CR
	migration := &migrationv1alpha1.StatefulMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "integration-test-1",
			Namespace: "default",
		},
		Spec: migrationv1alpha1.StatefulMigrationSpec{
			SourcePod:                 "app-0",
			TargetNode:                "node-2",
			CheckpointImageRepository: "registry.example.com/checkpoints",
			ReplayCutoffSeconds:       120,
			MessageQueueConfig: migrationv1alpha1.MessageQueueConfig{
				BrokerURL:    "amqp://localhost:5672",
				QueueName:    "app.events",
				ExchangeName: "app.fanout",
			},
		},
	}

	err = k8sClient.Create(ctx, migration)
	if err != nil {
		t.Fatalf("failed to create migration: %v", err)
	}
	defer k8sClient.Delete(ctx, migration)

	// Set up reconciler with mock messaging
	mockMsg := messaging.NewMockBrokerClient()
	reconciler := &StatefulMigrationReconciler{
		Client:    k8sClient,
		Scheme:    testScheme(),
		MsgClient: mockMsg,
	}

	// Reconcile -- should set phase to Pending
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "integration-test-1",
			Namespace: "default",
		},
	})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if !result.Requeue {
		t.Error("expected requeue after initial reconcile")
	}

	// Verify the status was updated
	updated := &migrationv1alpha1.StatefulMigration{}
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "integration-test-1", Namespace: "default"}, updated)
	if err != nil {
		t.Fatalf("failed to get updated migration: %v", err)
	}
	if updated.Status.Phase != migrationv1alpha1.PhasePending {
		t.Errorf("expected phase Pending, got %s", updated.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// Tests for specific fixes
// ---------------------------------------------------------------------------

func TestReconcile_Pending_ResolvesContainerName(t *testing.T) {
	// When Spec.ContainerName is empty, handlePending should auto-detect
	// the container name from the first container in the source pod.
	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "worker", Image: "worker:latest"},
				{Name: "sidecar", Image: "sidecar:latest"},
			},
		},
	}

	migration := newMigration("mig-resolve-ctr", migrationv1alpha1.PhasePending)
	// Ensure ContainerName is empty so auto-detection kicks in
	migration.Spec.ContainerName = ""

	r, _, ctx := setupTest(migration, sourcePod)

	_, err := reconcileOnce(r, ctx, "mig-resolve-ctr", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-resolve-ctr", "default")
	if got.Status.ContainerName != "worker" {
		t.Errorf("expected Status.ContainerName %q (first container), got %q", "worker", got.Status.ContainerName)
	}
}

func TestReconcile_Pending_UsesExplicitContainerName(t *testing.T) {
	// When Spec.ContainerName is set explicitly, it should be used as-is.
	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "main-app", Image: "main:latest"},
				{Name: "my-sidecar", Image: "sidecar:latest"},
			},
		},
	}

	migration := newMigration("mig-explicit-ctr", migrationv1alpha1.PhasePending)
	migration.Spec.ContainerName = "my-sidecar"

	r, _, ctx := setupTest(migration, sourcePod)

	_, err := reconcileOnce(r, ctx, "mig-explicit-ctr", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-explicit-ctr", "default")
	if got.Status.ContainerName != "my-sidecar" {
		t.Errorf("expected Status.ContainerName %q, got %q", "my-sidecar", got.Status.ContainerName)
	}
}

func TestReconcile_Transferring_JobHasOwnerRef(t *testing.T) {
	// Verify the transfer Job has an OwnerReference pointing to the StatefulMigration.
	migration := newMigration("mig-ownerref", migrationv1alpha1.PhaseTransferring)
	migration.Status.SourceNode = "node-1"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	r, _, ctx := setupTest(migration)

	_, err := reconcileOnce(r, ctx, "mig-ownerref", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: "mig-ownerref-transfer", Namespace: "default"}, job); err != nil {
		t.Fatalf("expected transfer job to be created: %v", err)
	}

	if len(job.OwnerReferences) == 0 {
		t.Fatal("expected job to have at least one OwnerReference")
	}
	ownerRef := job.OwnerReferences[0]
	if ownerRef.Kind != "StatefulMigration" {
		t.Errorf("expected OwnerReference Kind %q, got %q", "StatefulMigration", ownerRef.Kind)
	}
	if ownerRef.Name != "mig-ownerref" {
		t.Errorf("expected OwnerReference Name %q, got %q", "mig-ownerref", ownerRef.Name)
	}
}

func TestReconcile_Transferring_JobHasVolumeMount(t *testing.T) {
	// Verify the transfer Job has a hostPath volume mount at /var/lib/kubelet/checkpoints.
	migration := newMigration("mig-volmnt", migrationv1alpha1.PhaseTransferring)
	migration.Status.SourceNode = "node-1"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	r, _, ctx := setupTest(migration)

	_, err := reconcileOnce(r, ctx, "mig-volmnt", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: "mig-volmnt-transfer", Namespace: "default"}, job); err != nil {
		t.Fatalf("expected transfer job to be created: %v", err)
	}

	// Check volumes for hostPath
	foundVolume := false
	for _, vol := range job.Spec.Template.Spec.Volumes {
		if vol.Name == "checkpoints" && vol.HostPath != nil && vol.HostPath.Path == "/var/lib/kubelet/checkpoints" {
			foundVolume = true
		}
	}
	if !foundVolume {
		t.Error("expected job to have a hostPath volume named 'checkpoints' at /var/lib/kubelet/checkpoints")
	}

	// Check volume mount on the container
	containers := job.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		t.Fatal("expected at least one container in the job")
	}
	foundMount := false
	for _, vm := range containers[0].VolumeMounts {
		if vm.Name == "checkpoints" && vm.MountPath == "/var/lib/kubelet/checkpoints" {
			foundMount = true
		}
	}
	if !foundMount {
		t.Error("expected container to have a volume mount named 'checkpoints' at /var/lib/kubelet/checkpoints")
	}
}

func TestReconcile_Transferring_JobPassesContainerName(t *testing.T) {
	// Verify the transfer Job's container Args include 3 arguments:
	// checkpoint path, image ref, and container name.
	migration := newMigration("mig-args", migrationv1alpha1.PhaseTransferring)
	migration.Status.SourceNode = "node-1"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.ContainerName = "my-container"
	migration.Status.PhaseTimings = map[string]string{}

	r, _, ctx := setupTest(migration)

	_, err := reconcileOnce(r, ctx, "mig-args", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: "mig-args-transfer", Namespace: "default"}, job); err != nil {
		t.Fatalf("expected transfer job to be created: %v", err)
	}

	containers := job.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		t.Fatal("expected at least one container in the job")
	}

	args := containers[0].Args
	if len(args) != 3 {
		t.Fatalf("expected 3 args (checkpoint path, image ref, container name), got %d: %v", len(args), args)
	}
	if args[0] != migration.Status.CheckpointID {
		t.Errorf("expected args[0] (checkpoint path) %q, got %q", migration.Status.CheckpointID, args[0])
	}
	expectedImageRef := fmt.Sprintf("%s/%s:checkpoint", migration.Spec.CheckpointImageRepository, migration.Spec.SourcePod)
	if args[1] != expectedImageRef {
		t.Errorf("expected args[1] (image ref) %q, got %q", expectedImageRef, args[1])
	}
	if args[2] != "my-container" {
		t.Errorf("expected args[2] (container name) %q, got %q", "my-container", args[2])
	}
}

func TestReconcile_Restoring_ShadowPod_HasOwnerRef(t *testing.T) {
	// Verify the target pod created during restore has an OwnerReference
	// to the StatefulMigration.
	migration := newMigration("mig-pod-owner", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.PhaseTimings = map[string]string{}

	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
	}

	r, _, ctx := setupTest(migration, sourcePod)

	_, err := reconcileOnce(r, ctx, "mig-pod-owner", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	targetPod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: "myapp-0-shadow", Namespace: "default"}, targetPod); err != nil {
		t.Fatalf("expected shadow pod to be created: %v", err)
	}

	if len(targetPod.OwnerReferences) == 0 {
		t.Fatal("expected target pod to have at least one OwnerReference")
	}
	ownerRef := targetPod.OwnerReferences[0]
	if ownerRef.Kind != "StatefulMigration" {
		t.Errorf("expected OwnerReference Kind %q, got %q", "StatefulMigration", ownerRef.Kind)
	}
	if ownerRef.Name != "mig-pod-owner" {
		t.Errorf("expected OwnerReference Name %q, got %q", "mig-pod-owner", ownerRef.Name)
	}
}

func TestReconcile_Restoring_ShadowPod_CopiesSourceLabels(t *testing.T) {
	// Verify that source pod labels are copied to the target pod.
	migration := newMigration("mig-labels", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.PhaseTimings = map[string]string{}

	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
			Labels: map[string]string{
				"app":     "myapp",
				"version": "v1",
				"team":    "backend",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
	}

	r, _, ctx := setupTest(migration, sourcePod)

	_, err := reconcileOnce(r, ctx, "mig-labels", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	targetPod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: "myapp-0-shadow", Namespace: "default"}, targetPod); err != nil {
		t.Fatalf("expected shadow pod to be created: %v", err)
	}

	// Verify source labels are copied
	for _, key := range []string{"app", "version", "team"} {
		if targetPod.Labels[key] != sourcePod.Labels[key] {
			t.Errorf("expected label %q=%q on target pod, got %q", key, sourcePod.Labels[key], targetPod.Labels[key])
		}
	}

	// Verify migration labels are also present
	if targetPod.Labels["migration.ms2m.io/migration"] != "mig-labels" {
		t.Errorf("expected migration label to be set, got %q", targetPod.Labels["migration.ms2m.io/migration"])
	}
	if targetPod.Labels["migration.ms2m.io/role"] != "target" {
		t.Errorf("expected role label %q, got %q", "target", targetPod.Labels["migration.ms2m.io/role"])
	}
}

func TestReconcile_Restoring_UsesContainerName(t *testing.T) {
	// Verify the target pod uses Status.ContainerName instead of hardcoded "app".
	migration := newMigration("mig-ctr-name", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "my-worker"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.PhaseTimings = map[string]string{}

	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "my-worker", Image: "worker:latest"},
			},
		},
	}

	r, _, ctx := setupTest(migration, sourcePod)

	_, err := reconcileOnce(r, ctx, "mig-ctr-name", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	targetPod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: "myapp-0-shadow", Namespace: "default"}, targetPod); err != nil {
		t.Fatalf("expected shadow pod to be created: %v", err)
	}

	if len(targetPod.Spec.Containers) == 0 {
		t.Fatal("expected at least one container in the target pod")
	}
	if targetPod.Spec.Containers[0].Name != "my-worker" {
		t.Errorf("expected container name %q, got %q", "my-worker", targetPod.Spec.Containers[0].Name)
	}
}

func TestIntegration_MigrationNotFound_NoError(t *testing.T) {
	if cfg == nil {
		t.Skip("envtest not available, skipping integration test")
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: testScheme()})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	reconciler := &StatefulMigrationReconciler{
		Client:    k8sClient,
		Scheme:    testScheme(),
		MsgClient: messaging.NewMockBrokerClient(),
	}

	// Reconcile a non-existent resource
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "does-not-exist",
			Namespace: "default",
		},
	})
	if err != nil {
		t.Fatalf("expected no error for missing resource, got: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue for missing resource")
	}
}
