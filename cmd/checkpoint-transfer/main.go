package main

import (
	"fmt"
	"os"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// buildCheckpointImage creates a single-layer OCI image from a CRIU checkpoint tarball.
func buildCheckpointImage(checkpointPath, containerName string) (v1.Image, error) {
	layer, err := tarball.LayerFromFile(checkpointPath)
	if err != nil {
		return nil, fmt.Errorf("creating layer from checkpoint: %w", err)
	}

	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return nil, fmt.Errorf("appending layer to image: %w", err)
	}

	if containerName != "" {
		img = mutate.Annotations(img, map[string]string{
			"io.kubernetes.cri-o.annotations.checkpoint.name": containerName,
		}).(v1.Image)
	}

	return img, nil
}

func main() {
	if len(os.Args) < 3 || len(os.Args) > 4 {
		fmt.Fprintf(os.Stderr, "usage: %s <checkpoint-tar-path> <image-ref> [container-name]\n", os.Args[0])
		os.Exit(1)
	}

	checkpointPath := os.Args[1]
	imageRef := os.Args[2]
	containerName := ""
	if len(os.Args) == 4 {
		containerName = os.Args[3]
	}

	fmt.Printf("Building checkpoint image from %s\n", checkpointPath)
	img, err := buildCheckpointImage(checkpointPath, containerName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building image: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Pushing image to %s\n", imageRef)
	if err := crane.Push(img, imageRef, crane.WithAuthFromKeychain(authn.DefaultKeychain)); err != nil {
		fmt.Fprintf(os.Stderr, "error pushing image: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Checkpoint image pushed successfully")
}
