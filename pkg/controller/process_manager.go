package controller

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"k8s.io/klog/v2"
)

// ProcessManager manages tcpdump processes with concurrency control
type ProcessManager struct {
	mu            sync.Mutex
	captures      map[string]*CaptureProcess
	maxConcurrent int
	semaphore     chan struct{}
	captureDir    string
	criSocket     string
}

// CaptureProcess tracks a running tcpdump process
type CaptureProcess struct {
	cmd         *exec.Cmd
	cancel      context.CancelFunc
	release     func()
	releaseOnce sync.Once
	filePattern string
}

// ErrMaxConcurrent indicates the capture limit was reached.
var ErrMaxConcurrent = errors.New("max concurrent captures reached")

// NewProcessManager creates a new process manager
func NewProcessManager(maxConcurrent int, captureDir, criSocket string) *ProcessManager {
	pm := &ProcessManager{
		captures:      make(map[string]*CaptureProcess),
		maxConcurrent: maxConcurrent,
		semaphore:     make(chan struct{}, maxConcurrent),
		captureDir:    captureDir,
		criSocket:     criSocket,
	}

	// Ensure capture directory exists
	if err := os.MkdirAll(captureDir, 0755); err != nil {
		klog.ErrorS(err, "Failed to create capture directory", "dir", captureDir)
	}

	return pm
}

// StartCapture queues a capture request
func (pm *ProcessManager) StartCapture(ctx context.Context, key, captureName, podName, containerID string, maxFiles int) error {
	if err := pm.tryAcquire(ctx); err != nil {
		return err
	}

	err := pm.doStartCapture(ctx, key, captureName, podName, containerID, maxFiles)
	if err != nil {
		pm.releaseSlot()
		return err
	}

	return nil
}

// doStartCapture actually starts the tcpdump process
func (pm *ProcessManager) doStartCapture(ctx context.Context, key, captureName, podName, containerID string, maxFiles int) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Check if already running
	if _, exists := pm.captures[key]; exists {
		return fmt.Errorf("capture already running for %s", key)
	}

	// Get container PID from container ID
	pid, err := getContainerPID(containerID, pm.criSocket)
	if err != nil {
		return fmt.Errorf("failed to get container PID: %w", err)
	}

	// Create capture context with cancellation
	captureCtx, cancel := context.WithCancel(ctx)

	// Build tcpdump command with nsenter
	outputFile := filepath.Join(pm.captureDir, fmt.Sprintf("capture-%s-%s.pcap", captureName, podName))
	filePattern := filepath.Join(pm.captureDir, fmt.Sprintf("capture-%s-%s.pcap*", captureName, podName))

	// Use nsenter to enter the container's network namespace
	cmd := exec.CommandContext(captureCtx,
		"nsenter",
		"--net="+fmt.Sprintf("/proc/%d/ns/net", pid),
		"--",
		"tcpdump",
		"-C", "1", // 1MB file size
		"-W", fmt.Sprintf("%d", maxFiles), // max files
		"-w", outputFile,
		"-i", "eth0",
	)

	// Set process group to ensure we can kill children (tcpdump)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Capture stderr for debugging
	stderr, _ := cmd.StderrPipe()

	// Start the process
	if err := cmd.Start(); err != nil {
		cancel()
		RecordCaptureStart(err)
		return fmt.Errorf("failed to start tcpdump: %w", err)
	}

	pm.captures[key] = &CaptureProcess{
		cmd:     cmd,
		cancel:  cancel,
		release: pm.releaseSlot,
		filePattern: filePattern,
	}

	RecordCaptureStart(nil)
	RecordCaptureActive(1)
	klog.InfoS("tcpdump process started", "pod", key, "pid", cmd.Process.Pid)

	// Monitor stderr in background
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			klog.V(2).InfoS("tcpdump stderr", "pod", key, "msg", scanner.Text())
		}
	}()

	// Monitor process in background
	go pm.monitorProcess(key, cmd, cancel)

	return nil
}

// monitorProcess waits for tcpdump to exit
func (pm *ProcessManager) monitorProcess(key string, cmd *exec.Cmd, cancel context.CancelFunc) {
	err := cmd.Wait()
	if err != nil {
		klog.V(2).InfoS("tcpdump process exited", "pod", key, "error", err)
	} else {
		klog.V(2).InfoS("tcpdump process exited normally", "pod", key)
	}

	pm.mu.Lock()
	capture, exists := pm.captures[key]
	if exists {
		delete(pm.captures, key)
		RecordCaptureActive(-1)
	}
	pm.mu.Unlock()

	if exists {
		capture.releaseOnce.Do(capture.release)
	}

	cancel()
}

// StopCapture stops a running capture and cleans up files
func (pm *ProcessManager) StopCapture(key string) {
	pm.mu.Lock()
	capture, exists := pm.captures[key]
	if exists {
		delete(pm.captures, key)
		RecordCaptureActive(-1)
	}
	pm.mu.Unlock()

	if !exists {
		return
	}

	klog.InfoS("Stopping capture", "pod", key)

	// Signal tcpdump/nsenter to stop by killing the process group
	if capture.cmd.Process != nil {
		// Kill the entire process group
		syscall.Kill(-capture.cmd.Process.Pid, syscall.SIGKILL)
	}
	capture.cancel()
	capture.releaseOnce.Do(capture.release)

	// Clean up pcap files
	pm.cleanupFiles(capture.filePattern)
}

func (pm *ProcessManager) tryAcquire(ctx context.Context) error {
	select {
	case pm.semaphore <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrMaxConcurrent
	}
}

func (pm *ProcessManager) releaseSlot() {
	select {
	case <-pm.semaphore:
	default:
	}
}

// cleanupFiles removes all pcap files for a Pod
func (pm *ProcessManager) cleanupFiles(pattern string) {
	files, err := filepath.Glob(pattern)
	if err != nil {
		klog.ErrorS(err, "Failed to glob capture files", "pattern", pattern)
		return
	}

	for _, f := range files {
		if err := os.Remove(f); err != nil {
			klog.ErrorS(err, "Failed to remove capture file", "file", f)
		} else {
			klog.V(2).InfoS("Removed capture file", "file", f)
		}
	}
}

// getContainerPID extracts PID from container runtime
var getContainerPID = func(containerID, criSocket string) (int, error) {
	// Container ID format: containerd://<id> or docker://<id>
	parts := strings.SplitN(containerID, "://", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid container ID format: %s", containerID)
	}
	runtime := parts[0]
	id := parts[1]

	var cmd *exec.Cmd
	switch runtime {
	case "containerd", "cri-o", "crio":
		// Use crictl to get PID, specifying the socket if provided
		args := []string{"inspect", "--output", "go-template", "--template", "{{.info.pid}}", id}
		if criSocket != "" {
			args = append([]string{"--runtime-endpoint", criSocket}, args...)
		}
		cmd = exec.Command("crictl", args...)
	case "docker":
		cmd = exec.Command("docker", "inspect", "--format", "{{.State.Pid}}", id)
	default:
		return 0, fmt.Errorf("unsupported container runtime: %s", runtime)
	}

	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("failed to get container PID: %w", err)
	}

	var pid int
	_, err = fmt.Sscanf(strings.TrimSpace(string(output)), "%d", &pid)
	if err != nil {
		return 0, fmt.Errorf("failed to parse PID: %w", err)
	}

	return pid, nil
}
