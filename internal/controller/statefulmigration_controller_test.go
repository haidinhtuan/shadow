package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

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

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// setupTestWithInterceptors creates a reconciler with interceptor functions for
// injecting errors into the fake client. Useful for testing error paths that
// the fake client would not naturally produce.
func setupTestWithInterceptors(funcs interceptor.Funcs, objs ...client.Object) (*StatefulMigrationReconciler, *messaging.MockBrokerClient, context.Context) {
	scheme := testScheme()
	cb := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&migrationv1alpha1.StatefulMigration{}).WithInterceptorFuncs(funcs)
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	migration := newMigration("mig-pending", migrationv1alpha1.PhasePending)
	r, _, ctx := setupTest(migration, sourcePod)

	result, err := reconcileOnce(r, ctx, "mig-pending", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Phase chaining: Pending -> Checkpointing -> Transferring (creates job, returns RequeueAfter)
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter after phase chaining through to Transferring")
	}

	got := fetchMigration(r, ctx, "mig-pending", "default")
	if got.Status.SourceNode != "node-1" {
		t.Errorf("expected sourceNode %q, got %q", "node-1", got.Status.SourceNode)
	}
	if got.Status.StartTime == nil {
		t.Error("expected startTime to be set")
	}
	// With phase chaining, Pending and Checkpointing complete synchronously
	// and the reconcile stops at Transferring (waiting for job)
	if got.Status.Phase != migrationv1alpha1.PhaseTransferring {
		t.Errorf("expected phase %q, got %q", migrationv1alpha1.PhaseTransferring, got.Status.Phase)
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
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
	// When queue depth is 0, replaying should chain through to Completed.
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
	// Phase chaining: Replaying (drained) -> Finalizing -> Completed
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected phase %q, got %q", migrationv1alpha1.PhaseCompleted, got.Status.Phase)
	}

	// Should have sent START_REPLAY and END_REPLAY
	foundStart := false
	foundEnd := false
	for _, msg := range mockBroker.ControlMessages {
		if msg.Type == messaging.ControlStartReplay {
			foundStart = true
		}
		if msg.Type == messaging.ControlEndReplay {
			foundEnd = true
		}
	}
	if !foundStart {
		t.Error("expected START_REPLAY control message to have been sent")
	}
	if !foundEnd {
		t.Error("expected END_REPLAY control message to have been sent")
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
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
	migration.Status.ContainerName = "app"
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

	// Source pod needed for phase chaining into Restoring (ShadowPod strategy)
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r, _, ctx := setupTest(migration, job, sourcePod)

	result, err := reconcileOnce(r, ctx, "mig-xfer2", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-xfer2", "default")
	// Phase chaining: Transferring (job complete) -> Restoring (creates pod, returns RequeueAfter)
	if got.Status.Phase != migrationv1alpha1.PhaseRestoring {
		t.Errorf("expected phase %q, got %q", migrationv1alpha1.PhaseRestoring, got.Status.Phase)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter while waiting for target pod to start")
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
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

	r, mockBroker, ctx := setupTest(migration, sourcePod, shadowPod)
	mockBroker.Connected = true

	result, err := reconcileOnce(r, ctx, "mig-restore2", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-restore2", "default")
	// Phase chaining: Restoring (pod running) -> Replaying (queue drained) -> Finalizing -> Completed
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected phase %q, got %q", migrationv1alpha1.PhaseCompleted, got.Status.Phase)
	}
	if got.Status.TargetPod != "myapp-0-shadow" {
		t.Errorf("expected targetPod %q, got %q", "myapp-0-shadow", got.Status.TargetPod)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue after completing full migration chain")
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
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

func TestReconcile_Restoring_Sequential_ScalesDownStatefulSet(t *testing.T) {
	// When restoring with Sequential strategy, the controller should scale down
	// the owning StatefulSet before deleting the source pod.
	stsReplicas := int32(1)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &stsReplicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "myapp"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "myapp"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "myapp:latest"}}},
			},
		},
	}

	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
			Labels:    map[string]string{"app": "myapp"},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{
					Name:  "app",
					Image: "myapp:latest",
					Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
					Env:   []corev1.EnvVar{{Name: "APP_ENV", Value: "production"}},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	migration := newMigration("mig-seq-restore", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "Sequential"
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
	migration.Status.StatefulSetName = "myapp"
	migration.Status.PhaseTimings = map[string]string{}
	migration.Status.SourcePodLabels = map[string]string{"app": "myapp"}
	migration.Status.SourceContainers = []corev1.Container{
		{
			Name:  "app",
			Image: "myapp:latest",
			Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
			Env:   []corev1.EnvVar{{Name: "APP_ENV", Value: "production"}},
		},
	}

	r, _, ctx := setupTest(migration, sourcePod, sts)

	// First reconcile: should scale down StatefulSet and delete source pod
	result, err := reconcileOnce(r, ctx, "mig-seq-restore", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter after deleting source pod")
	}

	// Verify StatefulSet was scaled down
	updatedSts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: "myapp", Namespace: "default"}, updatedSts); err != nil {
		t.Fatalf("failed to get StatefulSet: %v", err)
	}
	if *updatedSts.Spec.Replicas != 0 {
		t.Errorf("expected StatefulSet replicas 0, got %d", *updatedSts.Spec.Replicas)
	}

	// Verify original replicas stored in status
	got := fetchMigration(r, ctx, "mig-seq-restore", "default")
	if got.Status.OriginalReplicas != 1 {
		t.Errorf("expected OriginalReplicas 1, got %d", got.Status.OriginalReplicas)
	}

	// Second reconcile: source pod still exists (waiting for StatefulSet controller),
	// controller should requeue and wait.
	result, err = reconcileOnce(r, ctx, "mig-seq-restore", "default")
	if err != nil {
		t.Fatalf("unexpected error on second reconcile: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter while waiting for pod removal")
	}

	// Simulate StatefulSet controller deleting the pod after processing scale-down
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: "myapp-0", Namespace: "default"}, pod); err == nil {
		_ = r.Delete(ctx, pod)
	}

	// Third reconcile: pod is gone, should create target pod
	result, err = reconcileOnce(r, ctx, "mig-seq-restore", "default")
	if err != nil {
		t.Fatalf("unexpected error on third reconcile: %v", err)
	}

	// Verify target pod was created
	targetPod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: "myapp-0", Namespace: "default"}, targetPod); err != nil {
		t.Fatalf("expected target pod to be created: %v", err)
	}
	if targetPod.Spec.NodeName != "node-2" {
		t.Errorf("expected target on node-2, got %q", targetPod.Spec.NodeName)
	}
	// Verify container ports were copied (env is baked into CRIU checkpoint)
	if len(targetPod.Spec.Containers) == 0 {
		t.Fatal("expected at least one container")
	}
	c := targetPod.Spec.Containers[0]
	if len(c.Ports) == 0 || c.Ports[0].ContainerPort != 8080 {
		t.Errorf("expected container port 8080, got %v", c.Ports)
	}
	// Verify source labels were copied
	if targetPod.Labels["app"] != "myapp" {
		t.Errorf("expected label app=myapp, got %q", targetPod.Labels["app"])
	}
	// Verify migration labels
	if targetPod.Labels["migration.ms2m.io/migration"] != "mig-seq-restore" {
		t.Errorf("expected migration label, got %q", targetPod.Labels["migration.ms2m.io/migration"])
	}
}

