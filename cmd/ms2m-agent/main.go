package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/haidinhtuan/kubernetes-controller/internal/checkpoint"
)

type checkpointHandler struct {
	storageDir string
	skipLoad   bool // for testing: skip skopeo load
}

func (h *checkpointHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseMultipartForm(500 << 20); err != nil {
		http.Error(w, fmt.Sprintf("parse form: %v", err), http.StatusBadRequest)
		return
	}

	file, _, err := r.FormFile("checkpoint")
	if err != nil {
		http.Error(w, fmt.Sprintf("get file: %v", err), http.StatusBadRequest)
		return
	}
	defer file.Close()

	containerName := r.FormValue("containerName")

	// Write tar to local storage
	tarPath := filepath.Join(h.storageDir, fmt.Sprintf("checkpoint-%d.tar", time.Now().UnixNano()))
	out, err := os.Create(tarPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("create file: %v", err), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		http.Error(w, fmt.Sprintf("write file: %v", err), http.StatusInternalServerError)
		return
	}
	out.Close()

	fmt.Printf("Received checkpoint tar: %s (%s)\n", tarPath, containerName)

	// Build OCI image from tar
	img, err := checkpoint.BuildCheckpointImage(tarPath, containerName)
	if err != nil {
		http.Error(w, fmt.Sprintf("build image: %v", err), http.StatusInternalServerError)
		return
	}

	// Save as OCI layout for skopeo to load
	layoutDir := tarPath + "-oci"
	if err := os.MkdirAll(layoutDir, 0755); err != nil {
		http.Error(w, fmt.Sprintf("mkdir: %v", err), http.StatusInternalServerError)
		return
	}

	p, err := layout.Write(layoutDir, empty.Index)
	if err != nil {
		http.Error(w, fmt.Sprintf("write layout: %v", err), http.StatusInternalServerError)
		return
	}

	if err := p.AppendImage(img); err != nil {
		http.Error(w, fmt.Sprintf("append image: %v", err), http.StatusInternalServerError)
		return
	}

	if !h.skipLoad {
		imageTag := fmt.Sprintf("localhost/checkpoint/%s:latest", containerName)
		cmd := exec.Command("skopeo", "copy",
			"oci:"+layoutDir,
			"containers-storage:"+imageTag)
		output, err := cmd.CombinedOutput()
		if err != nil {
			http.Error(w, fmt.Sprintf("skopeo copy: %v: %s", err, output), http.StatusInternalServerError)
			return
		}
		fmt.Printf("Loaded image into CRI-O: %s\n", imageTag)
	}

	// Cleanup
	os.Remove(tarPath)
	os.RemoveAll(layoutDir)

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "checkpoint loaded successfully")
}

// localLoad builds an OCI image from a checkpoint tar and loads it directly
// into the node's containers-storage via skopeo. No network transfer needed.
func localLoad(tarPath, containerName, imageTag string) error {
	fmt.Printf("Local load: building OCI image from %s\n", tarPath)

	img, err := checkpoint.BuildCheckpointImage(tarPath, containerName)
	if err != nil {
		return fmt.Errorf("build image: %w", err)
	}

	layoutDir, err := os.MkdirTemp("", "swap-oci-*")
	if err != nil {
		return fmt.Errorf("mkdirtemp: %w", err)
	}
	defer os.RemoveAll(layoutDir)

	p, err := layout.Write(layoutDir, empty.Index)
	if err != nil {
		return fmt.Errorf("write layout: %w", err)
	}

	if err := p.AppendImage(img); err != nil {
		return fmt.Errorf("append image: %w", err)
	}

	cmd := exec.Command("skopeo", "copy",
		"oci:"+layoutDir,
		"containers-storage:"+imageTag)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("skopeo copy: %v: %s", err, output)
	}

	fmt.Printf("Loaded image into containers-storage: %s\n", imageTag)
	return nil
}

// registryPush builds an OCI image from a checkpoint tar and pushes it to a
// container registry via crane.
func registryPush(tarPath, containerName, imageRef string, insecure bool) error {
	fmt.Printf("Registry push: building image from %s\n", tarPath)

	img, err := checkpoint.BuildCheckpointImage(tarPath, containerName)
	if err != nil {
		return fmt.Errorf("build image: %w", err)
	}

	opts := []crane.Option{crane.WithAuthFromKeychain(authn.DefaultKeychain)}
	if insecure {
		opts = append(opts, crane.Insecure)
	}

	if err := crane.Push(img, imageRef, opts...); err != nil {
		return fmt.Errorf("push image: %w", err)
	}

	fmt.Printf("Pushed image to registry: %s\n", imageRef)
	return nil
}

// handleLocalLoad handles POST /local-load requests from the controller.
func handleLocalLoad(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TarPath       string `json:"tarPath"`
		ContainerName string `json:"containerName"`
		ImageTag      string `json:"imageTag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}

	start := time.Now()
	if err := localLoad(req.TarPath, req.ContainerName, req.ImageTag); err != nil {
		http.Error(w, fmt.Sprintf("local-load: %v", err), http.StatusInternalServerError)
		return
	}

	fmt.Printf("local-load completed in %s\n", time.Since(start))
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "local-load completed successfully")
}

// handleRegistryPush handles POST /registry-push requests from the controller.
func handleRegistryPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TarPath       string `json:"tarPath"`
		ContainerName string `json:"containerName"`
		ImageRef      string `json:"imageRef"`
		Insecure      bool   `json:"insecure"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}

	start := time.Now()
	if err := registryPush(req.TarPath, req.ContainerName, req.ImageRef, req.Insecure); err != nil {
		http.Error(w, fmt.Sprintf("registry-push: %v", err), http.StatusInternalServerError)
		return
	}

	fmt.Printf("registry-push completed in %s\n", time.Since(start))
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "registry-push completed successfully")
}

func main() {
	// CLI mode: ms2m-agent local-load <tar-path> <container-name> <image-tag>
	if len(os.Args) > 1 && os.Args[1] == "local-load" {
		if len(os.Args) != 5 {
			fmt.Fprintf(os.Stderr, "usage: ms2m-agent local-load <checkpoint-tar> <container-name> <image-tag>\n")
			os.Exit(1)
		}
		if err := localLoad(os.Args[2], os.Args[3], os.Args[4]); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	storageDir := os.Getenv("STORAGE_DIR")
	if storageDir == "" {
		storageDir = "/var/lib/ms2m/incoming"
	}
	os.MkdirAll(storageDir, 0755)

	mux := http.NewServeMux()

	// Existing checkpoint upload handler (Direct transfer mode)
	handler := &checkpointHandler{storageDir: storageDir}
	mux.Handle("/checkpoint", handler)

	// New endpoints: controller calls these instead of creating Jobs
	mux.HandleFunc("/local-load", handleLocalLoad)
	mux.HandleFunc("/registry-push", handleRegistryPush)

	port := os.Getenv("PORT")
	if port == "" {
		port = "9443"
	}

	fmt.Printf("ms2m-agent listening on :%s\n", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}
