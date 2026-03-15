package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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

// Exchange-Fence protocol constants
const (
	preFenceObservationWindow = 3 * time.Second
	fenceTimeThreshold        = 60.0 // seconds
	parallelDrainStallTimeout = 30 * time.Second
	parallelDrainMaxTimeout   = 120 * time.Second
	parallelDrainPollInterval = 2 * time.Second
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
// Phases that complete synchronously (returning Requeue: true) are chained
// within a single reconcile call to avoid unnecessary round-trips through the
// work queue.
func (r *StatefulMigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the StatefulMigration instance
	migration := &migrationv1alpha1.StatefulMigration{}
	if err := r.Get(ctx, req.NamespacedName, migration); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Phase chaining loop: when a handler returns Requeue: true (no delay),
	// re-fetch and immediately process the next phase instead of returning
	// to the work queue.
	for {
		logger.Info("Reconciling StatefulMigration", "phase", migration.Status.Phase)

		var result ctrl.Result
		var err error

		switch migration.Status.Phase {
		case "":
			// Initial state, move to Pending and requeue
			patch := client.MergeFrom(migration.DeepCopy())
			migration.Status.Phase = migrationv1alpha1.PhasePending
			if err := r.Status().Patch(ctx, migration, patch); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil

		case migrationv1alpha1.PhasePending:
			result, err = r.handlePending(ctx, migration)

		case migrationv1alpha1.PhaseCheckpointing:
			result, err = r.handleCheckpointing(ctx, migration)

		case migrationv1alpha1.PhaseTransferring:
			result, err = r.handleTransferring(ctx, migration)

		case migrationv1alpha1.PhaseRestoring:
			result, err = r.handleRestoring(ctx, migration)

		case migrationv1alpha1.PhaseReplaying:
			result, err = r.handleReplaying(ctx, migration)

		case migrationv1alpha1.PhaseFinalizing:
			result, err = r.handleFinalizing(ctx, migration)

		case migrationv1alpha1.PhaseCompleted, migrationv1alpha1.PhaseFailed:
			return ctrl.Result{}, nil

		default:
			return ctrl.Result{}, nil
		}

		// If error or the handler needs a delayed requeue, return immediately
		if err != nil || result.RequeueAfter > 0 {
			return result, err
		}

		// If no requeue requested, we're done (terminal state or explicit stop)
		if !result.Requeue {
			return result, nil
		}

		// Requeue: true means the phase completed synchronously. Re-fetch
		// the resource to get the updated phase and continue the loop.
		if fetchErr := r.Get(ctx, req.NamespacedName, migration); fetchErr != nil {
			return ctrl.Result{}, client.IgnoreNotFound(fetchErr)
		}
	}
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

	// Ensure source pod is running and not being deleted
	if sourcePod.DeletionTimestamp != nil {
		logger.Info("Source pod is terminating, waiting", "pod", m.Spec.SourcePod)
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
	}
	if sourcePod.Status.Phase != corev1.PodRunning {
		logger.Info("Source pod not Running yet, waiting", "pod", m.Spec.SourcePod, "phase", sourcePod.Status.Phase)
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
	}

	// Resolve container name
	containerName := m.Spec.ContainerName
	if containerName == "" && len(sourcePod.Spec.Containers) > 0 {
		containerName = sourcePod.Spec.Containers[0].Name
	}
	if containerName == "" {
		return r.failMigration(ctx, m, "could not determine container name for source pod")
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

	// Take patch base AFTER any spec update / re-fetch, BEFORE status modifications
	base := m.DeepCopy()

	// Apply all status fields
	m.Status.SourceNode = sourcePod.Spec.NodeName
	m.Status.ContainerName = containerName
	now := metav1.Now()
	m.Status.StartTime = &now
	if m.Status.PhaseTimings == nil {
		m.Status.PhaseTimings = make(map[string]string)
	}

	// Capture source pod labels and containers for use during restore phase
	m.Status.SourcePodLabels = sourcePod.Labels
	m.Status.SourceContainers = sourcePod.Spec.Containers

	// Record the owning StatefulSet or Deployment name from ownerReferences
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

	logger.Info("Pending phase complete",
		"sourceNode", m.Status.SourceNode,
		"strategy", m.Spec.MigrationStrategy)

	return r.transitionPhase(ctx, m, base, migrationv1alpha1.PhaseCheckpointing)
}

// handleCheckpointing connects to the message broker, creates the secondary
// replay queue for fan-out duplication, and triggers a CRIU checkpoint via
// the kubelet API.
func (r *StatefulMigrationReconciler) handleCheckpointing(ctx context.Context, m *migrationv1alpha1.StatefulMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	base := m.DeepCopy()
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

	return r.transitionPhase(ctx, m, base, migrationv1alpha1.PhaseTransferring)
}

// handleTransferring builds an OCI image from the checkpoint archive and pushes
// it to the configured registry. It first tries a direct HTTP call to the
// ms2m-agent DaemonSet on the source node (fast path, no Job overhead). If no
// agent is available, it falls back to creating a Kubernetes Job.
func (r *StatefulMigrationReconciler) handleTransferring(ctx context.Context, m *migrationv1alpha1.StatefulMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ensurePhaseTimings(m)

	// Fast path: direct HTTP call to ms2m-agent on source node.
	// Only for Registry mode — Direct mode uses the checkpoint-transfer Job
	// which POSTs the tar across nodes.
	if m.Spec.TransferMode != "Direct" {
		agentIP, agentErr := r.findAgentPodIP(ctx, m.Status.SourceNode)
		if agentErr == nil {
			base := m.DeepCopy()
			phaseStart := time.Now()

			imageRef := fmt.Sprintf("%s/%s:checkpoint", m.Spec.CheckpointImageRepository, m.Spec.SourcePod)
			if err := r.callAgentRegistryPush(ctx, agentIP, m.Status.CheckpointID, m.Status.ContainerName, imageRef); err != nil {
				return r.failMigration(ctx, m, fmt.Sprintf("agent registry-push: %v", err))
			}

			r.recordPhaseTiming(m, "Transferring", time.Since(phaseStart))
			logger.Info("Transfer complete via agent", "duration", time.Since(phaseStart))
			return r.transitionPhase(ctx, m, base, migrationv1alpha1.PhaseRestoring)
		}
		logger.Info("No ms2m-agent found, falling back to transfer Job", "node", m.Status.SourceNode, "err", agentErr)
	}

	// Fallback: Job-based transfer
	return r.handleTransferringViaJob(ctx, m)
}

// handleTransferringViaJob creates a Kubernetes Job to build and push the
// checkpoint image. Used when the ms2m-agent DaemonSet is not available or
// for Direct transfer mode.
func (r *StatefulMigrationReconciler) handleTransferringViaJob(ctx context.Context, m *migrationv1alpha1.StatefulMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	jobName := m.Name + "-transfer"

	existingJob := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: m.Namespace}, existingJob)

	if errors.IsNotFound(err) {
		if _, ok := m.Status.PhaseTimings["Transferring.start"]; !ok {
			patch := client.MergeFrom(m.DeepCopy())
			m.Status.PhaseTimings["Transferring.start"] = time.Now().Format(time.RFC3339)
			_ = r.Status().Patch(ctx, m, patch)
		}

		var transferArgs []string
		if m.Spec.TransferMode == "Direct" {
			agentURL := fmt.Sprintf("http://ms2m-agent.ms2m-system.svc.cluster.local:9443/checkpoint")
			transferArgs = []string{m.Status.CheckpointID, agentURL, m.Status.ContainerName}
		} else {
			imageRef := fmt.Sprintf("%s/%s:checkpoint", m.Spec.CheckpointImageRepository, m.Spec.SourcePod)
			transferArgs = []string{m.Status.CheckpointID, imageRef, m.Status.ContainerName}
		}

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
								Name:            "checkpoint-transfer",
								Image:           "localhost/checkpoint-transfer:latest",
								ImagePullPolicy: corev1.PullIfNotPresent,
								Args:            transferArgs,
								Env: []corev1.EnvVar{
									{
										Name:  "INSECURE_REGISTRY",
										Value: "true",
									},
								},
								SecurityContext: &corev1.SecurityContext{
									RunAsUser:  func() *int64 { uid := int64(0); return &uid }(),
									RunAsGroup: func() *int64 { gid := int64(0); return &gid }(),
								},
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
			if errors.IsAlreadyExists(err) {
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			return r.failMigration(ctx, m, fmt.Sprintf("create transfer job: %v", err))
		}

		logger.Info("Created transfer job", "job", jobName)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil

	} else if err != nil {
		return ctrl.Result{}, err
	}

	if existingJob.Status.Succeeded >= 1 {
		base := m.DeepCopy()
		var duration time.Duration
		if startStr, ok := m.Status.PhaseTimings["Transferring.start"]; ok {
			if startTime, err := time.Parse(time.RFC3339, startStr); err == nil {
				duration = time.Since(startTime)
			}
			delete(m.Status.PhaseTimings, "Transferring.start")
		}
		r.recordPhaseTiming(m, "Transferring", duration)
		logger.Info("Transfer job completed", "job", jobName)
		return r.transitionPhase(ctx, m, base, migrationv1alpha1.PhaseRestoring)
	}

	logger.Info("Waiting for transfer job", "job", jobName)
	return ctrl.Result{RequeueAfter: r.pollingBackoff(m, "Transferring.start")}, nil
}