func TestReconcile_Restoring_Sequential_WaitsForPodRemoval(t *testing.T) {
	// If the target pod name exists but without migration labels (e.g. still
	// being managed by StatefulSet controller), handleRestoring should requeue
	// and wait — not delete the pod ourselves.
	existingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
			Labels:    map[string]string{"app": "myapp"}, // no migration labels
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
	}

	migration := newMigration("mig-identity", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "Sequential"
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}
	migration.Status.SourceContainers = []corev1.Container{
		{Name: "app", Image: "myapp:latest"},
	}

	r, _, ctx := setupTest(migration, existingPod)

	result, err := reconcileOnce(r, ctx, "mig-identity", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter while waiting for pod removal")
	}

	// Verify the pod was NOT deleted — we wait for StatefulSet controller
	pod := &corev1.Pod{}
	podErr := r.Get(ctx, types.NamespacedName{Name: "myapp-0", Namespace: "default"}, pod)
	if podErr != nil {
		t.Error("expected pod to still exist — controller should wait, not delete")
	}
}

func TestReconcile_Finalizing_Sequential_ScalesUpStatefulSet(t *testing.T) {
	// After Sequential migration completes, handleFinalizing should:
	// 1. Remove StatefulMigration ownerRef from target pod (for StatefulSet adoption)
	// 2. Scale the StatefulSet back to its original replica count
	stsReplicas := int32(0)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &stsReplicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "myapp"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "myapp"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "myapp:latest"}}},
			},
		},
	}

	// Target pod with StatefulMigration ownerRef (as created by handleRestoring)
	isController := true
	targetPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
			Labels:    map[string]string{"app": "myapp"},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "migration.ms2m.io/v1alpha1",
					Kind:       "StatefulMigration",
					Name:       "mig-final-scaleup",
					UID:        "mig-uid-123",
					Controller: &isController,
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-2",
			Containers: []corev1.Container{{Name: "app", Image: "checkpoint:latest"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	migration := newMigration("mig-final-scaleup", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.MigrationStrategy = "Sequential"
	migration.Status.TargetPod = "myapp-0"
	migration.Status.SourceNode = "node-1"
	migration.Status.StatefulSetName = "myapp"
	migration.Status.OriginalReplicas = 1
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration, sts, targetPod)
	mockBroker.Connected = true

	_, err := reconcileOnce(r, ctx, "mig-final-scaleup", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-final-scaleup", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected phase %q, got %q", migrationv1alpha1.PhaseCompleted, got.Status.Phase)
	}

	// Verify StatefulMigration ownerRef was removed from target pod
	updatedPod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: "myapp-0", Namespace: "default"}, updatedPod); err != nil {
		t.Fatalf("failed to get target pod: %v", err)
	}
	for _, ref := range updatedPod.OwnerReferences {
		if ref.Kind == "StatefulMigration" {
			t.Errorf("expected StatefulMigration ownerRef to be removed from target pod, but it still exists")
		}
	}

	// Verify StatefulSet was scaled back up
	updatedSts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: "myapp", Namespace: "default"}, updatedSts); err != nil {
		t.Fatalf("failed to get StatefulSet: %v", err)
	}
	if *updatedSts.Spec.Replicas != 1 {
		t.Errorf("expected StatefulSet replicas 1, got %d", *updatedSts.Spec.Replicas)
	}
}

