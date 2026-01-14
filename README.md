# Kubernetes Controller Demo

This repository contains a simple Kubernetes Controller written in Go, along with Terraform code to provision a GKE cluster.

## Prerequisites

- Go 1.25+
- Docker
- Terraform
- Google Cloud SDK (`gcloud`)

## Project Structure

- `main.go`: The source code for the controller.
- `Dockerfile`: Instructions to build the container image.
- `terraform/`: Terraform configuration to provision GKE.
- `manifests/`: Kubernetes manifests (Deployment, RBAC).

## Why MS2M? (Overcoming StatefulSet Limitations)

Standard Kubernetes `StatefulSets` handle stable identities and storage but fail to preserve **execution state** (RAM) during rescheduling. When a Pod is moved (e.g., node drain, scaling), it is terminated and restarted, leading to:
1.  **Memory Loss**: All in-memory data is wiped.
2.  **Downtime**: The application performs a cold boot and must rebuild cache.
3.  **Connection Drops**: Active connections are severed without coordination.

This controller implements the **Message-based Stateful Microservice Migration (MS2M)** framework to solve these issues:

| Feature | Standard StatefulSet | MS2M Controller |
| :--- | :--- | :--- |
| **Migration Mechanism** | Delete & Recreate (Destructive) | Checkpoint & Restore (Preservative) |
| **In-Memory State** | Lost | **Preserved** via CRIU (Forensic Container Checkpointing) |
| **Transfer Method** | Detach/Attach PV (Slow) | **OCI Image** (Fast, Portable) |
| **Coexistence** | Impossible (Name Collision) | **Shadow Pods** (Target runs as `pod-xyz-shadow`) |
| **Data Consistency** | Crash Recovery | **Message Replay** (Zero-Loss Handoff) |

### The "Two Instances" Problem
A major limitation of StatefulSets is that `app-0` cannot exist on two nodes simultaneously. MS2M overcomes this using a **Shadow Pod Strategy**:
1.  **Source**: `app-0` (Running on Node A).
2.  **Target**: Controller creates a standalone pod named `app-0-shadow` (on Node B).
3.  **Sync**: `app-0-shadow` restores memory and replays messages while `app-0` is frozen.
4.  **Switch**: Once synchronized, traffic is routed to `app-0-shadow`, and `app-0` is terminated.

## Local Development (Running locally)

You can run the controller locally against a remote cluster or a local cluster (like kind or minikube).

1. Ensure your `~/.kube/config` is pointing to the correct cluster.
2. Run the controller:
   ```bash
   go run main.go
   ```

## Deploying to GKE

### 1. Prerequisites & Tooling

Before starting, ensure you have the following installed on your local machine:
- **Google Cloud SDK (`gcloud`)**: [Installation Guide](https://cloud.google.com/sdk/docs/install)
- **Terraform**: [Installation Guide](https://developer.hashicorp.com/terraform/downloads)
- **kubectl**: Install via `gcloud components install kubectl`

### 2. Google Cloud Platform Setup

1.  **Create a Project**: Create a new project in the [Google Cloud Console](https://console.cloud.google.com/).
2.  **Enable Billing**: Ensure billing is active for the project.
3.  **Enable APIs**: Enable the Kubernetes Engine API:
    ```bash
    gcloud services enable container.googleapis.com
    ```
4.  **Authenticate**:
    ```bash
    gcloud auth login
    gcloud auth application-default login
    ```

### 3. Provision Infrastructure (Terraform)

The Terraform configuration is optimized for MS2M, using `UBUNTU_CONTAINERD` nodes to support CRIU checkpointing.

1.  **Navigate to the terraform directory**:
    ```bash
    cd terraform
    ```
2.  **Initialize and Apply**:
    ```bash
    export PROJECT_ID="your-project-id"
    terraform init
    terraform apply -var="project_id=$PROJECT_ID"
    ```
3.  **Connect to the Cluster**:
    ```bash
    gcloud container clusters get-credentials my-k8s-controller-cluster --region us-central1-a
    ```
4.  **Verify Nodes**:
    ```bash
    kubectl get nodes
    ```

### 4. Build and Push Image

Build the Docker image and push it to Google Container Registry (GCR) or Artifact Registry.

```bash
export PROJECT_ID=YOUR_PROJECT_ID
docker build -t gcr.io/$PROJECT_ID/k8s-controller:latest .
docker push gcr.io/$PROJECT_ID/k8s-controller:latest
```

### 3. Deploy Controller

Update `manifests/deployment.yaml` with your image name (`gcr.io/YOUR_PROJECT_ID/k8s-controller:latest`).

Apply the manifests:

```bash
kubectl apply -f manifests/rbac.yaml
kubectl apply -f manifests/deployment.yaml
```

### 4. Verify

Check the logs of the controller:

```bash
kubectl get pods
kubectl logs -f deployment/k8s-controller
```

You should see output indicating the number of pods in the cluster.

---

## MS2M Controller Implementation Roadmap

This project implements the Message-based Stateful Microservice Migration (MS2M) framework. Below is the high-level plan for implementation and testing.

### 1. Implementation Planning

The development is divided into sequential phases to ensure stability:

*   **Phase 1: Foundation (Completed)**
    *   Setup Go module, SDKs, and Project Scaffolding.
    *   Define `StatefulMigration` CRD (v1alpha1).
    *   Implement the base Reconciler loop and state machine skeleton.

*   **Phase 2: Checkpoint Orchestration**
    *   **Goal**: Interface with the Kubelet Checkpoint API.
    *   **Tasks**:
        *   Implement `KubeletClient` to send `POST /checkpoint` requests.
        *   Add logic to `handlePending` to trigger checkpointing.
        *   Verify checkpoint file existence on the source node.

*   **Phase 3: State Transfer & Restoration**
    *   **Goal**: Move the checkpoint from Node A to Node B via a Registry.
    *   **Tasks**:
        *   Implement an ephemeral "Transfer Job" (Pod) that builds an OCI image from the checkpoint tarball.
        *   Update Controller to launch this Job and monitor its completion.
        *   Implement logic to create the Target Pod using the new Checkpoint Image.

*   **Phase 4: Message Replay & Switchover**
    *   **Goal**: Zero-loss synchronization.
    *   **Tasks**:
        *   Integrate RabbitMQ/NATS client to handle queue creation and switching.
        *   Implement `START_REPLAY` and `END_REPLAY` control signals.
        *   Add "Cutoff" logic: Stop source pod if replay lag > threshold.

### 2. Testing Strategy

We employ a pyramid testing approach:

*   **Unit Tests (Fast Feedback)**
    *   Focus: State Machine logic.
    *   Tools: `go test`, `gomock`.
    *   What: Verify that the controller transitions correctly between phases (e.g., `Pending` -> `Checkpointing`) and handles error states gracefully.

*   **Integration Tests (API Logic)**
    *   Focus: CRD interaction.
    *   Tools: `envtest` (controller-runtime).
    *   What: Verify the Controller can successfully create/update/patch the Custom Resources on a real (simulated) API server.

*   **End-to-End (E2E) Tests (Real World)**
    *   Focus: Full Migration Success.
    *   Environment: GKE Cluster with `UBUNTU_CONTAINERD` nodes (for CRIU support).
    *   Scenario:
        1.  Deploy a stateful counter app.
        2.  Generate high-frequency traffic.
        3.  Trigger Migration.
        4.  **Assert**: No messages lost, Target Pod resumes count exactly where Source left off.