// handleRestoring creates the target pod on the destination node using the
// checkpoint image. Depending on the migration strategy:
//   - ShadowPod: creates a new pod alongside the source
//   - Sequential: scales down the StatefulSet, deletes the source, then creates the target
func (r *StatefulMigrationReconciler) handleRestoring(ctx context.Context, m *migrationv1alpha1.StatefulMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ensurePhaseTimings(m)

	// Record phase start time (only on first entry)
	if _, ok := m.Status.PhaseTimings["Restoring.start"]; !ok {
		patch := client.MergeFrom(m.DeepCopy())
		m.Status.PhaseTimings["Restoring.start"] = time.Now().Format(time.RFC3339)
		_ = r.Status().Patch(ctx, m, patch)
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

	if err == nil {
		// For Sequential strategy, the target pod name equals the source pod name.
		// A pod without migration labels was not created by us (it's either the
		// original source pod or a pod recreated by the StatefulSet controller).
		// Scale down the StatefulSet and wait for it to delete the pod.
		if m.Spec.MigrationStrategy == "Sequential" && targetPod.Labels["migration.ms2m.io/migration"] != m.Name {
			if m.Status.OriginalReplicas == 0 && m.Status.StatefulSetName != "" {
				// First time: scale down the StatefulSet so it stops recreating pods
				sts := &appsv1.StatefulSet{}
				stsErr := r.Get(ctx, types.NamespacedName{Name: m.Status.StatefulSetName, Namespace: m.Namespace}, sts)
				if stsErr == nil && sts.Spec.Replicas != nil && *sts.Spec.Replicas > 0 {
					patch := client.MergeFrom(m.DeepCopy())
					m.Status.OriginalReplicas = *sts.Spec.Replicas
					_ = r.Status().Patch(ctx, m, patch)

					newReplicas := int32(0)
					stsPatch := client.MergeFrom(sts.DeepCopy())
					sts.Spec.Replicas = &newReplicas
					if err := r.Patch(ctx, sts, stsPatch); err != nil {
						return r.failMigration(ctx, m, fmt.Sprintf("scale down StatefulSet %q: %v", m.Status.StatefulSetName, err))
					}
					logger.Info("Scaled down StatefulSet", "statefulset", m.Status.StatefulSetName, "replicas", newReplicas)
				} else if stsErr != nil && !errors.IsNotFound(stsErr) {
					return ctrl.Result{}, stsErr
				}
			}
			// Wait for the StatefulSet controller to process the scale-down
			// and delete the pod. Don't delete it ourselves to avoid a race.
			logger.Info("Waiting for source pod to be removed by StatefulSet controller", "pod", targetPodName)
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}

		// If the pod is being deleted (e.g., leftover from a previous migration)
		// or is not owned by this migration, wait for it to be gone so we can
		// create our own.
		if targetPod.DeletionTimestamp != nil || targetPod.Labels["migration.ms2m.io/migration"] != m.Name {
			logger.Info("Waiting for stale target pod to be removed", "pod", targetPodName,
				"owner", targetPod.Labels["migration.ms2m.io/migration"], "deleting", targetPod.DeletionTimestamp != nil)
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		}

		// Target pod exists with correct identity, wait for it to be Running
		if targetPod.Status.Phase != corev1.PodRunning {
			logger.Info("Waiting for target pod to become Running", "pod", targetPodName, "phase", targetPod.Status.Phase)
			return ctrl.Result{RequeueAfter: r.pollingBackoff(m, "Restoring.start")}, nil
		}

		// Target pod is Running, record the result and move on
		base := m.DeepCopy()
		m.Status.TargetPod = targetPodName

		var duration time.Duration
		if startStr, ok := m.Status.PhaseTimings["Restoring.start"]; ok {
			if startTime, parseErr := time.Parse(time.RFC3339, startStr); parseErr == nil {
				duration = time.Since(startTime)
			}
			delete(m.Status.PhaseTimings, "Restoring.start")
		}
		r.recordPhaseTiming(m, "Restoring", duration)

		logger.Info("Target pod is Running", "pod", targetPodName)
		return r.transitionPhase(ctx, m, base, migrationv1alpha1.PhaseReplaying)
	}

	if !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// Gather source pod information for building the target pod.
	// For Sequential, the source was deleted — use data captured during Pending.
	// For ShadowPod, the source is still alive — look it up directly.
	var checkpointImage string
	var pullPolicy corev1.PullPolicy
	if m.Spec.TransferMode == "Direct" {
		checkpointImage = fmt.Sprintf("localhost/checkpoint/%s:latest", m.Status.ContainerName)
		pullPolicy = corev1.PullNever
	} else {
		checkpointImage = fmt.Sprintf("%s/%s:checkpoint", m.Spec.CheckpointImageRepository, m.Spec.SourcePod)
		pullPolicy = corev1.PullAlways
	}
	var sourceContainers []corev1.Container
	var sourceLabels map[string]string

	if m.Spec.MigrationStrategy != "Sequential" {
		// ShadowPod: source pod is still alive
		sourcePod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Name: m.Spec.SourcePod, Namespace: m.Namespace}, sourcePod); err != nil {
			return r.failMigration(ctx, m, fmt.Sprintf("source pod lookup for restore: %v", err))
		}
		sourceContainers = sourcePod.Spec.Containers
		sourceLabels = sourcePod.Labels
	} else if len(m.Status.SourceContainers) > 0 {
		// Sequential: use data captured during Pending phase
		sourceContainers = m.Status.SourceContainers
		sourceLabels = m.Status.SourcePodLabels
	}

	// Build containers: use checkpoint image with ports from source.
	// Only copy ports from source containers — volume mounts, env vars, and
	// other fields are either auto-injected by Kubernetes (kube-api-access)
	// or already baked into the CRIU checkpoint image.
	var containers []corev1.Container
	if len(sourceContainers) > 0 {
		for _, c := range sourceContainers {
			restored := corev1.Container{
				Name:            c.Name,
				Image:           checkpointImage,
				ImagePullPolicy: pullPolicy,
				Ports:           c.Ports,
			}
			if c.Name != m.Status.ContainerName {
				// Non-checkpoint containers keep their original image
				restored.Image = c.Image
			}
			containers = append(containers, restored)
		}
	} else {
		containers = []corev1.Container{
			{
				Name:            m.Status.ContainerName,
				Image:           checkpointImage,
				ImagePullPolicy: pullPolicy,
			},
		}
	}

	// Build labels — migration labels first, then source pod labels
	labels := map[string]string{
		"migration.ms2m.io/migration": m.Name,
		"migration.ms2m.io/role":      "target",
	}
	for k, v := range sourceLabels {
		if _, exists := labels[k]; !exists {
			labels[k] = v
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
		if errors.IsAlreadyExists(err) {
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		}
		return r.failMigration(ctx, m, fmt.Sprintf("create target pod: %v", err))
	}

	logger.Info("Created target pod", "pod", targetPodName, "node", m.Spec.TargetNode)
	return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
}

// handleReplaying sends the START_REPLAY control message and monitors the
// secondary queue depth. Once the queue is drained (or within the cutoff
// threshold), the migration transitions to the Finalizing phase.
//
// Two modes are supported via spec.replayMode:
//   - "Cutoff" (default): secondary queue stays bound to the exchange.
//     A time-based cutoff (replayCutoffSeconds) forces finalization.
//   - "Drain": secondary queue is unbound first (fixed message set).
//     Waits for full drain; fails only if depth stalls for 30s.
func (r *StatefulMigrationReconciler) handleReplaying(ctx context.Context, m *migrationv1alpha1.StatefulMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ensurePhaseTimings(m)

	secondaryQueue := m.Spec.MessageQueueConfig.QueueName + ".ms2m-replay"
	drainMode := m.Spec.ReplayMode == "Drain"

	// Record phase start time (only on first entry)
	if _, ok := m.Status.PhaseTimings["Replaying.start"]; !ok {
		// In Drain mode, unbind the secondary queue first so it has a
		// fixed message set. No new messages arrive after this point.
		if drainMode {
			if err := r.MsgClient.UnbindQueue(ctx, secondaryQueue, m.Spec.MessageQueueConfig.ExchangeName); err != nil {
				logger.Error(err, "Failed to unbind secondary queue, continuing anyway")
			}
		}

		patch := client.MergeFrom(m.DeepCopy())
		m.Status.PhaseTimings["Replaying.start"] = time.Now().Format(time.RFC3339)

		// Send the START_REPLAY control message on the first pass
		payload := map[string]interface{}{
			"queue": secondaryQueue,
		}
		if err := r.MsgClient.SendControlMessage(ctx, m.Status.TargetPod, messaging.ControlStartReplay, payload); err != nil {
			return r.failMigration(ctx, m, fmt.Sprintf("send START_REPLAY: %v", err))
		}
		_ = r.Status().Patch(ctx, m, patch)
	}

	// Poll the secondary queue depth
	depth, err := r.MsgClient.GetQueueDepth(ctx, secondaryQueue)
	if err != nil {
		return r.failMigration(ctx, m, fmt.Sprintf("get queue depth: %v", err))
	}

	logger.Info("Replay queue depth", "queue", secondaryQueue, "depth", depth)

	if depth == 0 {
		// Queue is fully drained, proceed to finalization
		base := m.DeepCopy()
		var duration time.Duration
		if startStr, ok := m.Status.PhaseTimings["Replaying.start"]; ok {
			if startTime, parseErr := time.Parse(time.RFC3339, startStr); parseErr == nil {
				duration = time.Since(startTime)
			}
			delete(m.Status.PhaseTimings, "Replaying.start")
		}
		delete(m.Status.PhaseTimings, "Replaying.lastDepth")
		delete(m.Status.PhaseTimings, "Replaying.lastDecrease")
		r.recordPhaseTiming(m, "Replaying", duration)
		return r.transitionPhase(ctx, m, base, migrationv1alpha1.PhaseFinalizing)
	}

	if drainMode {
		// Stall detection: fail if queue depth hasn't decreased for 30s.
		return r.replayDrainCheck(ctx, m, depth)
	}

	// Cutoff mode: check if we've exceeded the replay cutoff timeout
	if m.Spec.ReplayCutoffSeconds > 0 {
		if startStr, ok := m.Status.PhaseTimings["Replaying.start"]; ok {
			if startTime, parseErr := time.Parse(time.RFC3339, startStr); parseErr == nil {
				elapsed := time.Since(startTime)
				cutoff := time.Duration(m.Spec.ReplayCutoffSeconds) * time.Second
				if elapsed > cutoff {
					logger.Info("Replay cutoff reached, proceeding to finalization",
						"elapsed", elapsed, "cutoff", cutoff, "remainingDepth", depth)
					base := m.DeepCopy()
					delete(m.Status.PhaseTimings, "Replaying.start")
					r.recordPhaseTiming(m, "Replaying", elapsed)
					return r.transitionPhase(ctx, m, base, migrationv1alpha1.PhaseFinalizing)
				}
			}
		}
	}

	// Still draining — use exponential backoff
	return ctrl.Result{RequeueAfter: r.pollingBackoff(m, "Replaying.start")}, nil
}