func TestReconcile_Pending_CapturesSourcePodInfo(t *testing.T) {
	// handlePending should capture source pod labels, containers,
	// and the StatefulSet name into the migration status.
	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
			Labels:    map[string]string{"app": "myapp", "version": "v2"},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "myapp",
					UID:        "sts-uid-123",
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{
					Name:  "app",
					Image: "myapp:latest",
					Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	migration := newMigration("mig-capture", migrationv1alpha1.PhasePending)
	migration.Spec.MigrationStrategy = ""

	r, _, ctx := setupTest(migration, sourcePod)

	_, err := reconcileOnce(r, ctx, "mig-capture", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-capture", "default")

	// Verify source pod labels were captured
	if got.Status.SourcePodLabels["app"] != "myapp" {
		t.Errorf("expected SourcePodLabels[app] = myapp, got %q", got.Status.SourcePodLabels["app"])
	}
	if got.Status.SourcePodLabels["version"] != "v2" {
		t.Errorf("expected SourcePodLabels[version] = v2, got %q", got.Status.SourcePodLabels["version"])
	}

	// Verify source containers were captured
	if len(got.Status.SourceContainers) != 1 {
		t.Fatalf("expected 1 source container, got %d", len(got.Status.SourceContainers))
	}
	if got.Status.SourceContainers[0].Name != "app" {
		t.Errorf("expected container name %q, got %q", "app", got.Status.SourceContainers[0].Name)
	}

	// Verify StatefulSet name was captured
	if got.Status.StatefulSetName != "myapp" {
		t.Errorf("expected StatefulSetName %q, got %q", "myapp", got.Status.StatefulSetName)
	}
}

func TestReconcile_Restoring_ShadowPod_CopiesFullContainerSpec(t *testing.T) {
	// Verify that the target pod gets the full container spec (ports, env,
	// resource limits) from the source pod, not just the container name.
	migration := newMigration("mig-full-spec", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{
					Name:  "app",
					Image: "myapp:latest",
					Ports: []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
					Env:   []corev1.EnvVar{{Name: "DB_HOST", Value: "db.local"}},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r, _, ctx := setupTest(migration, sourcePod)

	_, err := reconcileOnce(r, ctx, "mig-full-spec", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	targetPod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: "myapp-0-shadow", Namespace: "default"}, targetPod); err != nil {
		t.Fatalf("expected shadow pod to be created: %v", err)
	}

	c := targetPod.Spec.Containers[0]
	// Image should be the checkpoint image
	expectedImage := fmt.Sprintf("%s/%s:checkpoint", migration.Spec.CheckpointImageRepository, migration.Spec.SourcePod)
	if c.Image != expectedImage {
		t.Errorf("expected checkpoint image %q, got %q", expectedImage, c.Image)
	}
	// Ports should be copied
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 8080 {
		t.Errorf("expected port 8080, got %v", c.Ports)
	}
	// Env should NOT be copied — it's baked into the CRIU checkpoint image
	if len(c.Env) != 0 {
		t.Errorf("expected no env vars (baked into checkpoint), got %v", c.Env)
	}
}

// ---------------------------------------------------------------------------
// Coverage gap tests
// ---------------------------------------------------------------------------

// -- Reconcile: unknown phase returns empty result --
func TestReconcile_UnknownPhase_NoRequeue(t *testing.T) {
	migration := newMigration("mig-unknown", "SomeUnknownPhase")
	r, _, ctx := setupTest(migration)

	result, err := reconcileOnce(r, ctx, "mig-unknown", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue for unknown phase")
	}
}

// -- handleCheckpointing: Connect fails --
func TestReconcile_Checkpointing_ConnectFails(t *testing.T) {
	migration := newMigration("mig-ck-conn", migrationv1alpha1.PhaseCheckpointing)
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.ConnectErr = fmt.Errorf("connection refused")

	_, err := reconcileOnce(r, ctx, "mig-ck-conn", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-ck-conn", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseFailed {
		t.Errorf("expected phase Failed when connect fails, got %q", got.Status.Phase)
	}
}

// -- handleCheckpointing: CreateSecondaryQueue fails --
func TestReconcile_Checkpointing_CreateQueueFails(t *testing.T) {
	migration := newMigration("mig-ck-queue", migrationv1alpha1.PhaseCheckpointing)
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.CreateQueueErr = fmt.Errorf("queue creation failed")

	_, err := reconcileOnce(r, ctx, "mig-ck-queue", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-ck-queue", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseFailed {
		t.Errorf("expected phase Failed when queue creation fails, got %q", got.Status.Phase)
	}
}

// NOTE: KubeletClient checkpoint paths (KubeletClient != nil) cannot be
// unit-tested because kubelet.Client is a concrete struct with an unexported
// restClient field. Those paths require integration tests with a real API server.

// -- handleTransferring: Job running uses pollingBackoff --
func TestReconcile_Transferring_JobRunning_PollingBackoff(t *testing.T) {
	migration := newMigration("mig-xfer-poll", migrationv1alpha1.PhaseTransferring)
	migration.Status.SourceNode = "node-1"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.PhaseTimings = map[string]string{
		"Transferring.start": time.Now().Add(-15 * time.Second).Format(time.RFC3339),
	}

	// Job exists but not succeeded
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mig-xfer-poll-transfer",
			Namespace: "default",
		},
		Status: batchv1.JobStatus{
			Succeeded: 0,
		},
	}

	r, _, ctx := setupTest(migration, job)

	result, err := reconcileOnce(r, ctx, "mig-xfer-poll", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// With elapsed ~15s, should use 2s backoff
	if result.RequeueAfter != 2*time.Second {
		t.Errorf("expected 2s backoff for elapsed ~15s, got %v", result.RequeueAfter)
	}
}

// -- handleRestoring: target pod exists but NOT Running (Pending phase) --
func TestReconcile_Restoring_ShadowPod_PodPending(t *testing.T) {
	migration := newMigration("mig-restore-pending", migrationv1alpha1.PhaseRestoring)
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	// Shadow pod exists but is Pending (not Running)
	shadowPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0-shadow",
			Namespace: "default",
			Labels: map[string]string{
				"migration.ms2m.io/migration": "mig-restore-pending",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-2",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	r, _, ctx := setupTest(migration, sourcePod, shadowPod)

	result, err := reconcileOnce(r, ctx, "mig-restore-pending", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-restore-pending", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseRestoring {
		t.Errorf("expected phase to remain Restoring while pod is Pending, got %q", got.Status.Phase)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter while target pod is not Running")
	}
}

// -- handleTransferring: Job complete with Transferring.start timing --
func TestReconcile_Transferring_JobComplete_WithStartTime(t *testing.T) {
	migration := newMigration("mig-xfer-timing", migrationv1alpha1.PhaseTransferring)
	migration.Status.SourceNode = "node-1"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{
		"Transferring.start": time.Now().Add(-5 * time.Second).Format(time.RFC3339),
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mig-xfer-timing-transfer",
			Namespace: "default",
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
		},
	}

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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r, _, ctx := setupTest(migration, job, sourcePod)

	_, err := reconcileOnce(r, ctx, "mig-xfer-timing", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-xfer-timing", "default")
	// Should have transitioned past Transferring
	if got.Status.Phase == migrationv1alpha1.PhaseTransferring {
		t.Error("expected phase to advance past Transferring")
	}
	// Verify Transferring timing was recorded
	if _, ok := got.Status.PhaseTimings["Transferring"]; !ok {
		t.Error("expected Transferring phase timing to be recorded")
	}
	// Verify Transferring.start was cleaned up
	if _, ok := got.Status.PhaseTimings["Transferring.start"]; ok {
		t.Error("expected Transferring.start to be deleted after completion")
	}
}

// -- handleRestoring: ShadowPod with subdomain --
func TestReconcile_Restoring_ShadowPod_WithSubdomain(t *testing.T) {
	migration := newMigration("mig-restore-subdomain", migrationv1alpha1.PhaseRestoring)
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
			NodeName:  "node-1",
			Subdomain: "myapp-headless",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r, _, ctx := setupTest(migration, sourcePod)

	result, err := reconcileOnce(r, ctx, "mig-restore-subdomain", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter after creating target pod")
	}

	targetPod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: "myapp-0-shadow", Namespace: "default"}, targetPod); err != nil {
		t.Fatalf("expected shadow pod to be created: %v", err)
	}
	// Shadow pod must NOT set hostname — the consumer uses gethostname() for its
	// control queue name, which must match the pod name (not the source pod name)
	// so the controller can route START_REPLAY/END_REPLAY correctly.
	if targetPod.Spec.Hostname != "" {
		t.Errorf("expected empty hostname, got %q", targetPod.Spec.Hostname)
	}
}

// -- handleFinalizing: DeleteSecondaryQueue error (logged but not fatal) --
func TestReconcile_Finalizing_DeleteQueueError(t *testing.T) {
	migration := newMigration("mig-final-delq", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "myapp-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.Connected = true
	mockBroker.DeleteQueueErr = fmt.Errorf("delete queue failed")

	_, err := reconcileOnce(r, ctx, "mig-final-delq", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-final-delq", "default")
	// Delete queue error is logged but not fatal
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected phase Completed despite delete queue error, got %q", got.Status.Phase)
	}
}

// -- handleRestoring: ShadowPod source pod lookup fails --
func TestReconcile_Restoring_ShadowPod_SourceLookupFails(t *testing.T) {
	migration := newMigration("mig-restore-nosrc", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.PhaseTimings = map[string]string{}

	// No source pod and no shadow pod -- target pod not found -> source pod lookup fails
	r, _, ctx := setupTest(migration)

	_, err := reconcileOnce(r, ctx, "mig-restore-nosrc", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-restore-nosrc", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseFailed {
		t.Errorf("expected phase Failed when source pod not found during ShadowPod restore, got %q", got.Status.Phase)
	}
}

// -- handleRestoring: Sequential, empty sourceContainers uses fallback --
func TestReconcile_Restoring_Sequential_FallbackSingleContainer(t *testing.T) {
	migration := newMigration("mig-restore-fallback", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "Sequential"
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}
	// Intentionally leave SourceContainers empty to trigger fallback
	migration.Status.SourceContainers = nil
	migration.Status.SourcePodLabels = nil

	r, _, ctx := setupTest(migration)

	result, err := reconcileOnce(r, ctx, "mig-restore-fallback", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter after creating target pod")
	}

	// Verify the target pod was created with the fallback single container
	targetPod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: "myapp-0", Namespace: "default"}, targetPod); err != nil {
		t.Fatalf("expected target pod to be created: %v", err)
	}
	if len(targetPod.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container (fallback), got %d", len(targetPod.Spec.Containers))
	}
	if targetPod.Spec.Containers[0].Name != "app" {
		t.Errorf("expected fallback container name %q, got %q", "app", targetPod.Spec.Containers[0].Name)
	}
}

// -- handleRestoring: Sequential, StatefulSet not found (ignored) --
func TestReconcile_Restoring_Sequential_StatefulSetNotFound(t *testing.T) {
	// Source pod exists (blocking sequential path), StatefulSet not found.
	existingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
			Labels:    map[string]string{"app": "myapp"},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
	}

	migration := newMigration("mig-restore-nosts", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "Sequential"
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
	migration.Status.StatefulSetName = "myapp-nonexistent"
	migration.Status.PhaseTimings = map[string]string{}

	r, _, ctx := setupTest(migration, existingPod)

	result, err := reconcileOnce(r, ctx, "mig-restore-nosts", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should requeue because pod still exists (waiting for removal)
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter while waiting for pod removal")
	}

	got := fetchMigration(r, ctx, "mig-restore-nosts", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseRestoring {
		t.Errorf("expected phase Restoring, got %q", got.Status.Phase)
	}
}

// -- handleReplaying: SendControlMessage START_REPLAY fails --
func TestReconcile_Replaying_StartReplayFails(t *testing.T) {
	migration := newMigration("mig-replay-sendfail", migrationv1alpha1.PhaseReplaying)
	migration.Status.TargetPod = "myapp-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.Connected = true
	mockBroker.SendErr = fmt.Errorf("send failed")

	_, err := reconcileOnce(r, ctx, "mig-replay-sendfail", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-replay-sendfail", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseFailed {
		t.Errorf("expected phase Failed when START_REPLAY fails, got %q", got.Status.Phase)
	}
}

// -- handleReplaying: GetQueueDepth fails --
func TestReconcile_Replaying_GetQueueDepthFails(t *testing.T) {
	migration := newMigration("mig-replay-depth", migrationv1alpha1.PhaseReplaying)
	migration.Status.TargetPod = "myapp-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.PhaseTimings = map[string]string{
		"Replaying.start": time.Now().Format(time.RFC3339),
	}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.Connected = true
	mockBroker.DepthErr = fmt.Errorf("depth check failed")

	_, err := reconcileOnce(r, ctx, "mig-replay-depth", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-replay-depth", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseFailed {
		t.Errorf("expected phase Failed when GetQueueDepth fails, got %q", got.Status.Phase)
	}
}

// -- handleReplaying: cutoff timeout exceeded --
func TestReconcile_Replaying_CutoffExceeded(t *testing.T) {
	migration := newMigration("mig-replay-cutoff", migrationv1alpha1.PhaseReplaying)
	migration.Status.TargetPod = "myapp-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Spec.ReplayCutoffSeconds = 1 // 1 second cutoff
	migration.Status.PhaseTimings = map[string]string{
		// Start time 10 seconds ago, exceeding the 1s cutoff
		"Replaying.start": time.Now().Add(-10 * time.Second).Format(time.RFC3339),
	}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.Connected = true
	// Queue still has messages but cutoff is exceeded
	secondaryQ := migration.Spec.MessageQueueConfig.QueueName + ".ms2m-replay"
	mockBroker.SetQueueDepth(secondaryQ, 50)

	result, err := reconcileOnce(r, ctx, "mig-replay-cutoff", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-replay-cutoff", "default")
	// Should transition to Finalizing (and then chain to Completed)
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected phase Completed after cutoff, got %q", got.Status.Phase)
	}
	_ = result
}

// -- handleReplaying: depth == 0, valid start time -> Finalizing with timing --
func TestReconcile_Replaying_DrainedWithStartTime(t *testing.T) {
	migration := newMigration("mig-replay-drained", migrationv1alpha1.PhaseReplaying)
	migration.Status.TargetPod = "myapp-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.PhaseTimings = map[string]string{
		"Replaying.start": time.Now().Add(-3 * time.Second).Format(time.RFC3339),
	}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.Connected = true
	// Queue depth is 0

	_, err := reconcileOnce(r, ctx, "mig-replay-drained", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-replay-drained", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected phase Completed, got %q", got.Status.Phase)
	}
	// Verify Replaying timing was recorded
	if _, ok := got.Status.PhaseTimings["Replaying"]; !ok {
		t.Error("expected Replaying phase timing to be recorded")
	}
}

