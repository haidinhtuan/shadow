# Plan: MS2M Kubernetes Migration Controller

This plan outlines the steps to implement the **Migration Manager** controller for the Message-based Stateful Microservice Migration (MS2M) framework.

## 1. Project Initialization
- [x] Initialize Go module: `go mod init <module-name>`
- [x] Install dependencies:
    - `sigs.k8s.io/controller-runtime`
    - `k8s.io/client-go`
    - `k8s.io/api`
    - `k8s.io/apimachinery`
    - Message Broker Client (e.g., `github.com/rabbitmq/amqp091-go` or NATS client)

## 2. API Definition (CRD: `StatefulMigration`)
- [x] Define `StatefulMigration` Custom Resource:
    - **Spec**:
        - `SourcePod` (Name, Namespace)
        - `TargetNode` (Optional node selector)
        - `CheckpointImageRepository` (Registry to push checkpoint image)
        - `ReplayCutoffSeconds` (Threshold-based cutoff time)
        - `MessageQueueConfig` (Queue names, Broker URL)
    - **Status**:
        - `Phase` (Pending, Checkpointing, Transferring, Restoring, Replaying, Finalizing, Completed, Failed)
        - `SourceNode`
        - `CheckpointID`
        - `TargetPodName`
        - Conditions (Standard K8s conditions)

## 3. Controller Logic (Migration Manager)
The Reconciler will manage the state machine moving through the 5 phases:

### Phase 1: Checkpoint Creation
- [ ] **Action**:
    - Trigger message buffering (Create Secondary Queue).
    - Call Kubelet Checkpoint API: `POST /api/v1/nodes/{node}/proxy/checkpoint/...`
- [ ] **Validation**: Verify checkpoint tarball exists on Source Node (or success response).

### Phase 2: Checkpoint Transfer
- [ ] **Action**:
    - *Note: This requires execution on the Node.*
    - Launch a "Transfer Job" (Privileged Pod) or use a DaemonSet agent on the Source Node to:
        - Take the checkpoint file.
        - Build OCI Image (`buildah` or similar).
        - Push to `CheckpointImageRepository`.
- [ ] **Validation**: Verify Image exists in Registry.

### Phase 3: Service Restoration
- [ ] **Action**:
    - Create Target Pod manifest.
    - Add Annotation for checkpoint restoration (dependant on container runtime/CRIU integration).
    - Schedule on Target Node.
- [ ] **Validation**: Wait for Target Pod to be `Running`.

### Phase 4: Message Replay
- [ ] **Action**:
    - Send `START_REPLAY` control message to Target Pod.
    - Monitor lag/queue depth.
    - **Logic**: If lag > `ReplayCutoffSeconds` -> Stop Source Pod (Freeze).
- [ ] **Validation**: Wait for Target Pod to signal catch-up.

### Phase 5: Finalization
- [ ] **Action**:
    - Send `END_REPLAY` control message.
    - Switch Target Pod to Primary Queue (Live Traffic).
    - Delete/Terminate Source Pod.
- [ ] **Validation**: Migration marked `Completed`.

## 4. Components & Interfaces
- [ ] **Kubelet Client**: Wrapper for interacting with the Checkpoint API.
- [ ] **Messaging Client**: Interface for Queue switching and Control Messages.
- [ ] **Registry Client**: Helper to check image existence (optional).

## 5. Deployment Manifests
- [x] **RBAC**:
    - `nodes/proxy` (For checkpoint API).
    - `pods/create`, `pods/delete`, `pods/exec`.
- [x] **CRD**: `statefulmigrations.mydomain.io`.
- [x] **Manager Deployment**: The Controller Pod.

## 6. Build & Test
- [x] `Dockerfile` for Controller.
- [x] `Makefile`.
- [ ] Unit Tests for State Machine logic.

---

## 7. Infrastructure Setup (GKE)

To test the `StatefulMigration` controller, we need a Kubernetes cluster. 

**Note on Checkpointing (CRIU)**: Forensic Container Checkpointing (FCC) is an alpha feature. Standard GKE nodes may not have CRIU installed or the feature enabled. For a production-like test, you may need custom Ubuntu nodes.

### Step 1: Prerequisites
- [ ] Install Google Cloud CLI (`gcloud`).
- [ ] Install `kubectl`.
- [ ] Install `jq` (optional, for JSON processing).

### Step 2: Create GKE Cluster
We will create a standard zonal cluster.
```bash
# Set variables
export PROJECT_ID="your-project-id"
export CLUSTER_NAME="ms2m-test-cluster"
export ZONE="us-central1-a"

# Create Cluster (Ubuntu image is preferred for easier CRIU installation if needed)
gcloud container clusters create $CLUSTER_NAME \
    --project $PROJECT_ID \
    --zone $ZONE \
    --image-type "UBUNTU_CONTAINERD" \
    --num-nodes 3 \
    --machine-type "e2-standard-4"
```

### Step 3: Configure `kubectl`
```bash
gcloud container clusters get-credentials $CLUSTER_NAME --zone $ZONE --project $PROJECT_ID
```

### Step 4: Install Dependencies (On Cluster)
- [ ] **RabbitMQ / Message Broker**:
    ```bash
    helm repo add bitnami https://charts.bitnami.com/bitnami
    helm install rabbitmq bitnami/rabbitmq
    ```

## 8. Testing Strategy

### 8.1 Unit Testing
- **Scope**: Internal state machine logic.
- **Tools**: Go `testing` package, `gomock`.
- **Cases**:
    - Test state transitions (e.g., Checkpointing -> Transferring).
    - Test error handling (e.g., Checkpoint API failure -> Retry/Fail).
    - Test cutoff logic (e.g., Lag > ReplayCutoffSeconds -> Freeze Source).

### 8.2 Integration Testing
- **Scope**: Controller interaction with Kubernetes API Server.
- **Tools**: `envtest` (controller-runtime).
- **Cases**:
    - Create `StatefulMigration` CR -> Verify Status becomes `Pending`.
    - Manually update Status to `Checkpointing` -> Verify Controller reacts.

### 8.3 End-to-End (E2E) Testing
- **Scope**: Full migration flow on GKE.
- **Tools**: `chainsaw` or Go test suite.
- **Scenario**: "Live Migration of Counter App".
    1.  Deploy a "Counter" StatefulSet (writes to RabbitMQ + memory state).
    2.  Start generating traffic (100 msg/sec).
    3.  Create `StatefulMigration` CR targeting `counter-0`.
    4.  **Verify**:
        - Source Pod is paused/checkpointed.
        - Checkpoint image appears in Registry.
        - Target Pod starts.
        - Messages are replayed.
        - Source Pod terminates.
        - **NO Data Loss**: Final counter value matches sent messages.

## 9. Evaluation Criteria

We will evaluate the success of the implementation based on the following metrics:

### 9.1 Functional Correctness
- [ ] **Data Integrity**: Zero lost messages during migration.
- [ ] **State Preservation**: The target pod resumes with the exact memory state of the source.
- [ ] **Cleanup**: Old resources (Source Pod) are removed correctly.

### 9.2 Performance Metrics
- [ ] **Total Migration Time**: Time from `CreationTimestamp` to `PhaseCompleted`.
- [ ] **Downtime (Write Availability)**: Duration where the application cannot accept *new* write requests.
    - *Goal*: Minimize this via the "Replay" phase.
- [ ] **Checkpoint Size**: Size of the transferred checkpoint image.

### 9.3 Reliability
- [ ] **Failure Recovery**: If a phase fails (e.g., Registry down), the controller should retry or fail gracefully without corrupting the state.
