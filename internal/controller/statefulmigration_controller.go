package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	migrationv1alpha1 "github.com/haidinhtuan/kubernetes-controller/api/v1alpha1"
	"github.com/haidinhtuan/kubernetes-controller/internal/kubelet"
	"github.com/haidinhtuan/kubernetes-controller/internal/messaging"
)

// StatefulMigrationReconciler reconciles a StatefulMigration object
type StatefulMigrationReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	MsgClient     messaging.BrokerClient
	KubeletClient *kubelet.Client
}

// +kubebuilder:rbac:groups=migration.ms2m.io,resources=statefulmigrations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=migration.ms2m.io,resources=statefulmigrations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=migration.ms2m.io,resources=statefulmigrations/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create

// Reconcile drives the StatefulMigration through its phase-based state machine.
// Each invocation handles exactly one phase transition (or requeues to wait).
func (r *StatefulMigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the StatefulMigration instance
	migration := &migrationv1alpha1.StatefulMigration{}
	if err := r.Get(ctx, req.NamespacedName, migration); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Reconciling StatefulMigration", "phase", migration.Status.Phase)

	// State Machine
	switch migration.Status.Phase {
	case "":
		// Initial state, move to Pending
		migration.Status.Phase = migrationv1alpha1.PhasePending
		if err := r.Status().Update(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil

	case migrationv1alpha1.PhasePending:
		return r.handlePending(ctx, migration)

	case migrationv1alpha1.PhaseCheckpointing:
		return r.handleCheckpointing(ctx, migration)

	case migrationv1alpha1.PhaseTransferring:
		return r.handleTransferring(ctx, migration)

	case migrationv1alpha1.PhaseRestoring:
		return r.handleRestoring(ctx, migration)

	case migrationv1alpha1.PhaseReplaying:
		return r.handleReplaying(ctx, migration)

	case migrationv1alpha1.PhaseFinalizing:
		return r.handleFinalizing(ctx, migration)

	case migrationv1alpha1.PhaseCompleted, migrationv1alpha1.PhaseFailed:
		// Terminal states, no action
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// handlePending validates the migration spec and records initial metadata.
// It looks up the source pod to determine the node it runs on and auto-detects
// the migration strategy from the pod's ownerReferences.
func (r *StatefulMigrationReconciler) handlePending(ctx context.Context, m *migrationv1alpha1.StatefulMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Look up the source pod
	sourcePod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      m.Spec.SourcePod,
		Namespace: m.Namespace,
	}, sourcePod)
	if err != nil {
		if errors.IsNotFound(err) {
			return r.failMigration(ctx, m, fmt.Sprintf("source pod %q not found", m.Spec.SourcePod))
		}
		return ctrl.Result{}, err
	}

	// Record source node
	m.Status.SourceNode = sourcePod.Spec.NodeName

	// Resolve container name
	containerName := m.Spec.ContainerName
	if containerName == "" && len(sourcePod.Spec.Containers) > 0 {
		containerName = sourcePod.Spec.Containers[0].Name
	}
	if containerName == "" {
		return r.failMigration(ctx, m, "could not determine container name for source pod")
	}
	m.Status.ContainerName = containerName

	// Initialize timing metadata
	now := metav1.Now()
	m.Status.StartTime = &now
	if m.Status.PhaseTimings == nil {
		m.Status.PhaseTimings = make(map[string]string)
	}

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

	// Re-apply status fields (may have been reset by re-fetch above)
	m.Status.SourceNode = sourcePod.Spec.NodeName
	m.Status.StartTime = &now
	m.Status.ContainerName = containerName
	if m.Status.PhaseTimings == nil {
		m.Status.PhaseTimings = make(map[string]string)
	}

	logger.Info("Pending phase complete",
		"sourceNode", m.Status.SourceNode,
		"strategy", m.Spec.MigrationStrategy)

	return r.transitionPhase(ctx, m, migrationv1alpha1.PhaseCheckpointing)
}

// handleCheckpointing connects to the message broker, creates the secondary
// replay queue for fan-out duplication, and triggers a CRIU checkpoint via
// the kubelet API.
func (r *StatefulMigrationReconciler) handleCheckpointing(ctx context.Context, m *migrationv1alpha1.StatefulMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	phaseStart := time.Now()

	// Connect to the message broker (idempotent if already connected)
	mqCfg := m.Spec.MessageQueueConfig
	if err := r.MsgClient.Connect(ctx, mqCfg.BrokerURL); err != nil {
		return r.failMigration(ctx, m, fmt.Sprintf("broker connect: %v", err))
	}

	// Create the secondary queue for fan-out duplication
	_, err := r.MsgClient.CreateSecondaryQueue(ctx, mqCfg.QueueName, mqCfg.ExchangeName, mqCfg.RoutingKey)
	if err != nil {
		return r.failMigration(ctx, m, fmt.Sprintf("create secondary queue: %v", err))
	}

	// Trigger the CRIU checkpoint through the kubelet proxy API.
	// The checkpoint-transfer job will later pick up the archive from this path.
	if r.KubeletClient != nil {
		resp, err := r.KubeletClient.Checkpoint(
			ctx,
			m.Status.SourceNode,
			m.Namespace,
			m.Spec.SourcePod,
			m.Status.ContainerName,
		)
		if err != nil {
			return r.failMigration(ctx, m, fmt.Sprintf("kubelet checkpoint: %v", err))
		}
		if len(resp.Items) > 0 {
			m.Status.CheckpointID = resp.Items[0]
		}
	} else {
		// Fallback for environments without a real kubelet client (e.g., tests)
		m.Status.CheckpointID = fmt.Sprintf("/var/lib/kubelet/checkpoints/checkpoint-%s.tar", m.Spec.SourcePod)
	}

	r.recordPhaseTiming(m, "Checkpointing", time.Since(phaseStart))
	logger.Info("Checkpointing complete", "checkpointID", m.Status.CheckpointID)

	return r.transitionPhase(ctx, m, migrationv1alpha1.PhaseTransferring)
}

// handleTransferring creates a Kubernetes Job that runs the checkpoint-transfer
// tool on the source node to build an OCI image from the checkpoint archive
// and push it to the configured registry.
func (r *StatefulMigrationReconciler) handleTransferring(ctx context.Context, m *migrationv1alpha1.StatefulMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ensurePhaseTimings(m)
	jobName := m.Name + "-transfer"

	// Check if the Job already exists
	existingJob := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: m.Namespace}, existingJob)

	if errors.IsNotFound(err) {
		// Record the phase start when we first create the job
		if _, ok := m.Status.PhaseTimings["Transferring.start"]; !ok {
			m.Status.PhaseTimings["Transferring.start"] = time.Now().Format(time.RFC3339)
			_ = r.Status().Update(ctx, m)
		}

		// Build the transfer Job spec
		imageRef := fmt.Sprintf("%s/%s:checkpoint", m.Spec.CheckpointImageRepository, m.Spec.SourcePod)
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      jobName,
				Namespace: m.Namespace,
				Labels: map[string]string{
					"migration.ms2m.io/migration": m.Name,
					"migration.ms2m.io/phase":     "transferring",
				},
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(m, migrationv1alpha1.GroupVersion.WithKind("StatefulMigration")),
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

		if err := r.Create(ctx, job); err != nil {
			return r.failMigration(ctx, m, fmt.Sprintf("create transfer job: %v", err))
		}

		logger.Info("Created transfer job", "job", jobName)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil

	} else if err != nil {
		return ctrl.Result{}, err
	}

	// Job exists, check if it completed
	if existingJob.Status.Succeeded >= 1 {
		// Parse the phase start time to calculate duration
		var duration time.Duration
		if startStr, ok := m.Status.PhaseTimings["Transferring.start"]; ok {
			if startTime, err := time.Parse(time.RFC3339, startStr); err == nil {
				duration = time.Since(startTime)
			}
			delete(m.Status.PhaseTimings, "Transferring.start")
		}
		r.recordPhaseTiming(m, "Transferring", duration)
		logger.Info("Transfer job completed", "job", jobName)
		return r.transitionPhase(ctx, m, migrationv1alpha1.PhaseRestoring)
	}

	// Still running
	logger.Info("Waiting for transfer job", "job", jobName)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// handleRestoring creates the target pod on the destination node using the