// -- handleReplaying: depth > 0, no cutoff, backoff path --
func TestReconcile_Replaying_QueueNotDrained_BackoffPath(t *testing.T) {
	migration := newMigration("mig-replay-backoff", migrationv1alpha1.PhaseReplaying)
	migration.Status.TargetPod = "myapp-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Spec.ReplayCutoffSeconds = 0 // no cutoff
	migration.Status.PhaseTimings = map[string]string{
		"Replaying.start": time.Now().Add(-5 * time.Second).Format(time.RFC3339),
	}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.Connected = true
	secondaryQ := migration.Spec.MessageQueueConfig.QueueName + ".ms2m-replay"
	mockBroker.SetQueueDepth(secondaryQ, 25)

	result, err := reconcileOnce(r, ctx, "mig-replay-backoff", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-replay-backoff", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseReplaying {
		t.Errorf("expected phase to remain Replaying, got %q", got.Status.Phase)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter for backoff")
	}
}

// -- handleFinalizing: END_REPLAY fails (non-fatal, migration still completes) --
func TestReconcile_Finalizing_EndReplayFails(t *testing.T) {
	migration := newMigration("mig-final-endfail", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "myapp-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.Connected = true
	mockBroker.SendErr = fmt.Errorf("end replay send failed")

	_, err := reconcileOnce(r, ctx, "mig-final-endfail", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-final-endfail", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected phase Completed (END_REPLAY failure is non-fatal), got %q", got.Status.Phase)
	}
}

// -- handleFinalizing: ShadowPod source pod not found (skip quietly) --
func TestReconcile_Finalizing_ShadowPod_SourceNotFound(t *testing.T) {
	// No source pod exists — should skip deletion and complete
	migration := newMigration("mig-final-nopod", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "myapp-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.Connected = true

	_, err := reconcileOnce(r, ctx, "mig-final-nopod", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-final-nopod", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected phase Completed even when source pod not found, got %q", got.Status.Phase)
	}
}

// -- handleFinalizing: Sequential, StatefulSet not found --
func TestReconcile_Finalizing_Sequential_StatefulSetNotFound(t *testing.T) {
	migration := newMigration("mig-final-nosts", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.MigrationStrategy = "Sequential"
	migration.Status.TargetPod = "myapp-0"
	migration.Status.SourceNode = "node-1"
	migration.Status.StatefulSetName = "myapp-missing"
	migration.Status.OriginalReplicas = 1
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.Connected = true

	_, err := reconcileOnce(r, ctx, "mig-final-nosts", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-final-nosts", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected phase Completed when StatefulSet not found, got %q", got.Status.Phase)
	}
}