// replayDrainCheck implements stall detection for Drain replay mode.
// It tracks the last observed queue depth and when it last decreased.
// If the depth hasn't decreased for 30s, the migration fails.
func (r *StatefulMigrationReconciler) replayDrainCheck(ctx context.Context, m *migrationv1alpha1.StatefulMigration, depth int) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	now := time.Now()

	lastDepthStr := m.Status.PhaseTimings["Replaying.lastDepth"]
	lastDecreaseStr := m.Status.PhaseTimings["Replaying.lastDecrease"]

	lastDepth := 0
	if lastDepthStr != "" {
		fmt.Sscanf(lastDepthStr, "%d", &lastDepth)
	}

	depthDecreased := lastDepthStr == "" || depth < lastDepth

	patch := client.MergeFrom(m.DeepCopy())
	m.Status.PhaseTimings["Replaying.lastDepth"] = fmt.Sprintf("%d", depth)

	if depthDecreased {
		m.Status.PhaseTimings["Replaying.lastDecrease"] = now.Format(time.RFC3339)
	} else if lastDecreaseStr != "" {
		// Check stall duration
		if lastDecrease, parseErr := time.Parse(time.RFC3339, lastDecreaseStr); parseErr == nil {
			stalled := now.Sub(lastDecrease)
			if stalled > 30*time.Second {
				logger.Info("Replay stalled, consumer not making progress",
					"stalledFor", stalled, "depth", depth)
				return r.failMigration(ctx, m, fmt.Sprintf("replay stalled: queue depth %d unchanged for %s", depth, stalled.Round(time.Second)))
			}
		}
	}

	_ = r.Status().Patch(ctx, m, patch)

	// Poll every 2s in drain mode (fixed set, no backoff needed)
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

// handleFinalizing sends END_REPLAY, tears down the secondary queue, and
// cleans up the source pod (in ShadowPod strategy). After this, the migration
// is marked as Completed.
func (r *StatefulMigrationReconciler) handleFinalizing(ctx context.Context, m *migrationv1alpha1.StatefulMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	base := m.DeepCopy()
	ensurePhaseTimings(m)

	// Record phase start time (only on first entry)
	if _, ok := m.Status.PhaseTimings["Finalizing.start"]; !ok {
		patch := client.MergeFrom(m.DeepCopy())
		m.Status.PhaseTimings["Finalizing.start"] = time.Now().Format(time.RFC3339)
		_ = r.Status().Patch(ctx, m, patch)
	}

	// Send END_REPLAY and tear down secondary queue on first entry only.
	// Skip during swap sub-phases to avoid destroying the swap queue.
	// These are best-effort: if the broker channel was already closed (e.g.,
	// a stale reconcile re-entering this handler), skip gracefully.
	if m.Status.SwapSubPhase == "" {
		if err := r.MsgClient.SendControlMessage(ctx, m.Status.TargetPod, messaging.ControlEndReplay, nil); err != nil {
			logger.Error(err, "Failed to send END_REPLAY, continuing anyway")
		}

		secondaryQueue := m.Spec.MessageQueueConfig.QueueName + ".ms2m-replay"
		if err := r.MsgClient.DeleteSecondaryQueue(ctx, secondaryQueue, m.Spec.MessageQueueConfig.QueueName, m.Spec.MessageQueueConfig.ExchangeName); err != nil {
			logger.Error(err, "Failed to delete secondary queue, continuing anyway")
		}
	}

	// In ShadowPod strategy, the source pod is still around and needs to be removed.
	// For StatefulSet-owned pods, perform identity swap if requested via IdentitySwapMode.
	if m.Spec.MigrationStrategy == "ShadowPod" {
		swapMode := m.Spec.IdentitySwapMode
		if m.Status.StatefulSetName != "" && swapMode != "" && swapMode != "None" {
			// ShadowPod + StatefulSet + identity swap enabled
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
				logger.Error(err, "Failed to patch Deployment nodeAffinity", "deployment", m.Status.DeploymentName)
			} else {
				logger.Info("Patched Deployment nodeAffinity", "deployment", m.Status.DeploymentName, "targetNode", m.Spec.TargetNode)
			}
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// For Sequential strategy with StatefulSets: remove the StatefulMigration
	// ownerReference from the target pod so the StatefulSet controller can adopt
	// it, then scale the StatefulSet back to its original replica count.
	if m.Spec.MigrationStrategy == "Sequential" && m.Status.StatefulSetName != "" && m.Status.OriginalReplicas > 0 {
		// Remove StatefulMigration ownerRef from target pod to allow adoption
		if m.Status.TargetPod != "" {
			targetPod := &corev1.Pod{}
			if err := r.Get(ctx, types.NamespacedName{Name: m.Status.TargetPod, Namespace: m.Namespace}, targetPod); err == nil {
				filtered := make([]metav1.OwnerReference, 0, len(targetPod.OwnerReferences))
				for _, ref := range targetPod.OwnerReferences {
					if ref.Kind != "StatefulMigration" {
						filtered = append(filtered, ref)
					}
				}
				if len(filtered) != len(targetPod.OwnerReferences) {
					podPatch := client.MergeFrom(targetPod.DeepCopy())
					targetPod.OwnerReferences = filtered
					if err := r.Patch(ctx, targetPod, podPatch); err != nil {
						logger.Error(err, "Failed to remove ownerRef from target pod", "pod", m.Status.TargetPod)
					} else {
						logger.Info("Removed StatefulMigration ownerRef from target pod", "pod", m.Status.TargetPod)
					}
				}
			} else if !errors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}

		// Scale StatefulSet back to original replica count
		sts := &appsv1.StatefulSet{}
		if err := r.Get(ctx, types.NamespacedName{Name: m.Status.StatefulSetName, Namespace: m.Namespace}, sts); err == nil {
			if sts.Spec.Replicas == nil || *sts.Spec.Replicas < m.Status.OriginalReplicas {
				stsPatch := client.MergeFrom(sts.DeepCopy())
				sts.Spec.Replicas = &m.Status.OriginalReplicas
				if err := r.Patch(ctx, sts, stsPatch); err != nil {
					logger.Error(err, "Failed to scale up StatefulSet", "statefulset", m.Status.StatefulSetName)
				} else {
					logger.Info("Scaled up StatefulSet", "statefulset", m.Status.StatefulSetName, "replicas", m.Status.OriginalReplicas)
				}
			}
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// Close the broker connection
	if err := r.MsgClient.Close(); err != nil {
		logger.Error(err, "Failed to close broker connection")
	}

	// Calculate Finalizing duration from the start time recorded on first entry
	var finalizeDuration time.Duration
	if startStr, ok := m.Status.PhaseTimings["Finalizing.start"]; ok {
		if startTime, parseErr := time.Parse(time.RFC3339, startStr); parseErr == nil {
			finalizeDuration = time.Since(startTime)
		}
		delete(m.Status.PhaseTimings, "Finalizing.start")
	}
	r.recordPhaseTiming(m, "Finalizing", finalizeDuration)
	logger.Info("Migration finalized successfully")

	// Use direct patch instead of transitionPhase to avoid phase chaining.
	// The informer cache may return a stale "Finalizing" phase after the patch,
	// causing the loop to re-enter handleFinalizing with a nil broker channel.
	m.Status.Phase = migrationv1alpha1.PhaseCompleted
	m.Status.SwapSubPhase = "" // Ensure swap sub-phase is cleared
	if err := r.Status().Patch(ctx, m, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// ---------------------------------------------------------------------------
// Identity swap sub-phases (ShadowPod + StatefulSet)
// ---------------------------------------------------------------------------

// handleIdentitySwap manages the local identity swap for ShadowPod+StatefulSet
// migrations. It returns (result, done, error) where done=true means the swap
// is complete and the caller should proceed to normal Finalizing completion.
func (r *StatefulMigrationReconciler) handleIdentitySwap(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	// Sub-phase chaining loop: when a sub-phase completes synchronously
	// (Requeue: true, no delay), re-fetch and immediately dispatch the next
	// sub-phase instead of going back through the main reconcile loop.
	for {
		var result ctrl.Result
		var done bool
		var err error

		switch m.Status.SwapSubPhase {
		case "":
			if m.Status.ReplacementPod != "" {
				return ctrl.Result{}, true, nil
			}
			result, done, err = r.handleSwapPrepare(ctx, m, base)
		case "PrepareSwap":
			result, done, err = r.handleSwapPrepare(ctx, m, base)
		case "ReCheckpoint":
			result, done, err = r.handleSwapReCheckpoint(ctx, m, base)
		case "SwapTransfer":
			result, done, err = r.handleSwapTransfer(ctx, m, base)
		case "CreateReplacement":
			result, done, err = r.handleSwapCreateReplacement(ctx, m, base)
		case "MiniReplay":
			result, done, err = r.handleSwapMiniReplay(ctx, m, base)
		case "TrafficSwitch":
			result, done, err = r.handleSwapTrafficSwitch(ctx, m, base)
		// Exchange-Fence sub-phases
		case "PreFenceDrain":
			result, done, err = r.handleSwapPreFenceDrain(ctx, m, base)
		case "ExchangeFence":
			result, done, err = r.handleSwapExchangeFence(ctx, m, base)
		case "ParallelDrain":
			result, done, err = r.handleSwapParallelDrain(ctx, m, base)
		case "FenceCutover":
			result, done, err = r.handleSwapFenceCutover(ctx, m, base)
		default:
			logger.Error(nil, "Unknown swap sub-phase", "subPhase", m.Status.SwapSubPhase)
			return ctrl.Result{}, true, nil
		}

		// Return immediately on error, completion, delayed requeue, or no requeue
		if err != nil || done || result.RequeueAfter > 0 || !result.Requeue {
			return result, done, err
		}

		// Requeue: true means sub-phase completed synchronously.
		// Re-fetch and continue to the next sub-phase.
		key := client.ObjectKeyFromObject(m)
		if fetchErr := r.Get(ctx, key, m); fetchErr != nil {
			return ctrl.Result{}, false, fetchErr
		}
		base = m.DeepCopy()
	}
}

// handleSwapPrepare creates a secondary queue to buffer messages during the swap.
func (r *StatefulMigrationReconciler) handleSwapPrepare(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	mqCfg := m.Spec.MessageQueueConfig

	// Reconnect to broker if needed (Finalizing may have closed it in a previous attempt)
	if err := r.MsgClient.Connect(ctx, mqCfg.BrokerURL); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("broker connect for swap: %w", err)
	}

	// Create a swap-specific secondary queue for buffering during the identity swap
	if _, err := r.MsgClient.CreateSecondaryQueue(ctx, mqCfg.QueueName, mqCfg.ExchangeName, mqCfg.RoutingKey); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("create swap queue: %w", err)
	}

	logger.Info("PrepareSwap complete, swap queue created")

	// Take the patch base BEFORE modifying OriginalReplicas so the status
	// patch below includes both OriginalReplicas and SwapSubPhase.
	patch := client.MergeFrom(m.DeepCopy())

	// Start STS scale-down early so it overlaps with re-checkpoint + transfer.
	// This saves ~7-15s from the accumulation window (less time for messages
	// to accumulate in the swap queue).
	if m.Status.StatefulSetName != "" {
		sts := &appsv1.StatefulSet{}
		if stsErr := r.Get(ctx, types.NamespacedName{Name: m.Status.StatefulSetName, Namespace: m.Namespace}, sts); stsErr == nil {
			if sts.Spec.Replicas != nil && *sts.Spec.Replicas > 0 {
				// Save original replicas before scaling down
				if m.Status.OriginalReplicas == 0 {
					m.Status.OriginalReplicas = *sts.Spec.Replicas
				}

				newReplicas := int32(0)
				stsPatch := client.MergeFrom(sts.DeepCopy())
				sts.Spec.Replicas = &newReplicas

				// Update nodeSelector so STS generates correct ControllerRevision
				if sts.Spec.Template.Spec.NodeSelector == nil {
					sts.Spec.Template.Spec.NodeSelector = make(map[string]string)
				}
				sts.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] = m.Spec.TargetNode

				if patchErr := r.Patch(ctx, sts, stsPatch); patchErr != nil {
					logger.Error(patchErr, "Failed to start early STS scale-down, will retry in CreateReplacement")
				} else {
					logger.Info("Started early STS scale-down in PrepareSwap",
						"statefulset", m.Status.StatefulSetName, "targetNode", m.Spec.TargetNode)

					// Force-delete the source pod immediately with 0s grace to
					// avoid the STS controller's default 30s graceful termination.
					// Remove Service labels first (traffic bridge) so traffic
					// shifts to shadow pod before the pod disappears.
					sourcePod := &corev1.Pod{}
					if getErr := r.Get(ctx, types.NamespacedName{Name: m.Spec.SourcePod, Namespace: m.Namespace}, sourcePod); getErr == nil {
						if sourcePod.Labels != nil {
							podPatch := client.MergeFrom(sourcePod.DeepCopy())
							changed := false
							for k := range m.Status.SourcePodLabels {
								if _, has := sourcePod.Labels[k]; has {
									if k == "controller-revision-hash" ||
										k == "statefulset.kubernetes.io/pod-name" ||
										k == "apps.kubernetes.io/pod-index" {
										continue
									}
									delete(sourcePod.Labels, k)
									changed = true
								}
							}
							if changed {
								if labelErr := r.Patch(ctx, sourcePod, podPatch); labelErr != nil {
									logger.Error(labelErr, "Failed to remove Service labels in PrepareSwap")
								} else {
									logger.Info("Removed Service labels from source pod in PrepareSwap", "pod", m.Spec.SourcePod)
								}
							}
						}

						gracePeriod := int64(0)
						if delErr := r.Delete(ctx, sourcePod, &client.DeleteOptions{
							GracePeriodSeconds: &gracePeriod,
						}); delErr != nil && !errors.IsNotFound(delErr) {
							logger.Error(delErr, "Failed to force-delete source pod in PrepareSwap")
						} else {
							logger.Info("Force-deleted source pod in PrepareSwap", "pod", m.Spec.SourcePod)
						}
					}
				}
			}
		}
	}

	// Transition to ReCheckpoint (patch also persists OriginalReplicas set above)
	m.Status.SwapSubPhase = "ReCheckpoint"
	if err := r.Status().Patch(ctx, m, patch); err != nil {
		return ctrl.Result{}, false, err
	}

	return ctrl.Result{Requeue: true}, false, nil
}

// handleSwapReCheckpoint triggers a CRIU checkpoint on the shadow pod (same node, no transfer).
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
			// Re-checkpoint of CRIU-restored containers fails with CRIU error -52
			// due to reconstructed TCP socket states after restore. This is a known
			// CRIU limitation: checkpointing a process that was itself restored from
			// a CRIU checkpoint produces socket states that CRIU cannot serialize again.
			// Fall back to the original checkpoint image — the replacement pod will
			// replay from the pre-migration state via the swap queue.
			logger.Info("Re-checkpoint failed, falling back to original checkpoint image",
				"error", err.Error())
			patch := client.MergeFrom(m.DeepCopy())
			m.Status.SwapSubPhase = "CreateReplacement"
			m.Status.PhaseTimings["Swap.ReCheckpoint.fallback"] = "true"
			if err := r.Status().Patch(ctx, m, patch); err != nil {
				return ctrl.Result{}, false, err
			}
			logger.Info("Skipping SwapTransfer (using original checkpoint image)")
			return ctrl.Result{Requeue: true}, false, nil
		}
		if len(resp.Items) > 0 {
			patch := client.MergeFrom(m.DeepCopy())
			m.Status.CheckpointID = resp.Items[0]
			m.Status.SwapSubPhase = "SwapTransfer"
			if err := r.Status().Patch(ctx, m, patch); err != nil {
				return ctrl.Result{}, false, err
			}
		}
	} else {
		// Test/dev fallback
		patch := client.MergeFrom(m.DeepCopy())
		m.Status.CheckpointID = fmt.Sprintf("/var/lib/kubelet/checkpoints/checkpoint-%s.tar", m.Status.TargetPod)
		m.Status.SwapSubPhase = "SwapTransfer"
		if err := r.Status().Patch(ctx, m, patch); err != nil {
			return ctrl.Result{}, false, err
		}
	}

	logger.Info("ReCheckpoint complete", "shadowPod", m.Status.TargetPod, "checkpointID", m.Status.CheckpointID)
	return ctrl.Result{Requeue: true}, false, nil
}

