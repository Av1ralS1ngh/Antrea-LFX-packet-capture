package controller

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestProcessManager_StartCapture_Queue(t *testing.T) {
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
	// But it proves StartCapture attempts PID lookup
	err := pm.StartCapture(ctx, "test/pod", "capture", "pod", "docker://123", 3)

	if err == nil {
		t.Error("Expected error from mock getContainerPID")
	}

	if !called {
		t.Error("create capture should have called getContainerPID")
	}
}