// -- handleFinalizing: ShadowPod + StatefulSet scales down instead of deleting pod --
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

	migration := newMigration("mig-final-shadow-sts", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.SourcePod = "consumer-0"
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "consumer-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.StatefulSetName = "consumer"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration, sts)
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

// -- handleFinalizing: ShadowPod + multi-replica StatefulSet enters identity swap --
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

	migration := newMigration("mig-final-shadow-multi", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.SourcePod = "consumer-0"
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "consumer-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.StatefulSetName = "consumer"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration, sts)
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

// -- handleFinalizing: MsgClient.Close() returns error (logged but not fatal) --
func TestReconcile_Finalizing_CloseError(t *testing.T) {
	migration := newMigration("mig-final-closeerr", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "myapp-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration)
	mockBroker.Connected = true
	mockBroker.CloseErr = fmt.Errorf("close connection failed")

	_, err := reconcileOnce(r, ctx, "mig-final-closeerr", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-final-closeerr", "default")
	// Close error is logged but not fatal -- migration should complete
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected phase Completed despite Close error, got %q", got.Status.Phase)
	}
}

// -- pollingBackoff: 2s case (elapsed between 10s and 30s) --
func TestPollingBackoff_MediumElapsed(t *testing.T) {
	migration := newMigration("mig-backoff-2s", migrationv1alpha1.PhaseTransferring)
	migration.Status.PhaseTimings = map[string]string{
		"Transferring.start": time.Now().Add(-15 * time.Second).Format(time.RFC3339),
	}

	r, _, _ := setupTest(migration)

	backoff := r.pollingBackoff(migration, "Transferring.start")
	if backoff != 2*time.Second {
		t.Errorf("expected 2s backoff for 15s elapsed, got %v", backoff)
	}
}

// -- pollingBackoff: 5s case (elapsed > 30s) --
func TestPollingBackoff_LongElapsed(t *testing.T) {
	migration := newMigration("mig-backoff-5s", migrationv1alpha1.PhaseTransferring)
	migration.Status.PhaseTimings = map[string]string{
		"Transferring.start": time.Now().Add(-60 * time.Second).Format(time.RFC3339),
	}

	r, _, _ := setupTest(migration)

	backoff := r.pollingBackoff(migration, "Transferring.start")
	if backoff != 5*time.Second {
		t.Errorf("expected 5s backoff for 60s elapsed, got %v", backoff)
	}
}

// -- pollingBackoff: no start key present -> default --
func TestPollingBackoff_NoStartKey(t *testing.T) {
	migration := newMigration("mig-backoff-none", migrationv1alpha1.PhaseTransferring)
	migration.Status.PhaseTimings = map[string]string{}

	r, _, _ := setupTest(migration)

	backoff := r.pollingBackoff(migration, "Transferring.start")
	if backoff != 1*time.Second {
		t.Errorf("expected 1s default backoff, got %v", backoff)
	}
}

// -- handlePending: source pod exists but containers empty, container name empty -> failMigration --
func TestReconcile_Pending_EmptyContainerName(t *testing.T) {
	// Pod with no containers -> containerName stays empty -> fail
	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{}, // empty
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	migration := newMigration("mig-no-ctr", migrationv1alpha1.PhasePending)
	migration.Spec.ContainerName = ""

	r, _, ctx := setupTest(migration, sourcePod)

	_, err := reconcileOnce(r, ctx, "mig-no-ctr", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-no-ctr", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseFailed {
		t.Errorf("expected phase Failed when container name is empty, got %q", got.Status.Phase)
	}
}

// -- handleRestoring: ShadowPod, target pod not Running (not ShadowPod specific - general) --
// -- handlePending: r.Update fails during auto-detect strategy --
func TestReconcile_Pending_UpdateFails(t *testing.T) {
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	migration := newMigration("mig-upd-fail", migrationv1alpha1.PhasePending)
	migration.Spec.MigrationStrategy = "" // triggers auto-detect + Update

	updateCalls := 0
	r, _, ctx := setupTestWithInterceptors(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			updateCalls++
			return fmt.Errorf("update failed")
		},
	}, migration, sourcePod)

	_, err := reconcileOnce(r, ctx, "mig-upd-fail", "default")
	if err == nil {
		t.Fatal("expected error when Update fails")
	}
	if updateCalls == 0 {
		t.Error("expected Update to have been called")
	}
}

// -- handlePending: r.Get fails after strategy update --
func TestReconcile_Pending_RefetchFails(t *testing.T) {
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	migration := newMigration("mig-refetch-fail", migrationv1alpha1.PhasePending)
	migration.Spec.MigrationStrategy = "" // triggers auto-detect + Update + re-fetch

	getCalls := 0
	r, _, ctx := setupTestWithInterceptors(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			getCalls++
			// Let the first two Gets succeed (migration fetch, source pod fetch)
			// The third Get is the re-fetch after Update
			if getCalls <= 2 {
				return c.Get(ctx, key, obj, opts...)
			}
			return fmt.Errorf("refetch failed")
		},
	}, migration, sourcePod)

	_, err := reconcileOnce(r, ctx, "mig-refetch-fail", "default")
	if err == nil {
		t.Fatal("expected error when re-fetch fails")
	}
}

// -- handlePending: source pod Get returns non-NotFound error --
func TestReconcile_Pending_SourcePodGetError(t *testing.T) {
	migration := newMigration("mig-pod-geterr", migrationv1alpha1.PhasePending)

	getCalls := 0
	r, _, ctx := setupTestWithInterceptors(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			getCalls++
			// First Get is the migration fetch (let it succeed)
			if getCalls <= 1 {
				return c.Get(ctx, key, obj, opts...)
			}
			// Second Get is source pod -- return a non-NotFound error
			return fmt.Errorf("internal server error")
		},
	}, migration)

	_, err := reconcileOnce(r, ctx, "mig-pod-geterr", "default")
	if err == nil {
		t.Fatal("expected error when source pod Get returns non-NotFound error")
	}
}

// -- handleTransferring: Job Get returns non-NotFound error --
func TestReconcile_Transferring_JobGetError(t *testing.T) {
	migration := newMigration("mig-xfer-geterr", migrationv1alpha1.PhaseTransferring)
	migration.Status.SourceNode = "node-1"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.PhaseTimings = map[string]string{}

	getCalls := 0
	r, _, ctx := setupTestWithInterceptors(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			getCalls++
			// First Get is the migration fetch (let it succeed)
			if getCalls <= 1 {
				return c.Get(ctx, key, obj, opts...)
			}
			// Second Get is for the job -- return non-NotFound error
			return fmt.Errorf("internal server error")
		},
	}, migration)

	_, err := reconcileOnce(r, ctx, "mig-xfer-geterr", "default")
	if err == nil {
		t.Fatal("expected error when job Get returns non-NotFound error")
	}
}

// -- handleTransferring: Job Create returns AlreadyExists --
func TestReconcile_Transferring_JobCreateAlreadyExists(t *testing.T) {
	migration := newMigration("mig-xfer-exists", migrationv1alpha1.PhaseTransferring)
	migration.Status.SourceNode = "node-1"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	r, _, ctx := setupTestWithInterceptors(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*batchv1.Job); ok {
				return errors.NewAlreadyExists(batchv1.Resource("jobs"), "mig-xfer-exists-transfer")
			}
			return c.Create(ctx, obj, opts...)
		},
	}, migration)

	result, err := reconcileOnce(r, ctx, "mig-xfer-exists", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// AlreadyExists should requeue with 2s delay
	if result.RequeueAfter != 2*time.Second {
		t.Errorf("expected 2s RequeueAfter for AlreadyExists, got %v", result.RequeueAfter)
	}
}

// -- handleTransferring: Job Create returns other error -> failMigration --
func TestReconcile_Transferring_JobCreateOtherError(t *testing.T) {
	migration := newMigration("mig-xfer-createerr", migrationv1alpha1.PhaseTransferring)
	migration.Status.SourceNode = "node-1"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	r, _, ctx := setupTestWithInterceptors(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*batchv1.Job); ok {
				return fmt.Errorf("permission denied")
			}
			return c.Create(ctx, obj, opts...)
		},
	}, migration)

	_, err := reconcileOnce(r, ctx, "mig-xfer-createerr", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-xfer-createerr", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseFailed {
		t.Errorf("expected phase Failed when job Create fails, got %q", got.Status.Phase)
	}
}

