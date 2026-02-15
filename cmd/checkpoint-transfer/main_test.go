package main

import (
	"os"
	"testing"
)

func TestBuildCheckpointImage_InvalidPath(t *testing.T) {
	_, err := buildCheckpointImage("/nonexistent/path/checkpoint.tar")
	if err == nil {
		t.Fatal("expected error for nonexistent file, got nil")
	}
}

func TestBuildCheckpointImage_ValidTar(t *testing.T) {
	// Create a minimal tar file (1024 zero bytes is a valid empty tar archive).
	tmpFile, err := os.CreateTemp(t.TempDir(), "checkpoint-*.tar")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer tmpFile.Close()

	buf := make([]byte, 1024)
	if _, err := tmpFile.Write(buf); err != nil {
		t.Fatalf("failed to write tar data: %v", err)
	}
	tmpFile.Close()

	img, err := buildCheckpointImage(tmpFile.Name())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if img == nil {
		t.Fatal("expected non-nil image")
	}

	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("failed to get layers: %v", err)
	}
	if len(layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(layers))
	}
}
