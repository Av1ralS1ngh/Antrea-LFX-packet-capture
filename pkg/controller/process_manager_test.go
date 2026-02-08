package controller

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestProcessManager_StartCapture_Queue(t *testing.T) {
	// Use a buffered channel to mock async execution
	done := make(chan struct{})
	
	// Mock getContainerPID to verify it's called
	originalGetPID := getContainerPID
	defer func() { getContainerPID = originalGetPID }()

	called := false
	getContainerPID = func(id, socket string) (int, error) {
		called = true
		return 123, fmt.Errorf("mock error to stop execution")
	}

	pm := NewProcessManager(1, "/tmp/captures", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// This should fail because getContainerPID returns error
	// But it proves the queue picked up the item
	err := pm.StartCapture(ctx, "test/pod", "pod", "docker://123", 3)
	
	if err == nil {
		t.Error("Expected error from mock getContainerPID")
	}
	
	if !called {
		t.Error("create capture should have called getContainerPID")
	}

	close(done)
}