// handleSwapTransfer loads the re-checkpoint into the target node's local
// containers-storage. It first tries a direct HTTP call to the ms2m-agent
// DaemonSet on the target node (fast path). Falls back to a Job if no agent
// is available.
func (r *StatefulMigrationReconciler) handleSwapTransfer(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	imageTag := fmt.Sprintf("localhost/checkpoint/%s:recheckpoint", m.Status.ContainerName)

	// Fast path: direct HTTP call to ms2m-agent on target node
	agentIP, agentErr := r.findAgentPodIP(ctx, m.Spec.TargetNode)
	if agentErr == nil {
		if err := r.callAgentLocalLoad(ctx, agentIP, m.Status.CheckpointID, m.Status.ContainerName, imageTag); err != nil {
			return ctrl.Result{}, false, fmt.Errorf("agent local-load: %w", err)
		}

		logger.Info("Swap local-load complete via agent", "imageTag", imageTag)
		patch := client.MergeFrom(m.DeepCopy())
		m.Status.SwapSubPhase = "CreateReplacement"
		if err := r.Status().Patch(ctx, m, patch); err != nil {
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{Requeue: true}, false, nil
	}

	logger.Info("No ms2m-agent found, falling back to swap transfer Job", "node", m.Spec.TargetNode, "err", agentErr)
	return r.handleSwapTransferViaJob(ctx, m, base, imageTag)
}

// handleSwapTransferViaJob creates a Job to load the re-checkpoint into
// containers-storage. Used when the ms2m-agent DaemonSet is not available.
func (r *StatefulMigrationReconciler) handleSwapTransferViaJob(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object, imageTag string) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	jobName := m.Name + "-swap-transfer"

	existingJob := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: m.Namespace}, existingJob)

	if errors.IsNotFound(err) {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      jobName,
				Namespace: m.Namespace,
				Labels: map[string]string{
					"migration.ms2m.io/migration": m.Name,
					"migration.ms2m.io/phase":     "swap-transfer",
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
							"kubernetes.io/hostname": m.Spec.TargetNode,
						},
						Containers: []corev1.Container{
							{
								Name:            "local-load",
								Image:           "localhost/ms2m-agent:latest",
								ImagePullPolicy: corev1.PullIfNotPresent,
								Args:            []string{"local-load", m.Status.CheckpointID, m.Status.ContainerName, imageTag},
								SecurityContext: &corev1.SecurityContext{
									Privileged: func() *bool { b := true; return &b }(),
									RunAsUser:  func() *int64 { uid := int64(0); return &uid }(),
									RunAsGroup: func() *int64 { gid := int64(0); return &gid }(),
								},
								VolumeMounts: []corev1.VolumeMount{
									{
										Name:      "checkpoints",
										MountPath: "/var/lib/kubelet/checkpoints",
										ReadOnly:  true,
									},
									{
										Name:      "containers-storage",
										MountPath: "/var/lib/containers/storage",
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
							{
								Name: "containers-storage",
								VolumeSource: corev1.VolumeSource{
									HostPath: &corev1.HostPathVolumeSource{
										Path: "/var/lib/containers/storage",
									},
								},
							},
						},
					},
				},
			},
		}

		if err := r.Create(ctx, job); err != nil {
			if errors.IsAlreadyExists(err) {
				return ctrl.Result{RequeueAfter: 2 * time.Second}, false, nil
			}
			return ctrl.Result{}, false, fmt.Errorf("create swap transfer job: %w", err)
		}

		logger.Info("Created swap local-load job", "job", jobName, "imageTag", imageTag)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, false, nil
	} else if err != nil {
		return ctrl.Result{}, false, err
	}

	if existingJob.Status.Succeeded >= 1 {
		logger.Info("Swap local-load job completed", "job", jobName)
		patch := client.MergeFrom(m.DeepCopy())
		m.Status.SwapSubPhase = "CreateReplacement"
		if err := r.Status().Patch(ctx, m, patch); err != nil {
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{Requeue: true}, false, nil
	}

	if existingJob.Status.Failed > 0 {
		return ctrl.Result{}, false, fmt.Errorf("swap local-load job %q failed", jobName)
	}

	logger.Info("Waiting for swap local-load job", "job", jobName)
	return ctrl.Result{RequeueAfter: 2 * time.Second}, false, nil
}