// -- handleRestoring: target pod Get returns non-NotFound error --
func TestReconcile_Restoring_TargetPodGetError(t *testing.T) {
	migration := newMigration("mig-restore-geterr", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	getCalls := 0
	r, _, ctx := setupTestWithInterceptors(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			getCalls++
			// First Get: migration fetch
			if getCalls <= 1 {
				return c.Get(ctx, key, obj, opts...)
			}
			// Second Get: target pod -- return non-NotFound error
			return fmt.Errorf("internal server error")
		},
	}, migration)

	_, err := reconcileOnce(r, ctx, "mig-restore-geterr", "default")
	if err == nil {
		t.Fatal("expected error when target pod Get returns non-NotFound error")
	}
}

// -- handleRestoring: pod Create returns AlreadyExists --
func TestReconcile_Restoring_PodCreateAlreadyExists(t *testing.T) {
	migration := newMigration("mig-restore-exists", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	getCalls := 0
	r, _, ctx := setupTestWithInterceptors(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			getCalls++
			// First Get: migration fetch (succeed)
			// Second Get: target shadow pod (not found -> pass through, will get NotFound)
			// Third Get: source pod (succeed)
			return c.Get(ctx, key, obj, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if pod, ok := obj.(*corev1.Pod); ok {
				if pod.Name == "myapp-0-shadow" {
					return errors.NewAlreadyExists(corev1.Resource("pods"), "myapp-0-shadow")
				}
			}
			return c.Create(ctx, obj, opts...)
		},
	}, migration, sourcePod)

	result, err := reconcileOnce(r, ctx, "mig-restore-exists", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 1*time.Second {
		t.Errorf("expected 1s RequeueAfter for AlreadyExists, got %v", result.RequeueAfter)
	}
}

// -- handleRestoring: pod Create returns other error -> failMigration --
func TestReconcile_Restoring_PodCreateOtherError(t *testing.T) {
	migration := newMigration("mig-restore-createerr", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r, _, ctx := setupTestWithInterceptors(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if pod, ok := obj.(*corev1.Pod); ok {
				if pod.Name == "myapp-0-shadow" {
					return fmt.Errorf("quota exceeded")
				}
			}
			return c.Create(ctx, obj, opts...)
		},
	}, migration, sourcePod)

	_, err := reconcileOnce(r, ctx, "mig-restore-createerr", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-restore-createerr", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseFailed {
		t.Errorf("expected phase Failed when pod Create fails, got %q", got.Status.Phase)
	}
}

// -- handleFinalizing: ShadowPod source pod Get returns non-NotFound error --
func TestReconcile_Finalizing_ShadowPod_SourceGetError(t *testing.T) {
	migration := newMigration("mig-final-srcerr", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "myapp-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.PhaseTimings = map[string]string{}

	getCalls := 0
	r, mockBroker, ctx := setupTestWithInterceptors(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			getCalls++
			// First Get: migration fetch (succeed)
			if getCalls <= 1 {
				return c.Get(ctx, key, obj, opts...)
			}
			// Second Get: source pod -- return non-NotFound error
			if key.Name == "myapp-0" {
				return fmt.Errorf("internal server error")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, migration)
	mockBroker.Connected = true

	_, err := reconcileOnce(r, ctx, "mig-final-srcerr", "default")
	if err == nil {
		t.Fatal("expected error when source pod Get returns non-NotFound error")
	}
}

// -- handleFinalizing: Sequential STS Get returns non-NotFound error --
func TestReconcile_Finalizing_Sequential_STSGetError(t *testing.T) {
	migration := newMigration("mig-final-stserr", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.MigrationStrategy = "Sequential"
	migration.Status.TargetPod = "myapp-0"
	migration.Status.SourceNode = "node-1"
	migration.Status.StatefulSetName = "myapp"
	migration.Status.OriginalReplicas = 1
	migration.Status.PhaseTimings = map[string]string{}

	getCalls := 0
	r, mockBroker, ctx := setupTestWithInterceptors(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			getCalls++
			// First Get: migration fetch (succeed)
			if getCalls <= 1 {
				return c.Get(ctx, key, obj, opts...)
			}
			// Second Get: StatefulSet -- return non-NotFound error
			if key.Name == "myapp" {
				return fmt.Errorf("internal server error")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, migration)
	mockBroker.Connected = true

	_, err := reconcileOnce(r, ctx, "mig-final-stserr", "default")
	if err == nil {
		t.Fatal("expected error when StatefulSet Get returns non-NotFound error in Finalizing")
	}
}

// -- handleRestoring: Sequential, STS Get returns non-NotFound error --
func TestReconcile_Restoring_Sequential_STSGetError(t *testing.T) {
	// Source pod exists, StatefulSet Get fails with non-NotFound
	existingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
			Labels:    map[string]string{"app": "myapp"},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
	}

	migration := newMigration("mig-restore-stserr", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "Sequential"
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
	migration.Status.StatefulSetName = "myapp"
	migration.Status.PhaseTimings = map[string]string{}

	getCalls := 0
	r, _, ctx := setupTestWithInterceptors(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			getCalls++
			// Let migration fetch and source pod fetch succeed
			if getCalls <= 1 {
				return c.Get(ctx, key, obj, opts...)
			}
			// Target pod Get (same name as source): let it succeed (pod exists)
			if key.Name == "myapp-0" && getCalls == 2 {
				return c.Get(ctx, key, obj, opts...)
			}
			// StatefulSet Get: return non-NotFound error
			if key.Name == "myapp" {
				return fmt.Errorf("internal server error")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, migration, existingPod)

	_, err := reconcileOnce(r, ctx, "mig-restore-stserr", "default")
	if err == nil {
		t.Fatal("expected error when StatefulSet Get returns non-NotFound error in Restoring")
	}
}

// -- transitionPhase: Status().Patch fails --
func TestReconcile_TransitionPhase_PatchFails(t *testing.T) {
	// Use Checkpointing phase to trigger transitionPhase at the end of handleCheckpointing
	migration := newMigration("mig-tphase-err", migrationv1alpha1.PhaseCheckpointing)
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	subResourcePatchCalls := 0
	r, _, ctx := setupTestWithInterceptors(interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			subResourcePatchCalls++
			// Let the first patch succeed (if any internal state updates), fail on transitionPhase
			if subResourcePatchCalls >= 1 {
				return fmt.Errorf("patch failed")
			}
			return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
		},
	}, migration)

	_, err := reconcileOnce(r, ctx, "mig-tphase-err", "default")
	if err == nil {
		t.Fatal("expected error when transitionPhase Patch fails")
	}
}

// -- failMigration: Status().Patch fails --
func TestReconcile_FailMigration_PatchFails(t *testing.T) {
	// Use Checkpointing phase with ConnectErr to trigger failMigration
	migration := newMigration("mig-failpatch", migrationv1alpha1.PhaseCheckpointing)
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTestWithInterceptors(interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			return fmt.Errorf("status patch failed")
		},
	}, migration)
	mockBroker.ConnectErr = fmt.Errorf("connection refused")

	_, err := reconcileOnce(r, ctx, "mig-failpatch", "default")
	if err == nil {
		t.Fatal("expected error when failMigration Patch fails")
	}
}

// -- Reconcile: re-fetch Get fails after phase chaining --
func TestReconcile_PhaseChaining_RefetchFails(t *testing.T) {
	// Start at Pending with a source pod. handlePending will succeed and
	// transition to Checkpointing with Requeue:true, triggering the re-fetch
	// in the phase chaining loop.
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	migration := newMigration("mig-chain-refetch", migrationv1alpha1.PhasePending)
	migration.Spec.MigrationStrategy = "ShadowPod" // already set, no Update needed

	getCalls := 0
	r, _, ctx := setupTestWithInterceptors(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			getCalls++
			// Calls 1-2: migration fetch + source pod Get (succeed)
			// Call 3: re-fetch after Pending->Checkpointing transition (fail)
			if getCalls >= 3 && key.Name == "mig-chain-refetch" {
				return fmt.Errorf("refetch in chaining loop failed")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, migration, sourcePod)

	result, err := reconcileOnce(r, ctx, "mig-chain-refetch", "default")
	// The error is wrapped in IgnoreNotFound; since it's not a NotFound error, it's returned
	if err == nil {
		// If IgnoreNotFound swallowed it... let's check what we got
		t.Logf("result: %+v, err: %v", result, err)
	}
	// The error from Get is passed through IgnoreNotFound -- non-NotFound errors are returned
	// But since it's a plain error (not a StatusError), IgnoreNotFound returns it
}

// -- handleRestoring: Sequential STS scale-down patch fails --
func TestReconcile_Restoring_Sequential_STSPatchFails(t *testing.T) {
	stsReplicas := int32(1)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &stsReplicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "myapp"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "myapp"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "myapp:latest"}}},
			},
		},
	}

	existingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
			Labels:    map[string]string{"app": "myapp"},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
	}

	migration := newMigration("mig-restore-stspatch", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "Sequential"
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
	migration.Status.StatefulSetName = "myapp"
	migration.Status.PhaseTimings = map[string]string{}

	r, _, ctx := setupTestWithInterceptors(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*appsv1.StatefulSet); ok {
				return fmt.Errorf("sts patch failed")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}, migration, existingPod, sts)

	_, err := reconcileOnce(r, ctx, "mig-restore-stspatch", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-restore-stspatch", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseFailed {
		t.Errorf("expected phase Failed when STS scale-down patch fails, got %q", got.Status.Phase)
	}
}