// checkpoint image. Depending on the migration strategy:
//   - ShadowPod: creates a new pod alongside the source
//   - Sequential: deletes the source first, then creates the target with the same name
func (r *StatefulMigrationReconciler) handleRestoring(ctx context.Context, m *migrationv1alpha1.StatefulMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ensurePhaseTimings(m)

	// Record phase start time (only on first entry)
	if _, ok := m.Status.PhaseTimings["Restoring.start"]; !ok {
		m.Status.PhaseTimings["Restoring.start"] = time.Now().Format(time.RFC3339)
		_ = r.Status().Update(ctx, m)
	}

	// Determine target pod name based on strategy
	var targetPodName string
	if m.Spec.MigrationStrategy == "Sequential" {
		targetPodName = m.Spec.SourcePod
	} else {
		targetPodName = m.Spec.SourcePod + "-shadow"
	}

	// Check if the target pod already exists
	targetPod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: targetPodName, Namespace: m.Namespace}, targetPod)

	if errors.IsNotFound(err) {
		// For Sequential strategy, delete the source pod first
		if m.Spec.MigrationStrategy == "Sequential" {
			sourcePod := &corev1.Pod{}
			srcErr := r.Get(ctx, types.NamespacedName{Name: m.Spec.SourcePod, Namespace: m.Namespace}, sourcePod)
			if srcErr == nil {
				// Source still exists, delete it and wait
				logger.Info("Deleting source pod for Sequential migration", "pod", m.Spec.SourcePod)
				if err := r.Delete(ctx, sourcePod); err != nil {
					return r.failMigration(ctx, m, fmt.Sprintf("delete source pod: %v", err))
				}
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			} else if !errors.IsNotFound(srcErr) {
				return ctrl.Result{}, srcErr
			}
			// Source pod is gone, proceed with creating the target
		}

		// Look up the source pod to copy container spec (for ShadowPod, source still exists)
		sourcePod := &corev1.Pod{}
		if m.Spec.MigrationStrategy != "Sequential" {
			if err := r.Get(ctx, types.NamespacedName{Name: m.Spec.SourcePod, Namespace: m.Namespace}, sourcePod); err != nil {
				return r.failMigration(ctx, m, fmt.Sprintf("source pod lookup for restore: %v", err))
			}
		}

		// Build the target pod spec
		checkpointImage := fmt.Sprintf("%s/%s:checkpoint", m.Spec.CheckpointImageRepository, m.Spec.SourcePod)
		containers := []corev1.Container{
			{
				Name:  m.Status.ContainerName,
				Image: checkpointImage,
			},
		}

		// Build labels - start with migration labels
		labels := map[string]string{
			"migration.ms2m.io/migration": m.Name,
			"migration.ms2m.io/role":      "target",
		}
		// Copy source pod labels (available in ShadowPod strategy)
		if sourcePod.Name != "" {
			for k, v := range sourcePod.Labels {
				if _, exists := labels[k]; !exists {
					labels[k] = v
				}
			}
		}

		newPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      targetPodName,
				Namespace: m.Namespace,
				Labels:    labels,
				Annotations: map[string]string{
					"migration.ms2m.io/checkpoint-image": checkpointImage,
				},
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(m, migrationv1alpha1.GroupVersion.WithKind("StatefulMigration")),
				},
			},
			Spec: corev1.PodSpec{
				NodeName:   m.Spec.TargetNode,
				Containers: containers,
			},
		}

		if err := r.Create(ctx, newPod); err != nil {
			return r.failMigration(ctx, m, fmt.Sprintf("create target pod: %v", err))
		}

		logger.Info("Created target pod", "pod", targetPodName, "node", m.Spec.TargetNode)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil

	} else if err != nil {
		return ctrl.Result{}, err
	}

	// Target pod exists, wait for it to be Running
	if targetPod.Status.Phase != corev1.PodRunning {
		logger.Info("Waiting for target pod to become Running", "pod", targetPodName, "phase", targetPod.Status.Phase)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Target pod is Running, record the result and move on
	m.Status.TargetPod = targetPodName

	var duration time.Duration
	if startStr, ok := m.Status.PhaseTimings["Restoring.start"]; ok {
		if startTime, err := time.Parse(time.RFC3339, startStr); err == nil {
			duration = time.Since(startTime)
		}
		delete(m.Status.PhaseTimings, "Restoring.start")
	}
	r.recordPhaseTiming(m, "Restoring", duration)

	logger.Info("Target pod is Running", "pod", targetPodName)
	return r.transitionPhase(ctx, m, migrationv1alpha1.PhaseReplaying)
}