// handleSwapCreateReplacement creates a pod with the correct StatefulSet name
// (e.g., consumer-0) from the re-checkpoint image. The pod has no controller
// ownerRef so the StatefulSet can adopt it later.
//
// For ShadowPod+StatefulSet, the original pod is still running on the source node.
// We must scale down the StatefulSet to remove it before creating the replacement
// on the target node.
func (r *StatefulMigrationReconciler) handleSwapCreateReplacement(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	// The replacement pod gets the original StatefulSet pod name
	replacementName := m.Spec.SourcePod // e.g., "consumer-0"

	// Check if a pod with the replacement name already exists
	existing := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: replacementName, Namespace: m.Namespace}, existing)
	if err == nil {
		// Pod exists — distinguish between the original (source node) and a replacement we created (target node)
		if existing.Spec.NodeName == m.Spec.TargetNode {
			// This is the replacement pod we created — wait for it to be Running
			if existing.Status.Phase == corev1.PodRunning {
				patch := client.MergeFrom(m.DeepCopy())
				m.Status.ReplacementPod = replacementName
				// Route based on identity swap mode
				if m.Spec.IdentitySwapMode == "ExchangeFence" {
					m.Status.SwapSubPhase = "PreFenceDrain"
				} else {
					m.Status.SwapSubPhase = "MiniReplay"
				}
				if err := r.Status().Patch(ctx, m, patch); err != nil {
					return ctrl.Result{}, false, err
				}
				return ctrl.Result{Requeue: true}, false, nil
			}
			// Still starting up
			logger.Info("Waiting for replacement pod to become Running", "pod", replacementName, "phase", existing.Status.Phase)
			return ctrl.Result{RequeueAfter: 1 * time.Second}, false, nil
		}

		// This is the original pod on the source node — scale down the StatefulSet
		// to remove it. Also update the nodeSelector NOW so that the STS creates
		// its ControllerRevision for the target node before we create the
		// replacement pod. This prevents a revision hash mismatch that would
		// trigger a rolling update after adoption.
		if m.Status.StatefulSetName != "" {
			sts := &appsv1.StatefulSet{}
			stsErr := r.Get(ctx, types.NamespacedName{Name: m.Status.StatefulSetName, Namespace: m.Namespace}, sts)
			if stsErr == nil && sts.Spec.Replicas != nil && *sts.Spec.Replicas > 0 {
				// Save original replicas before scaling down
				if m.Status.OriginalReplicas == 0 {
					patch := client.MergeFrom(m.DeepCopy())
					m.Status.OriginalReplicas = *sts.Spec.Replicas
					_ = r.Status().Patch(ctx, m, patch)
				}

				newReplicas := int32(0)
				stsPatch := client.MergeFrom(sts.DeepCopy())
				sts.Spec.Replicas = &newReplicas

				// Update nodeSelector in the same patch so the STS
				// generates the correct ControllerRevision immediately.
				if sts.Spec.Template.Spec.NodeSelector == nil {
					sts.Spec.Template.Spec.NodeSelector = make(map[string]string)
				}
				sts.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] = m.Spec.TargetNode

				if err := r.Patch(ctx, sts, stsPatch); err != nil {
					return ctrl.Result{}, false, fmt.Errorf("scale down StatefulSet %q for identity swap: %w", m.Status.StatefulSetName, err)
				}
				logger.Info("Scaled down StatefulSet and updated nodeSelector for identity swap",
					"statefulset", m.Status.StatefulSetName, "targetNode", m.Spec.TargetNode)
			} else if stsErr != nil && !errors.IsNotFound(stsErr) {
				return ctrl.Result{}, false, stsErr
			}
		}

		// Remove Service-matching labels from the original pod BEFORE
		// deleting it. This removes the pod from the Service endpoints,
		// so DNS resolves only to the shadow pod's IP. Traffic seamlessly
		// shifts to the shadow pod with no gap.
		if existing.Labels != nil {
			podPatch := client.MergeFrom(existing.DeepCopy())
			changed := false
			for k := range m.Status.SourcePodLabels {
				if _, has := existing.Labels[k]; has {
					// Keep StatefulSet-internal labels — only remove
					// labels that could match a Service selector.
					if k == "controller-revision-hash" ||
						k == "statefulset.kubernetes.io/pod-name" ||
						k == "apps.kubernetes.io/pod-index" {
						continue
					}
					delete(existing.Labels, k)
					changed = true
				}
			}
			if changed {
				if patchErr := r.Patch(ctx, existing, podPatch); patchErr != nil {
					logger.Error(patchErr, "Failed to remove Service labels from original pod")
				} else {
					logger.Info("Removed Service labels from original pod for traffic drain", "pod", replacementName)
				}
			}
		}

		// Force-delete the original pod with a short grace period.
		// Its state is already re-checkpointed, so graceful shutdown
		// adds no value and the default 30s grace period dominates
		// the identity swap duration.
		gracePeriod := int64(0)
		if delErr := r.Delete(ctx, existing, &client.DeleteOptions{
			GracePeriodSeconds: &gracePeriod,
		}); delErr != nil && !errors.IsNotFound(delErr) {
			logger.Error(delErr, "Failed to force-delete original pod", "pod", replacementName)
		}

		logger.Info("Waiting for original pod to terminate", "pod", replacementName, "node", existing.Spec.NodeName)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, false, nil
	}
	if !errors.IsNotFound(err) {
		return ctrl.Result{}, false, err
	}

	// Use the re-checkpoint image if SwapTransfer ran, otherwise fall back
	// to the original checkpoint image from the registry.
	var checkpointImage string
	var pullPolicy corev1.PullPolicy
	if m.Status.PhaseTimings["Swap.ReCheckpoint.fallback"] == "true" {
		checkpointImage = fmt.Sprintf("%s/%s:checkpoint", m.Spec.CheckpointImageRepository, m.Spec.SourcePod)
		pullPolicy = corev1.PullAlways
	} else {
		checkpointImage = fmt.Sprintf("localhost/checkpoint/%s:recheckpoint", m.Status.ContainerName)
		pullPolicy = corev1.PullNever
	}

	// Build containers from source containers captured during Pending.
	// Set MS2M_RESTORE_MODE=true so the CRIU-restored process blocks on the
	// primary queue until START_REPLAY arrives. Without this, the replacement
	// pod immediately reconnects to primary after restore, racing with the
	// shadow pod and causing duplicate consumption.
	restoreModeEnv := corev1.EnvVar{Name: "MS2M_RESTORE_MODE", Value: "true"}
	var containers []corev1.Container
	if len(m.Status.SourceContainers) > 0 {
		for _, c := range m.Status.SourceContainers {
			restored := corev1.Container{
				Name:            c.Name,
				Image:           checkpointImage,
				ImagePullPolicy: pullPolicy,
				Ports:           c.Ports,
				Env:             append(c.Env, restoreModeEnv),
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
			Env:             []corev1.EnvVar{restoreModeEnv},
		}}
	}

	// Build labels from source pod labels (for Service routing + StatefulSet adoption)
	// Do NOT include migration labels — this pod should look like a normal StatefulSet pod
	labels := make(map[string]string)
	for k, v := range m.Status.SourcePodLabels {
		labels[k] = v
	}

	// Use the STS's current updateRevision as the controller-revision-hash so
	// the StatefulSet doesn't trigger a rolling update after adopting this pod.
	// The nodeSelector was already updated during scale-down, so updateRevision
	// reflects the target-node template.
	if m.Status.StatefulSetName != "" {
		sts := &appsv1.StatefulSet{}
		if stsErr := r.Get(ctx, types.NamespacedName{Name: m.Status.StatefulSetName, Namespace: m.Namespace}, sts); stsErr == nil {
			if sts.Status.UpdateRevision != "" {
				labels["controller-revision-hash"] = sts.Status.UpdateRevision
				logger.Info("Set replacement pod revision hash from STS", "revision", sts.Status.UpdateRevision)
			}
		}
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

	// Scale the StatefulSet back up. The nodeSelector was already updated
	// during scale-down, so we only need to restore the replica count.
	// Without scale-up, the STS (at replicas=0) won't adopt our replacement.
	if m.Status.StatefulSetName != "" {
		targetReplicas := m.Status.OriginalReplicas
		if targetReplicas == 0 {
			targetReplicas = 1 // fallback: at least 1 replica to adopt the replacement
		}
		sts := &appsv1.StatefulSet{}
		if stsErr := r.Get(ctx, types.NamespacedName{Name: m.Status.StatefulSetName, Namespace: m.Namespace}, sts); stsErr == nil {
			if sts.Spec.Replicas == nil || *sts.Spec.Replicas < targetReplicas {
				stsPatch := client.MergeFrom(sts.DeepCopy())
				sts.Spec.Replicas = &targetReplicas
				if patchErr := r.Patch(ctx, sts, stsPatch); patchErr != nil {
					logger.Error(patchErr, "Failed to scale up StatefulSet after creating replacement")
				} else {
					logger.Info("Scaled up StatefulSet for identity swap", "statefulset", m.Status.StatefulSetName, "replicas", targetReplicas)
				}
			}
		}
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

// handleSwapMiniReplay sends START_REPLAY to the replacement pod and monitors
// the swap queue depth. Once drained, transitions to TrafficSwitch.
func (r *StatefulMigrationReconciler) handleSwapMiniReplay(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	ensurePhaseTimings(m)

	swapQueue := m.Spec.MessageQueueConfig.QueueName + ".ms2m-replay"

	// Send START_REPLAY on first entry (track with a phase timing key)
	if _, ok := m.Status.PhaseTimings["Swap.MiniReplay.start"]; !ok {
		// Unbind the swap queue from the exchange BEFORE starting replay.
		// This stops new messages from arriving so the queue has a fixed
		// set of messages to drain (only those buffered during re-checkpoint
		// + transfer + create replacement). Without this, the queue grows
		// indefinitely at high message rates and hits the cutoff timer.
		if err := r.MsgClient.UnbindQueue(ctx, swapQueue, m.Spec.MessageQueueConfig.ExchangeName); err != nil {
			logger.Error(err, "Failed to unbind swap queue, continuing anyway")
		}

		patch := client.MergeFrom(m.DeepCopy())
		m.Status.PhaseTimings["Swap.MiniReplay.start"] = time.Now().Format(time.RFC3339)

		payload := map[string]interface{}{
			"queue": swapQueue,
		}
		if err := r.MsgClient.SendControlMessage(ctx, m.Status.ReplacementPod, messaging.ControlStartReplay, payload); err != nil {
			return ctrl.Result{}, false, fmt.Errorf("send START_REPLAY to replacement: %w", err)
		}
		_ = r.Status().Patch(ctx, m, patch)
	}

	// Poll the swap queue depth
	depth, err := r.MsgClient.GetQueueDepth(ctx, swapQueue)
	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("get swap queue depth: %w", err)
	}

	// Short cutoff for MiniReplay: the queue is already unbound (fixed
	// message set), so this only guards against consumer stalls. 15s is
	// enough to drain the ~10s of buffered messages; if the consumer
	// can't keep up, the main Replay cutoff already accepted that.
	cutoff := 15 * time.Second
	var elapsed time.Duration
	if startStr, ok := m.Status.PhaseTimings["Swap.MiniReplay.start"]; ok {
		if startTime, parseErr := time.Parse(time.RFC3339, startStr); parseErr == nil {
			elapsed = time.Since(startTime)
		}
	}

	logger.Info("Swap replay queue depth", "queue", swapQueue, "depth", depth, "elapsed", elapsed.Round(time.Millisecond))

	if depth == 0 || elapsed > cutoff {
		if depth > 0 {
			logger.Info("MiniReplay cutoff reached, proceeding with remaining messages", "depth", depth, "elapsed", elapsed)
		}
		// Queue drained or cutoff — transition to TrafficSwitch
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

// handleSwapTrafficSwitch deletes the shadow pod, sends END_REPLAY to the
// replacement, scales up the StatefulSet for adoption, and cleans up the swap queue.
func (r *StatefulMigrationReconciler) handleSwapTrafficSwitch(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	// Send END_REPLAY to the replacement pod
	if err := r.MsgClient.SendControlMessage(ctx, m.Status.ReplacementPod, messaging.ControlEndReplay, nil); err != nil {
		logger.Error(err, "Failed to send END_REPLAY to replacement pod, continuing anyway")
	}

	// Delete the swap secondary queue
	swapQueue := m.Spec.MessageQueueConfig.QueueName + ".ms2m-replay"
	if err := r.MsgClient.DeleteSecondaryQueue(ctx, swapQueue, m.Spec.MessageQueueConfig.QueueName, m.Spec.MessageQueueConfig.ExchangeName); err != nil {
		logger.Error(err, "Failed to delete swap queue, continuing anyway")
	}

	// Force-delete the shadow pod with a short grace period so that a
	// subsequent migration doesn't wait for the default 30s termination.
	shadowPod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: m.Status.TargetPod, Namespace: m.Namespace}, shadowPod); err == nil {
		gracePeriod := int64(0)
		if err := r.Delete(ctx, shadowPod, &client.DeleteOptions{
			GracePeriodSeconds: &gracePeriod,
		}); err != nil {
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

// ---------------------------------------------------------------------------
// Exchange-Fence Convergence sub-phases
// ---------------------------------------------------------------------------

// handleSwapPreFenceDrain starts pre-fence consumption of the swap queue by
// the replacement pod. It monitors queue depth to measure R_in (publish rate)
// and R_out (consumption rate), then uses the adaptive decision function to
// choose Exchange-Fence or fall back to Cutoff (MiniReplay).
//
// Adaptive strategy: estimate fence drain time as
//
//	T_fence ≈ max(D_swap, D_primary) / R_net   where R_net = R_out - R_in
//
// If R_in ≥ R_out (ρ ≥ 1) or T_fence > 60s, fall back to Cutoff.
func (r *StatefulMigrationReconciler) handleSwapPreFenceDrain(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	ensurePhaseTimings(m)

	mqCfg := m.Spec.MessageQueueConfig
	swapQueue := mqCfg.QueueName + ".ms2m-replay"

	// Send START_REPLAY and record initial depth on first entry
	if _, ok := m.Status.PhaseTimings["Swap.PreFence.start"]; !ok {
		// Get initial swap queue depth before consumption starts
		initialDepth, err := r.MsgClient.GetQueueDepth(ctx, swapQueue)
		if err != nil {
			return ctrl.Result{}, false, fmt.Errorf("get initial swap depth: %w", err)
		}

		payload := map[string]interface{}{
			"queue": swapQueue,
		}
		if err := r.MsgClient.SendControlMessage(ctx, m.Status.ReplacementPod, messaging.ControlStartReplay, payload); err != nil {
			return ctrl.Result{}, false, fmt.Errorf("send START_REPLAY for pre-fence: %w", err)
		}

		patch := client.MergeFrom(m.DeepCopy())
		m.Status.PhaseTimings["Swap.PreFence.start"] = time.Now().Format(time.RFC3339)
		m.Status.PhaseTimings["Swap.PreFence.initialDepth"] = fmt.Sprintf("%d", initialDepth)
		_ = r.Status().Patch(ctx, m, patch)
	}

	// Measure current swap queue depth
	currentDepth, err := r.MsgClient.GetQueueDepth(ctx, swapQueue)
	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("get swap queue depth for pre-fence: %w", err)
	}

	var elapsed time.Duration
	if startStr, ok := m.Status.PhaseTimings["Swap.PreFence.start"]; ok {
		if startTime, parseErr := time.Parse(time.RFC3339, startStr); parseErr == nil {
			elapsed = time.Since(startTime)
		}
	}

	logger.Info("PreFenceDrain status", "queue", swapQueue, "depth", currentDepth, "elapsed", elapsed.Round(time.Millisecond))

	// Wait at least the observation window to collect meaningful rate samples
	if elapsed < preFenceObservationWindow {
		return ctrl.Result{RequeueAfter: 1 * time.Second}, false, nil
	}

	// Adaptive decision: estimate R_in and R_out from depth change.
	//
	// Over the observation window:
	//   - Consumer removed some messages (R_out × t)
	//   - Producer added some messages (R_in × t)
	//   - Net change: currentDepth - initialDepth = (R_in - R_out) × t
	//   - If depth decreased: R_out > R_in (good for fence)
	//   - If depth increased: R_in > R_out (fence would take too long)
	initialDepth := 0
	if depthStr, ok := m.Status.PhaseTimings["Swap.PreFence.initialDepth"]; ok {
		fmt.Sscanf(depthStr, "%d", &initialDepth)
	}

	elapsedSec := elapsed.Seconds()
	useFence := true
	var reason string

	if elapsedSec > 0 {
		// Net drain rate: positive means queue is shrinking
		netDrainRate := float64(initialDepth-currentDepth) / elapsedSec

		// Also get primary queue depth for fence time estimation
		primaryDepth, _ := r.MsgClient.GetQueueDepth(ctx, mqCfg.QueueName)

		maxDepth := primaryDepth
		if currentDepth > maxDepth {
			maxDepth = currentDepth
		}

		if netDrainRate <= 0 {
			// Queue growing or flat: R_in ≥ R_out (ρ ≥ 1)
			useFence = false
			reason = fmt.Sprintf("queue not draining (net rate %.1f msg/s)", netDrainRate)
		} else if maxDepth > 0 {
			// Estimate fence drain time
			estimatedFenceTime := float64(maxDepth) / netDrainRate
			if estimatedFenceTime > fenceTimeThreshold {
				useFence = false
				reason = fmt.Sprintf("estimated fence time %.0fs > %.0fs threshold", estimatedFenceTime, fenceTimeThreshold)
			} else {
				reason = fmt.Sprintf("estimated fence time %.1fs (net drain %.1f msg/s)", estimatedFenceTime, netDrainRate)
			}
		}

		logger.Info("Adaptive decision",
			"initialDepth", initialDepth, "currentDepth", currentDepth,
			"primaryDepth", primaryDepth, "netDrainRate", netDrainRate,
			"useFence", useFence, "reason", reason)
	}

	patch := client.MergeFrom(m.DeepCopy())
	delete(m.Status.PhaseTimings, "Swap.PreFence.start")
	delete(m.Status.PhaseTimings, "Swap.PreFence.initialDepth")

	if useFence {
		m.Status.SwapSubPhase = "ExchangeFence"
		logger.Info("Adaptive: proceeding with Exchange-Fence", "reason", reason)
	} else {
		// Fall back to Cutoff: unbind swap queue and use MiniReplay.
		// Set MiniReplay.start so handleSwapMiniReplay skips its init block
		// (START_REPLAY was already sent and swap queue will be unbound here).
		if unbindErr := r.MsgClient.UnbindQueue(ctx, swapQueue, mqCfg.ExchangeName); unbindErr != nil {
			logger.Error(unbindErr, "Failed to unbind swap queue for Cutoff fallback")
		}
		m.Status.PhaseTimings["Swap.MiniReplay.start"] = time.Now().Format(time.RFC3339)
		m.Status.SwapSubPhase = "MiniReplay"
		logger.Info("Adaptive: falling back to MiniReplay (Cutoff)", "reason", reason)
	}

	if err := r.Status().Patch(ctx, m, patch); err != nil {
		return ctrl.Result{}, false, err
	}

	return ctrl.Result{Requeue: true}, false, nil
}

// handleSwapExchangeFence performs the atomic topology change:
// 1. Create and bind a buffer queue to catch post-fence messages
// 2. Unbind primary queue from exchange (shadow gets no new messages)
// 3. Unbind swap queue from exchange (replacement gets no new messages)
// Both queues now have a finite message set to drain.
func (r *StatefulMigrationReconciler) handleSwapExchangeFence(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	ensurePhaseTimings(m)

	mqCfg := m.Spec.MessageQueueConfig
	swapQueue := mqCfg.QueueName + ".ms2m-replay"
	bufferQueue := mqCfg.QueueName + ".ms2m-fence-buffer"

	// Guard: if fence was already applied (e.g. controller restarted mid-fence),
	// skip straight to recording depths and transitioning to ParallelDrain.
	if _, alreadyFenced := m.Status.PhaseTimings["Swap.Fence.time"]; alreadyFenced {
		logger.Info("Exchange-Fence: fence already applied, skipping to ParallelDrain")
		patch := client.MergeFrom(m.DeepCopy())
		m.Status.SwapSubPhase = "ParallelDrain"
		if err := r.Status().Patch(ctx, m, patch); err != nil {
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{Requeue: true}, false, nil
	}

	// Get current depths before fence for timeout estimation
	primaryDepth, err := r.MsgClient.GetQueueDepth(ctx, mqCfg.QueueName)
	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("get primary depth before fence: %w", err)
	}
	swapDepth, err := r.MsgClient.GetQueueDepth(ctx, swapQueue)
	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("get swap depth before fence: %w", err)
	}

	logger.Info("Exchange-Fence: pre-fence depths", "primaryDepth", primaryDepth, "swapDepth", swapDepth)

	// Step 1: Create buffer queue and bind to exchange.
	// The buffer catches all messages published after the fence.
	// DeclareAndBindQueue is idempotent in RabbitMQ (safe to re-call).
	if err := r.MsgClient.DeclareAndBindQueue(ctx, bufferQueue, mqCfg.ExchangeName); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("create buffer queue: %w", err)
	}

	// Step 2: Unbind primary queue from exchange.
	// UnbindQueue on an already-unbound queue is a no-op in RabbitMQ.
	if err := r.MsgClient.UnbindQueue(ctx, mqCfg.QueueName, mqCfg.ExchangeName); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("unbind primary queue for fence: %w", err)
	}

	// Step 3: Unbind swap queue from exchange
	if err := r.MsgClient.UnbindQueue(ctx, swapQueue, mqCfg.ExchangeName); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("unbind swap queue for fence: %w", err)
	}

	logger.Info("Exchange-Fence: topology change complete",
		"primaryUnbound", mqCfg.QueueName,
		"swapUnbound", swapQueue,
		"bufferBound", bufferQueue)

	// Record fence time and depths for parallel drain timeout
	patch := client.MergeFrom(m.DeepCopy())
	m.Status.PhaseTimings["Swap.Fence.time"] = time.Now().Format(time.RFC3339)
	m.Status.PhaseTimings["Swap.Fence.primaryDepth"] = fmt.Sprintf("%d", primaryDepth)
	m.Status.PhaseTimings["Swap.Fence.swapDepth"] = fmt.Sprintf("%d", swapDepth)
	m.Status.SwapSubPhase = "ParallelDrain"
	if err := r.Status().Patch(ctx, m, patch); err != nil {
		return ctrl.Result{}, false, err
	}

	return ctrl.Result{Requeue: true}, false, nil
}