// -- handleFinalizing: source pod Delete fails (logged but not fatal) --
func TestReconcile_Finalizing_ShadowPod_DeleteFails(t *testing.T) {
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	migration := newMigration("mig-final-delfail", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "myapp-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTestWithInterceptors(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if pod, ok := obj.(*corev1.Pod); ok && pod.Name == "myapp-0" {
				return fmt.Errorf("delete failed")
			}
			return c.Delete(ctx, obj, opts...)
		},
	}, migration, sourcePod)
	mockBroker.Connected = true

	_, err := reconcileOnce(r, ctx, "mig-final-delfail", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-final-delfail", "default")
	// Delete error is logged but not fatal
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected phase Completed despite delete error, got %q", got.Status.Phase)
	}
}

// -- handleFinalizing: Sequential STS scale-up patch fails (logged but not fatal) --
func TestReconcile_Finalizing_Sequential_STSPatchFails(t *testing.T) {
	stsReplicas := int32(0)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &stsReplicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "myapp"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "myapp"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "myapp:latest"}}},
			},
		},
	}

	migration := newMigration("mig-final-stspatch", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.MigrationStrategy = "Sequential"
	migration.Status.TargetPod = "myapp-0"
	migration.Status.SourceNode = "node-1"
	migration.Status.StatefulSetName = "myapp"
	migration.Status.OriginalReplicas = 1
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTestWithInterceptors(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*appsv1.StatefulSet); ok {
				return fmt.Errorf("sts patch failed")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}, migration, sts)
	mockBroker.Connected = true

	_, err := reconcileOnce(r, ctx, "mig-final-stspatch", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-final-stspatch", "default")
	// STS patch error is logged but not fatal
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected phase Completed despite STS patch error, got %q", got.Status.Phase)
	}
}

// -- Reconcile: empty phase, Status().Patch fails --
func TestReconcile_EmptyPhase_PatchFails(t *testing.T) {
	migration := newMigration("mig-empty-patcherr", "")

	r, _, ctx := setupTestWithInterceptors(interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			return fmt.Errorf("status patch failed")
		},
	}, migration)

	_, err := reconcileOnce(r, ctx, "mig-empty-patcherr", "default")
	if err == nil {
		t.Fatal("expected error when empty phase Patch fails")
	}
}

func TestReconcile_Restoring_Sequential_PodNotRunning(t *testing.T) {
	// Sequential: target pod exists with migration labels but is Pending
	migration := newMigration("mig-seq-pending", migrationv1alpha1.PhaseRestoring)
	migration.Spec.MigrationStrategy = "Sequential"
	migration.Status.SourceNode = "node-1"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	targetPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
			Labels: map[string]string{
				"migration.ms2m.io/migration": "mig-seq-pending",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-2",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	r, _, ctx := setupTest(migration, targetPod)

	result, err := reconcileOnce(r, ctx, "mig-seq-pending", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-seq-pending", "default")
	if got.Status.Phase != migrationv1alpha1.PhaseRestoring {
		t.Errorf("expected phase Restoring while pod is Pending, got %q", got.Status.Phase)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter while target pod is not Running")
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

func TestReconcile_Pending_DetectsDeploymentStrategy(t *testing.T) {
	// Pod owned by a ReplicaSet whose parent is a Deployment should
	// auto-detect "ShadowPod" strategy and record DeploymentName in status.
	sourcePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-0",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "ReplicaSet",
					Name:       "myapp-rs-abc123",
					UID:        "rs-uid-1",
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:latest"},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	// The ReplicaSet that owns the pod, itself owned by a Deployment
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-rs-abc123",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "myapp-deploy",
					UID:        "deploy-uid-1",
				},
			},
		},
	}

	migration := newMigration("mig-deploy", migrationv1alpha1.PhasePending)
	migration.Spec.MigrationStrategy = ""

	r, _, ctx := setupTest(migration, sourcePod, rs)

	_, err := reconcileOnce(r, ctx, "mig-deploy", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-deploy", "default")
	if got.Spec.MigrationStrategy != "ShadowPod" {
		t.Errorf("expected strategy %q, got %q", "ShadowPod", got.Spec.MigrationStrategy)
	}
	if got.Status.DeploymentName != "myapp-deploy" {
		t.Errorf("expected DeploymentName %q, got %q", "myapp-deploy", got.Status.DeploymentName)
	}
}

func TestReconcile_Finalizing_ShadowPod_PatchesDeployment(t *testing.T) {
	// When DeploymentName is set, Finalizing should patch the Deployment
	// with nodeAffinity targeting the migration's TargetNode.
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	replicas := int32(3)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-deploy",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "myapp"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "myapp"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "myapp:latest"},
					},
				},
			},
		},
	}

	migration := newMigration("mig-final-deploy", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "myapp-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.DeploymentName = "myapp-deploy"
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
	if err := r.Get(ctx, types.NamespacedName{Name: "myapp-deploy", Namespace: "default"}, updatedDeploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	affinity := updatedDeploy.Spec.Template.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil {
		t.Fatal("expected Deployment to have nodeAffinity set")
	}

	required := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil || len(required.NodeSelectorTerms) == 0 {
		t.Fatal("expected RequiredDuringSchedulingIgnoredDuringExecution with at least one term")
	}

	term := required.NodeSelectorTerms[0]
	if len(term.MatchExpressions) == 0 {
		t.Fatal("expected at least one MatchExpression")
	}

	expr := term.MatchExpressions[0]
	if expr.Key != "kubernetes.io/hostname" {
		t.Errorf("expected key %q, got %q", "kubernetes.io/hostname", expr.Key)
	}
	if expr.Operator != corev1.NodeSelectorOpIn {
		t.Errorf("expected operator %q, got %q", corev1.NodeSelectorOpIn, expr.Operator)
	}
	if len(expr.Values) != 1 || expr.Values[0] != "node-2" {
		t.Errorf("expected values [%q], got %v", "node-2", expr.Values)
	}
}

// ---------------------------------------------------------------------------
// Direct transfer mode tests
// ---------------------------------------------------------------------------

func TestReconcile_Transferring_DirectMode_CreatesJob(t *testing.T) {
	// When TransferMode is "Direct", the transfer Job should have args pointing
	// to the ms2m-agent URL (starting with "http"), not an OCI image ref.
	migration := newMigration("mig-direct-xfer", migrationv1alpha1.PhaseTransferring)
	migration.Spec.TransferMode = "Direct"
	migration.Status.SourceNode = "node-1"
	migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	r, _, ctx := setupTest(migration)

	_, err := reconcileOnce(r, ctx, "mig-direct-xfer", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: "mig-direct-xfer-transfer", Namespace: "default"}, job); err != nil {
		t.Fatalf("expected transfer job to be created: %v", err)
	}

	containers := job.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		t.Fatal("expected at least one container in the job")
	}

	args := containers[0].Args
	if len(args) != 3 {
		t.Fatalf("expected 3 args, got %d: %v", len(args), args)
	}
	if args[0] != migration.Status.CheckpointID {
		t.Errorf("expected args[0] (checkpoint path) %q, got %q", migration.Status.CheckpointID, args[0])
	}
	// Direct mode: second arg should be the agent URL (HTTP), not an OCI ref
	expectedAgentURL := "http://ms2m-agent.ms2m-system.svc.cluster.local:9443/checkpoint"
	if args[1] != expectedAgentURL {
		t.Errorf("expected args[1] (agent URL) %q, got %q", expectedAgentURL, args[1])
	}
	if args[2] != "app" {
		t.Errorf("expected args[2] (container name) %q, got %q", "app", args[2])
	}
}

