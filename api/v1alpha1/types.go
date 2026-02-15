package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase defines the current stage of the migration
type Phase string

const (
	PhasePending       Phase = "Pending"
	PhaseCheckpointing Phase = "Checkpointing"
	PhaseTransferring  Phase = "Transferring"
	PhaseRestoring     Phase = "Restoring"
	PhaseReplaying     Phase = "Replaying"
	PhaseFinalizing    Phase = "Finalizing"
	PhaseCompleted     Phase = "Completed"
	PhaseFailed        Phase = "Failed"
)

// MessageQueueConfig defines configuration for the message broker
type MessageQueueConfig struct {
	// QueueName is the name of the queue to migrate
	QueueName string `json:"queueName,omitempty"`
	// BrokerURL is the connection string for the message broker
	BrokerURL string `json:"brokerUrl,omitempty"`
	// ExchangeName is the exchange the producer publishes to
	ExchangeName string `json:"exchangeName,omitempty"`
	// RoutingKey is the routing key used for the primary queue binding
	RoutingKey string `json:"routingKey,omitempty"`
}

// StatefulMigrationSpec defines the desired state of StatefulMigration
type StatefulMigrationSpec struct {
	// SourcePod is the name of the pod to migrate (must be in the same namespace)
	SourcePod string `json:"sourcePod,omitempty"`

	// ContainerName is the name of the container to checkpoint.
	// If empty, defaults to the first container in the source pod.
	ContainerName string `json:"containerName,omitempty"`

	// TargetNode is the optional node selector for the target
	TargetNode string `json:"targetNode,omitempty"`

	// CheckpointImageRepository is the registry location to push the checkpoint image
	CheckpointImageRepository string `json:"checkpointImageRepository,omitempty"`

	// ReplayCutoffSeconds is the threshold in seconds to trigger the final cutoff
	ReplayCutoffSeconds int32 `json:"replayCutoffSeconds,omitempty"`

	// MessageQueueConfig contains details about the messaging system
	MessageQueueConfig MessageQueueConfig `json:"messageQueueConfig,omitempty"`

	// MigrationStrategy determines how to handle pod identity conflicts.
	// "ShadowPod" creates a shadow pod alongside the source (for individual pods).
	// "Sequential" deletes source before creating target (required for StatefulSets).
	// If empty, auto-detected from ownerReferences.
	MigrationStrategy string `json:"migrationStrategy,omitempty"`
}

// StatefulMigrationStatus defines the observed state of StatefulMigration
type StatefulMigrationStatus struct {
	// Phase represents the current phase of the migration
	Phase Phase `json:"phase,omitempty"`

	// SourceNode is the node where the source pod is running
	SourceNode string `json:"sourceNode,omitempty"`

	// CheckpointID is the identifier of the created checkpoint
	CheckpointID string `json:"checkpointID,omitempty"`

	// TargetPod is the name of the restored pod
	TargetPod string `json:"targetPod,omitempty"`

	// ContainerName is the resolved container name (from spec or auto-detected)
	ContainerName string `json:"containerName,omitempty"`

	// Conditions represents the latest available observations of the object's state
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// StartTime records when the migration was initiated
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// PhaseTimings records the duration of each completed phase
	PhaseTimings map[string]string `json:"phaseTimings,omitempty"`

	// StatefulSetName is the name of the owning StatefulSet (for Sequential strategy scale-down/up)
	StatefulSetName string `json:"statefulSetName,omitempty"`

	// OriginalReplicas is the StatefulSet's replica count before scale-down
	OriginalReplicas int32 `json:"originalReplicas,omitempty"`

	// SourcePodLabels stores the source pod's labels, captured during Pending phase
	SourcePodLabels map[string]string `json:"sourcePodLabels,omitempty"`

	// SourceContainers stores the source pod's container specs for use during restore
	SourceContainers []corev1.Container `json:"sourceContainers,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// StatefulMigration is the Schema for the statefulmigrations API
type StatefulMigration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StatefulMigrationSpec   `json:"spec,omitempty"`
	Status StatefulMigrationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StatefulMigrationList contains a list of StatefulMigration
type StatefulMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StatefulMigration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StatefulMigration{}, &StatefulMigrationList{})
}