// handleSwapParallelDrain waits for both the shadow (primary queue) and
// replacement (swap queue) to drain their finite message sets to zero.
// Uses timeout and stall detection to handle failures.
func (r *StatefulMigrationReconciler) handleSwapParallelDrain(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	ensurePhaseTimings(m)

	mqCfg := m.Spec.MessageQueueConfig
	swapQueue := mqCfg.QueueName + ".ms2m-replay"

	// Get both queue depths (ready + unacked for correctness)
	primaryReady, primaryUnacked, err := r.MsgClient.GetQueueStats(ctx, mqCfg.QueueName)
	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("get primary queue stats: %w", err)
	}
	swapReady, swapUnacked, err := r.MsgClient.GetQueueStats(ctx, swapQueue)
	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("get swap queue stats: %w", err)
	}

	primaryTotal := primaryReady + primaryUnacked
	swapTotal := swapReady + swapUnacked

	// Elapsed time since fence
	var elapsed time.Duration
	if fenceStr, ok := m.Status.PhaseTimings["Swap.Fence.time"]; ok {
		if fenceTime, parseErr := time.Parse(time.RFC3339, fenceStr); parseErr == nil {
			elapsed = time.Since(fenceTime)
		}
	}

	logger.Info("ParallelDrain status",
		"primaryTotal", primaryTotal, "swapTotal", swapTotal,
		"elapsed", elapsed.Round(time.Millisecond))

	// Swap queue must be fully drained before deletion; primary queue is
	// never deleted so the replacement pod will consume it naturally.
	if swapTotal == 0 {
		logger.Info("ParallelDrain complete — swap queue drained",
			"primaryRemaining", primaryTotal)
		patch := client.MergeFrom(m.DeepCopy())
		delete(m.Status.PhaseTimings, "Swap.Fence.time")
		delete(m.Status.PhaseTimings, "Swap.Fence.primaryDepth")
		delete(m.Status.PhaseTimings, "Swap.Fence.swapDepth")
		m.Status.SwapSubPhase = "FenceCutover"
		if err := r.Status().Patch(ctx, m, patch); err != nil {
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{Requeue: true}, false, nil
	}

	// Stall detection: if depth hasn't changed for the stall timeout, fail the drain
	lastDepthKey := "Swap.ParallelDrain.lastDepth"
	lastCheckKey := "Swap.ParallelDrain.lastCheck"
	currentDepthStr := fmt.Sprintf("%d,%d", primaryTotal, swapTotal)

	if lastDepth, ok := m.Status.PhaseTimings[lastDepthKey]; ok {
		if lastDepth == currentDepthStr {
			// Depth hasn't changed — check how long
			if lastCheckStr, ok2 := m.Status.PhaseTimings[lastCheckKey]; ok2 {
				if lastCheck, parseErr := time.Parse(time.RFC3339, lastCheckStr); parseErr == nil {
					if time.Since(lastCheck) > parallelDrainStallTimeout {
						logger.Error(nil, "ParallelDrain stalled — depth unchanged",
							"primaryTotal", primaryTotal, "swapTotal", swapTotal)
						// Clean up stall tracking
						delete(m.Status.PhaseTimings, lastDepthKey)
						delete(m.Status.PhaseTimings, lastCheckKey)
						return r.handleSwapFenceRollback(ctx, m, base, "parallel drain stalled")
					}
				}
			}
		} else {
			// Depth changed — reset stall timer
			patch := client.MergeFrom(m.DeepCopy())
			m.Status.PhaseTimings[lastDepthKey] = currentDepthStr
			m.Status.PhaseTimings[lastCheckKey] = time.Now().Format(time.RFC3339)
			_ = r.Status().Patch(ctx, m, patch)
		}
	} else {
		// First check — initialize stall tracking
		patch := client.MergeFrom(m.DeepCopy())
		m.Status.PhaseTimings[lastDepthKey] = currentDepthStr
		m.Status.PhaseTimings[lastCheckKey] = time.Now().Format(time.RFC3339)
		_ = r.Status().Patch(ctx, m, patch)
	}

	// Timeout: max duration for parallel drain
	if elapsed > parallelDrainMaxTimeout {
		logger.Error(nil, "ParallelDrain timeout", "elapsed", elapsed)
		return r.handleSwapFenceRollback(ctx, m, base, "parallel drain timeout")
	}

	return ctrl.Result{RequeueAfter: parallelDrainPollInterval}, false, nil
}

