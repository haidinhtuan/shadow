package controller

import (
	"context"
	"fmt"
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
			patch := client.MergeFrom(m.DeepCopy())
			m.Status.PhaseTimings["Transferring.start"] = time.Now().Format(time.RFC3339)
			_ = r.Status().Patch(ctx, m, patch)
		}

		// Build the transfer Job spec.
		// Determine the transfer destination based on TransferMode.
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

	// Job exists, check if it completed
	if existingJob.Status.Succeeded >= 1 {
		base := m.DeepCopy()
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
		return r.transitionPhase(ctx, m, base, migrationv1alpha1.PhaseRestoring)
	}

	// Still running — use exponential backoff based on elapsed time
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
	var sourcePodSpec *corev1.PodSpec

	if m.Spec.MigrationStrategy != "Sequential" {
		// ShadowPod: source pod is still alive
		sourcePod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Name: m.Spec.SourcePod, Namespace: m.Namespace}, sourcePod); err != nil {
			return r.failMigration(ctx, m, fmt.Sprintf("source pod lookup for restore: %v", err))
		}
		sourceContainers = sourcePod.Spec.Containers
		sourceLabels = sourcePod.Labels
		sourcePodSpec = &sourcePod.Spec
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

	// For ShadowPod strategy, set hostname and subdomain for DNS identity
	if m.Spec.MigrationStrategy == "ShadowPod" && sourcePodSpec != nil {
		newPod.Spec.Hostname = m.Spec.SourcePod
		if sourcePodSpec.Subdomain != "" {
			newPod.Spec.Subdomain = sourcePodSpec.Subdomain
		}
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
func (r *StatefulMigrationReconciler) handleReplaying(ctx context.Context, m *migrationv1alpha1.StatefulMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ensurePhaseTimings(m)

	// Record phase start time (only on first entry)
	if _, ok := m.Status.PhaseTimings["Replaying.start"]; !ok {
		patch := client.MergeFrom(m.DeepCopy())
		m.Status.PhaseTimings["Replaying.start"] = time.Now().Format(time.RFC3339)

		// Send the START_REPLAY control message on the first pass
		payload := map[string]interface{}{
			"queue": m.Spec.MessageQueueConfig.QueueName + ".ms2m-replay",
		}
		if err := r.MsgClient.SendControlMessage(ctx, m.Status.TargetPod, messaging.ControlStartReplay, payload); err != nil {
			return r.failMigration(ctx, m, fmt.Sprintf("send START_REPLAY: %v", err))
		}
		_ = r.Status().Patch(ctx, m, patch)
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
		base := m.DeepCopy()
		var duration time.Duration
		if startStr, ok := m.Status.PhaseTimings["Replaying.start"]; ok {
			if startTime, parseErr := time.Parse(time.RFC3339, startStr); parseErr == nil {
				duration = time.Since(startTime)
			}
			delete(m.Status.PhaseTimings, "Replaying.start")
		}
		r.recordPhaseTiming(m, "Replaying", duration)
		return r.transitionPhase(ctx, m, base, migrationv1alpha1.PhaseFinalizing)
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

// handleFinalizing sends END_REPLAY, tears down the secondary queue, and
// cleans up the source pod (in ShadowPod strategy). After this, the migration
// is marked as Completed.
func (r *StatefulMigrationReconciler) handleFinalizing(ctx context.Context, m *migrationv1alpha1.StatefulMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	base := m.DeepCopy()
	phaseStart := time.Now()

	// Send END_REPLAY and tear down secondary queue.
	// These are best-effort: if the broker channel was already closed (e.g.,
	// a stale reconcile re-entering this handler), skip gracefully.
	if err := r.MsgClient.SendControlMessage(ctx, m.Status.TargetPod, messaging.ControlEndReplay, nil); err != nil {
		logger.Error(err, "Failed to send END_REPLAY, continuing anyway")
	}

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

	// For Sequential strategy, scale the StatefulSet back to its original replica count
	if m.Spec.MigrationStrategy == "Sequential" && m.Status.StatefulSetName != "" && m.Status.OriginalReplicas > 0 {
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

	r.recordPhaseTiming(m, "Finalizing", time.Since(phaseStart))
	logger.Info("Migration finalized successfully")

	// Use direct patch instead of transitionPhase to avoid phase chaining.
	// The informer cache may return a stale "Finalizing" phase after the patch,
	// causing the loop to re-enter handleFinalizing with a nil broker channel.
	m.Status.Phase = migrationv1alpha1.PhaseCompleted
	if err := r.Status().Patch(ctx, m, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
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

// SetupWithManager sets up the controller with the Manager.
func (r *StatefulMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&migrationv1alpha1.StatefulMigration{}).
		Complete(r)
}