// handleReplaying sends the START_REPLAY control message and monitors the
// secondary queue depth. Once the queue is drained (or within the cutoff
// threshold), the migration transitions to the Finalizing phase.
func (r *StatefulMigrationReconciler) handleReplaying(ctx context.Context, m *migrationv1alpha1.StatefulMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ensurePhaseTimings(m)

	// Record phase start time (only on first entry)
	if _, ok := m.Status.PhaseTimings["Replaying.start"]; !ok {
		m.Status.PhaseTimings["Replaying.start"] = time.Now().Format(time.RFC3339)

		// Send the START_REPLAY control message on the first pass
		payload := map[string]interface{}{
			"queue": m.Spec.MessageQueueConfig.QueueName + ".ms2m-replay",
		}
		if err := r.MsgClient.SendControlMessage(ctx, m.Status.TargetPod, messaging.ControlStartReplay, payload); err != nil {
			return r.failMigration(ctx, m, fmt.Sprintf("send START_REPLAY: %v", err))
		}
		_ = r.Status().Update(ctx, m)
	}

	// Poll the secondary queue depth
	secondaryQueue := m.Spec.MessageQueueConfig.QueueName + ".ms2m-replay"
	depth, err := r.MsgClient.GetQueueDepth(ctx, secondaryQueue)
	if err != nil {
		return r.failMigration(ctx, m, fmt.Sprintf("get queue depth: %v", err))
	}

	logger.Info("Replay queue depth", "queue", secondaryQueue, "depth", depth)

	if depth == 0 {
		// Queue is fully drained, proceed to finalization
		var duration time.Duration
		if startStr, ok := m.Status.PhaseTimings["Replaying.start"]; ok {
			if startTime, parseErr := time.Parse(time.RFC3339, startStr); parseErr == nil {
				duration = time.Since(startTime)
			}
			delete(m.Status.PhaseTimings, "Replaying.start")
		}
		r.recordPhaseTiming(m, "Replaying", duration)
		return r.transitionPhase(ctx, m, migrationv1alpha1.PhaseFinalizing)
	}

	// Check if we've exceeded the replay cutoff timeout
	if m.Spec.ReplayCutoffSeconds > 0 {
		if startStr, ok := m.Status.PhaseTimings["Replaying.start"]; ok {
			if startTime, parseErr := time.Parse(time.RFC3339, startStr); parseErr == nil {
				elapsed := time.Since(startTime)
				cutoff := time.Duration(m.Spec.ReplayCutoffSeconds) * time.Second
				if elapsed > cutoff {
					logger.Info("Replay cutoff reached, proceeding to finalization",
						"elapsed", elapsed, "cutoff", cutoff, "remainingDepth", depth)
					delete(m.Status.PhaseTimings, "Replaying.start")
					r.recordPhaseTiming(m, "Replaying", elapsed)
					return r.transitionPhase(ctx, m, migrationv1alpha1.PhaseFinalizing)
				}
			}
		}
	}

	// Still draining, poll again
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

// handleFinalizing sends END_REPLAY, tears down the secondary queue, and
// cleans up the source pod (in ShadowPod strategy). After this, the migration
// is marked as Completed.
func (r *StatefulMigrationReconciler) handleFinalizing(ctx context.Context, m *migrationv1alpha1.StatefulMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	phaseStart := time.Now()

	// Send END_REPLAY to signal the target pod to switch to the primary queue
	if err := r.MsgClient.SendControlMessage(ctx, m.Status.TargetPod, messaging.ControlEndReplay, nil); err != nil {
		return r.failMigration(ctx, m, fmt.Sprintf("send END_REPLAY: %v", err))
	}

	// Tear down the secondary queue
	secondaryQueue := m.Spec.MessageQueueConfig.QueueName + ".ms2m-replay"
	if err := r.MsgClient.DeleteSecondaryQueue(ctx, secondaryQueue, m.Spec.MessageQueueConfig.QueueName, m.Spec.MessageQueueConfig.ExchangeName); err != nil {
		logger.Error(err, "Failed to delete secondary queue, continuing anyway")
	}

	// In ShadowPod strategy, the source pod is still around and needs to be removed
	if m.Spec.MigrationStrategy == "ShadowPod" {
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

	// Close the broker connection
	if err := r.MsgClient.Close(); err != nil {
		logger.Error(err, "Failed to close broker connection")
	}

	r.recordPhaseTiming(m, "Finalizing", time.Since(phaseStart))
	logger.Info("Migration finalized successfully")

	return r.transitionPhase(ctx, m, migrationv1alpha1.PhaseCompleted)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// ensurePhaseTimings initializes the PhaseTimings map if it hasn't been created yet.
func ensurePhaseTimings(m *migrationv1alpha1.StatefulMigration) {
	if m.Status.PhaseTimings == nil {
		m.Status.PhaseTimings = make(map[string]string)
	}
}

// recordPhaseTiming stores the duration of a completed phase in the status.
func (r *StatefulMigrationReconciler) recordPhaseTiming(m *migrationv1alpha1.StatefulMigration, phaseName string, duration time.Duration) {
	ensurePhaseTimings(m)
	m.Status.PhaseTimings[phaseName] = duration.Round(time.Millisecond).String()
}

// transitionPhase updates the migration status to the new phase and requeues.
func (r *StatefulMigrationReconciler) transitionPhase(ctx context.Context, m *migrationv1alpha1.StatefulMigration, newPhase migrationv1alpha1.Phase) (ctrl.Result, error) {
	m.Status.Phase = newPhase
	if err := r.Status().Update(ctx, m); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// failMigration moves the migration to the Failed phase with a descriptive reason.
func (r *StatefulMigrationReconciler) failMigration(ctx context.Context, m *migrationv1alpha1.StatefulMigration, reason string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Error(fmt.Errorf("%s", reason), "migration failed")

	m.Status.Phase = migrationv1alpha1.PhaseFailed
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type:               "Failed",
		Status:             metav1.ConditionTrue,
		Reason:             "MigrationFailed",
		Message:            reason,
		LastTransitionTime: metav1.Now(),
	})

	if err := r.Status().Update(ctx, m); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *StatefulMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&migrationv1alpha1.StatefulMigration{}).
		Complete(r)
}