// handleSwapFenceCutover completes the Exchange-Fence protocol:
// 1. Kill shadow pod (it has drained its primary queue)
// 2. Rebind primary queue to exchange (restore normal routing)
// 3. Replacement drains buffer queue (post-fence messages)
// 4. Delete buffer + swap queues, send END_REPLAY
func (r *StatefulMigrationReconciler) handleSwapFenceCutover(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	ensurePhaseTimings(m)

	mqCfg := m.Spec.MessageQueueConfig
	swapQueue := mqCfg.QueueName + ".ms2m-replay"
	bufferQueue := mqCfg.QueueName + ".ms2m-fence-buffer"

	// Step 1: Kill the shadow pod (it has fully drained primary)
	shadowPod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: m.Status.TargetPod, Namespace: m.Namespace}, shadowPod); err == nil {
		gracePeriod := int64(0)
		if err := r.Delete(ctx, shadowPod, &client.DeleteOptions{
			GracePeriodSeconds: &gracePeriod,
		}); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "Failed to delete shadow pod during fence cutover", "pod", m.Status.TargetPod)
		} else {
			logger.Info("Deleted shadow pod (drained primary)", "pod", m.Status.TargetPod)
		}
	}

	// Step 2: Rebind primary queue to exchange (restore normal message flow).
	// Step 3: Then unbind buffer queue from exchange.
	// Order matters: rebind-then-unbind means both are briefly bound (possible
	// duplicates in buffer), but avoids message loss. Unbind-then-rebind would
	// lose messages published in the gap. At-least-once is preferable.
	if err := r.MsgClient.BindQueue(ctx, mqCfg.QueueName, mqCfg.ExchangeName, ""); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("rebind primary queue: %w", err)
	}

	if err := r.MsgClient.UnbindQueue(ctx, bufferQueue, mqCfg.ExchangeName); err != nil {
		logger.Error(err, "Failed to unbind buffer queue, continuing anyway")
	}

	// Step 4: Check if buffer queue has messages to drain
	bufferReady, bufferUnacked, err := r.MsgClient.GetQueueStats(ctx, bufferQueue)
	if err != nil {
		// Buffer queue may not exist if fence was fast — not an error
		logger.Info("Buffer queue not accessible, treating as empty", "err", err)
		bufferReady, bufferUnacked = 0, 0
	}

	if bufferReady+bufferUnacked > 0 {
		// Tell replacement to drain buffer queue before switching to primary
		if _, ok := m.Status.PhaseTimings["Swap.BufferDrain.start"]; !ok {
			payload := map[string]interface{}{
				"queue": bufferQueue,
			}
			if err := r.MsgClient.SendControlMessage(ctx, m.Status.ReplacementPod, messaging.ControlStartReplay, payload); err != nil {
				logger.Error(err, "Failed to send START_REPLAY for buffer drain")
			}
			patch := client.MergeFrom(m.DeepCopy())
			m.Status.PhaseTimings["Swap.BufferDrain.start"] = time.Now().Format(time.RFC3339)
			_ = r.Status().Patch(ctx, m, patch)
		}

		// Timeout: buffer queue should be small; fail if draining takes > 30s
		if startStr, ok := m.Status.PhaseTimings["Swap.BufferDrain.start"]; ok {
			if startTime, err := time.Parse(time.RFC3339, startStr); err == nil {
				if time.Since(startTime) > parallelDrainStallTimeout {
					logger.Error(nil, "Buffer drain timeout — proceeding without full drain",
						"bufferReady", bufferReady, "bufferUnacked", bufferUnacked)
					// Don't block forever; proceed to END_REPLAY. Buffer messages
					// will be lost but the migration can complete.
				} else {
					logger.Info("Draining buffer queue", "bufferReady", bufferReady, "bufferUnacked", bufferUnacked)
					return ctrl.Result{RequeueAfter: 1 * time.Second}, false, nil
				}
			}
		} else {
			logger.Info("Draining buffer queue", "bufferReady", bufferReady, "bufferUnacked", bufferUnacked)
			return ctrl.Result{RequeueAfter: 1 * time.Second}, false, nil
		}
	}

	// Buffer drained — send END_REPLAY and clean up
	if err := r.MsgClient.SendControlMessage(ctx, m.Status.ReplacementPod, messaging.ControlEndReplay, nil); err != nil {
		logger.Error(err, "Failed to send END_REPLAY to replacement pod")
	}

	// Delete swap and buffer queues
	if err := r.MsgClient.DeleteSecondaryQueue(ctx, swapQueue, mqCfg.QueueName, mqCfg.ExchangeName); err != nil {
		logger.Error(err, "Failed to delete swap queue during fence cleanup")
	}
	if err := r.MsgClient.DeleteQueue(ctx, bufferQueue); err != nil {
		logger.Error(err, "Failed to delete buffer queue during fence cleanup")
	}

	// Scale up StatefulSet for adoption
	if m.Status.OriginalReplicas > 0 {
		sts := &appsv1.StatefulSet{}
		if err := r.Get(ctx, types.NamespacedName{Name: m.Status.StatefulSetName, Namespace: m.Namespace}, sts); err == nil {
			if sts.Spec.Replicas == nil || *sts.Spec.Replicas < m.Status.OriginalReplicas {
				stsPatch := client.MergeFrom(sts.DeepCopy())
				sts.Spec.Replicas = &m.Status.OriginalReplicas
				if err := r.Patch(ctx, sts, stsPatch); err != nil {
					logger.Error(err, "Failed to scale up StatefulSet", "statefulset", m.Status.StatefulSetName)
				}
			}
		}
	}

	// Update TargetPod and clear swap state
	patch := client.MergeFrom(m.DeepCopy())
	m.Status.TargetPod = m.Status.ReplacementPod
	m.Status.SwapSubPhase = ""
	delete(m.Status.PhaseTimings, "Swap.BufferDrain.start")
	if err := r.Status().Patch(ctx, m, patch); err != nil {
		return ctrl.Result{}, false, err
	}

	logger.Info("FenceCutover complete — Exchange-Fence identity swap finished", "replacementPod", m.Status.ReplacementPod)
	return ctrl.Result{}, true, nil
}