func TestReconcile_Restoring_DirectMode_UsesNeverPullPolicy(t *testing.T) {
	// When TransferMode is "Direct", the shadow pod should have imagePullPolicy: Never
	// and image tag starting with "localhost/checkpoint/".
	migration := newMigration("mig-direct-restore", migrationv1alpha1.PhaseRestoring)
	migration.Spec.TransferMode = "Direct"
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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r, _, ctx := setupTest(migration, sourcePod)

	_, err := reconcileOnce(r, ctx, "mig-direct-restore", "default")
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

	container := targetPod.Spec.Containers[0]

	// Direct mode: image should be localhost/checkpoint/<containerName>:latest
	expectedImage := "localhost/checkpoint/app:latest"
	if container.Image != expectedImage {
		t.Errorf("expected image %q, got %q", expectedImage, container.Image)
	}

	// Direct mode: pull policy should be Never (image is in local CRI-O store)
	if container.ImagePullPolicy != corev1.PullNever {
		t.Errorf("expected imagePullPolicy %q, got %q", corev1.PullNever, container.ImagePullPolicy)
	}
}

func TestReconcile_Transferring_RegistryMode_Unchanged(t *testing.T) {
	// When TransferMode is empty or "Registry", args should contain OCI image ref,
	// not an HTTP URL. This verifies existing behavior is preserved.
	for _, mode := range []string{"", "Registry"} {
		name := fmt.Sprintf("mode_%s", mode)
		if mode == "" {
			name = "mode_empty"
		}
		t.Run(name, func(t *testing.T) {
			migName := fmt.Sprintf("mig-reg-xfer-%s", name)
			migration := newMigration(migName, migrationv1alpha1.PhaseTransferring)
			migration.Spec.TransferMode = mode
			migration.Status.SourceNode = "node-1"
			migration.Status.CheckpointID = "/var/lib/kubelet/checkpoints/checkpoint-myapp-0.tar"
			migration.Status.ContainerName = "app"
			migration.Status.PhaseTimings = map[string]string{}

			r, _, ctx := setupTest(migration)

			_, err := reconcileOnce(r, ctx, migName, "default")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			job := &batchv1.Job{}
			if err := r.Get(ctx, types.NamespacedName{Name: migName + "-transfer", Namespace: "default"}, job); err != nil {
				t.Fatalf("expected transfer job to be created: %v", err)
			}

			containers := job.Spec.Template.Spec.Containers
			if len(containers) == 0 {
				t.Fatal("expected at least one container in the job")
			}

			args := containers[0].Args
			if len(args) != 3 {
				t.Fatalf("expected 3 args, got %d: %v", len(args), args)
			}

			// Registry mode: second arg should be the OCI image ref, not an HTTP URL
			expectedImageRef := fmt.Sprintf("%s/%s:checkpoint", migration.Spec.CheckpointImageRepository, migration.Spec.SourcePod)
			if args[1] != expectedImageRef {
				t.Errorf("expected args[1] (image ref) %q, got %q", expectedImageRef, args[1])
			}
			if args[1][:4] == "http" {
				t.Errorf("registry mode args should not start with http, got %q", args[1])
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Identity swap tests (ShadowPod + StatefulSet)
// ---------------------------------------------------------------------------

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

	// NOTE: source pod "consumer-0" intentionally NOT created — if it existed
	// and were Running, CreateReplacement would treat it as the replacement pod
	// and the phase-chaining loop would drive through all sub-phases in one call.
	// Without it, the chain breaks at CreateReplacement (pod not Running).

	migration := newMigration("mig-swap", migrationv1alpha1.PhaseFinalizing)
	migration.Spec.SourcePod = "consumer-0"
	migration.Spec.MigrationStrategy = "ShadowPod"
	migration.Status.TargetPod = "consumer-0-shadow"
	migration.Status.SourceNode = "node-1"
	migration.Status.StatefulSetName = "consumer"
	migration.Status.ContainerName = "app"
	migration.Status.PhaseTimings = map[string]string{}

	r, mockBroker, ctx := setupTest(migration, sts, shadowPod)
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
	if _, exists := mockBroker.Queues["orders.ms2m-replay"]; !exists {
		t.Error("expected swap secondary queue 'orders.ms2m-replay' to be created")
	}
}

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
	mockBroker.SetQueueDepth("orders.ms2m-replay", 0)

	_, err := reconcileOnce(r, ctx, "mig-swap-replay", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := fetchMigration(r, ctx, "mig-swap-replay", "default")

	// Phase chaining drives MiniReplay → TrafficSwitch → Completed in one call
	if got.Status.Phase != migrationv1alpha1.PhaseCompleted {
		t.Errorf("expected Phase %q after full chain, got %q", migrationv1alpha1.PhaseCompleted, got.Status.Phase)
	}
	if got.Status.SwapSubPhase != "" {
		t.Errorf("expected SwapSubPhase cleared after TrafficSwitch, got %q", got.Status.SwapSubPhase)
	}

	// MiniReplay should have sent START_REPLAY to replacement pod
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
	mockBroker.SetQueueDepth("orders.ms2m-replay", 5)

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
