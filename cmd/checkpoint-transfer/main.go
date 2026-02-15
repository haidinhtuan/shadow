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
func buildCheckpointImage(checkpointPath string) (v1.Image, error) {
	layer, err := tarball.LayerFromFile(checkpointPath)
	if err != nil {
		return nil, fmt.Errorf("creating layer from checkpoint: %w", err)
	}

	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return nil, fmt.Errorf("appending layer to image: %w", err)
	}

	return img, nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: %s <checkpoint-tar-path> <image-ref>\n", os.Args[0])
		os.Exit(1)
	}

	checkpointPath := os.Args[1]
	imageRef := os.Args[2]

	fmt.Printf("Building checkpoint image from %s\n", checkpointPath)
	img, err := buildCheckpointImage(checkpointPath)
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