// handleSwapFenceRollback is called when the Exchange-Fence fails (stall or
// timeout during ParallelDrain). It restores normal message routing and falls
// back to the Cutoff path.
func (r *StatefulMigrationReconciler) handleSwapFenceRollback(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object, reason string) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	mqCfg := m.Spec.MessageQueueConfig
	bufferQueue := mqCfg.QueueName + ".ms2m-fence-buffer"
	swapQueue := mqCfg.QueueName + ".ms2m-replay"

	logger.Info("Exchange-Fence rollback", "reason", reason)

	// Rebind primary queue to restore live service
	if err := r.MsgClient.BindQueue(ctx, mqCfg.QueueName, mqCfg.ExchangeName, ""); err != nil {
		logger.Error(err, "Rollback: failed to rebind primary queue")
	}

	// Rebind swap queue so replacement can continue receiving
	if err := r.MsgClient.BindQueue(ctx, swapQueue, mqCfg.ExchangeName, ""); err != nil {
		logger.Error(err, "Rollback: failed to rebind swap queue")
	}

	// Clean up buffer queue. Warn if messages accumulated during the fence
	// window — these will be lost when the buffer is deleted. The primary
	// queue is now rebound, so new messages flow normally after rollback.
	if depth, depthErr := r.MsgClient.GetQueueDepth(ctx, bufferQueue); depthErr == nil && depth > 0 {
		logger.Error(nil, "Rollback: buffer queue has messages that will be lost",
			"bufferQueue", bufferQueue, "depth", depth)
	}
	if err := r.MsgClient.UnbindQueue(ctx, bufferQueue, mqCfg.ExchangeName); err != nil {
		logger.Error(err, "Rollback: failed to unbind buffer queue")
	}
	if err := r.MsgClient.DeleteQueue(ctx, bufferQueue); err != nil {
		logger.Error(err, "Rollback: failed to delete buffer queue")
	}

	// Fall back to Cutoff-style MiniReplay. The swap queue was rebound above;
	// MiniReplay will unbind it again for its drain approach.
	// Set MiniReplay.start so that handleSwapMiniReplay skips sending a
	// duplicate START_REPLAY (one was already sent in PreFenceDrain).
	patch := client.MergeFrom(m.DeepCopy())
	// Clean up fence tracking state
	delete(m.Status.PhaseTimings, "Swap.Fence.time")
	delete(m.Status.PhaseTimings, "Swap.Fence.primaryDepth")
	delete(m.Status.PhaseTimings, "Swap.Fence.swapDepth")
	delete(m.Status.PhaseTimings, "Swap.ParallelDrain.lastDepth")
	delete(m.Status.PhaseTimings, "Swap.ParallelDrain.lastCheck")
	m.Status.PhaseTimings["Swap.MiniReplay.start"] = time.Now().Format(time.RFC3339)
	m.Status.SwapSubPhase = "MiniReplay"
	if err := r.Status().Patch(ctx, m, patch); err != nil {
		return ctrl.Result{}, false, err
	}

	logger.Info("Exchange-Fence rolled back, falling back to MiniReplay (Cutoff)")
	return ctrl.Result{Requeue: true}, false, nil
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

// transitionPhase updates the migration status to the new phase using a merge
// patch and requeues. The base must be a DeepCopy taken BEFORE any in-memory
// status modifications so the patch diff includes all handler changes.
func (r *StatefulMigrationReconciler) transitionPhase(ctx context.Context, m *migrationv1alpha1.StatefulMigration, base client.Object, newPhase migrationv1alpha1.Phase) (ctrl.Result, error) {
	m.Status.Phase = newPhase
	if err := r.Status().Patch(ctx, m, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// failMigration moves the migration to the Failed phase with a descriptive reason.
func (r *StatefulMigrationReconciler) failMigration(ctx context.Context, m *migrationv1alpha1.StatefulMigration, reason string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Error(fmt.Errorf("%s", reason), "migration failed")

	patch := client.MergeFrom(m.DeepCopy())
	m.Status.Phase = migrationv1alpha1.PhaseFailed
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type:               "Failed",
		Status:             metav1.ConditionTrue,
		Reason:             "MigrationFailed",
		Message:            reason,
		LastTransitionTime: metav1.Now(),
	})

	if err := r.Status().Patch(ctx, m, patch); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// pollingBackoff computes an exponential backoff interval based on elapsed
// time since the phase started. Returns 1s initially, doubling up to 5s max.
func (r *StatefulMigrationReconciler) pollingBackoff(m *migrationv1alpha1.StatefulMigration, startKey string) time.Duration {
	const (
		minInterval = 1 * time.Second
		maxInterval = 5 * time.Second
	)
	if startStr, ok := m.Status.PhaseTimings[startKey]; ok {
		if startTime, err := time.Parse(time.RFC3339, startStr); err == nil {
			elapsed := time.Since(startTime)
			switch {
			case elapsed < 10*time.Second:
				return minInterval
			case elapsed < 30*time.Second:
				return 2 * time.Second
			default:
				return maxInterval
			}
		}
	}
	return minInterval
}

// ---------------------------------------------------------------------------
// ms2m-agent DaemonSet HTTP helpers
// ---------------------------------------------------------------------------

// findAgentPodIP looks up the ms2m-agent DaemonSet pod running on the given
// node and returns its pod IP. Returns an error if no running agent is found.
func (r *StatefulMigrationReconciler) findAgentPodIP(ctx context.Context, nodeName string) (string, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace("ms2m-system"),
		client.MatchingLabels{"app": "ms2m-agent"},
	); err != nil {
		return "", fmt.Errorf("list agent pods: %w", err)
	}

	for _, p := range podList.Items {
		if p.Spec.NodeName == nodeName && p.Status.Phase == corev1.PodRunning && p.Status.PodIP != "" {
			return p.Status.PodIP, nil
		}
	}
	return "", fmt.Errorf("no running ms2m-agent found on node %s", nodeName)
}

// callAgentRegistryPush calls the ms2m-agent's /registry-push endpoint to
// build an OCI image from the checkpoint tar and push it to the registry.
func (r *StatefulMigrationReconciler) callAgentRegistryPush(ctx context.Context, agentIP, tarPath, containerName, imageRef string) error {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"tarPath":       tarPath,
		"containerName": containerName,
		"imageRef":      imageRef,
		"insecure":      true,
	})
	return r.callAgent(ctx, agentIP, "/registry-push", reqBody)
}

// callAgentLocalLoad calls the ms2m-agent's /local-load endpoint to build
// an OCI image from the checkpoint tar and load it into containers-storage.
func (r *StatefulMigrationReconciler) callAgentLocalLoad(ctx context.Context, agentIP, tarPath, containerName, imageTag string) error {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"tarPath":       tarPath,
		"containerName": containerName,
		"imageTag":      imageTag,
	})
	return r.callAgent(ctx, agentIP, "/local-load", reqBody)
}

// callAgent makes a POST request to the ms2m-agent at the given IP and path.
func (r *StatefulMigrationReconciler) callAgent(ctx context.Context, agentIP, path string, body []byte) error {
	url := fmt.Sprintf("http://%s:9443%s", agentIP, path)

	httpCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(httpCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP call to agent %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agent returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *StatefulMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&migrationv1alpha1.StatefulMigration{}).
		Complete(r)
}
