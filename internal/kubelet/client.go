package kubelet

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// CheckpointResponse holds the result of a kubelet checkpoint API call.
type CheckpointResponse struct {
	Items []string `json:"items"`
}

// Client wraps a Kubernetes REST client to interact with the kubelet
// checkpoint API through the API server proxy.
type Client struct {
	restClient rest.Interface
}

// NewClient creates a new kubelet Client from a Kubernetes clientset.
// It uses the CoreV1 REST client, which provides access to the
// /api/v1/nodes/{node}/proxy/... endpoints.
func NewClient(clientset kubernetes.Interface) *Client {
	return &Client{
		restClient: clientset.CoreV1().RESTClient(),
	}
}

// Checkpoint triggers a container checkpoint via the kubelet API, proxied
// through the Kubernetes API server:
// POST /api/v1/nodes/{node}/proxy/checkpoint/{namespace}/{pod}/{container}
//
// It returns the checkpoint response containing the list of checkpoint
// archive paths on the node filesystem.
func (c *Client) Checkpoint(ctx context.Context, nodeName, namespace, podName, containerName string) (*CheckpointResponse, error) {
	if nodeName == "" || namespace == "" || podName == "" || containerName == "" {
		return nil, fmt.Errorf("all checkpoint parameters are required (node=%q, ns=%q, pod=%q, container=%q)",
			nodeName, namespace, podName, containerName)
	}

	result := c.restClient.Post().
		Resource("nodes").
		Name(nodeName).
		SubResource("proxy", "checkpoint", namespace, podName, containerName).
		Do(ctx)

	if err := result.Error(); err != nil {
		return nil, fmt.Errorf("checkpoint request for container %s/%s/%s on node %s failed: %w",
			namespace, podName, containerName, nodeName, err)
	}

	var statusCode int
	result.StatusCode(&statusCode)
	if statusCode != 200 {
		return nil, fmt.Errorf("checkpoint returned unexpected status %d for %s/%s/%s on %s",
			statusCode, namespace, podName, containerName, nodeName)
	}

	rawBody, err := result.Raw()
	if err != nil {
		return nil, fmt.Errorf("reading checkpoint response body: %w", err)
	}

	var resp CheckpointResponse
	if err := json.Unmarshal(rawBody, &resp); err != nil {
		return nil, fmt.Errorf("parsing checkpoint response: %w", err)
	}

	return &resp, nil
}
