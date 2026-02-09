package controller

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/klog/v2"
)

// ProcessManager manages tcpdump processes with concurrency control
type ProcessManager struct {
	mu            sync.Mutex
	captures      map[string]*CaptureProcess
	maxConcurrent int
	queue         chan *CaptureRequest
	captureDir    string
	criSocket     string
}

// CaptureProcess tracks a running tcpdump process
type CaptureProcess struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

// CaptureRequest represents a pending capture request
type CaptureRequest struct {
	Key         string
	PodName     string
	ContainerID string
	MaxFiles    int
	Ctx         context.Context
	ResultCh    chan error
}

// NewProcessManager creates a new process manager
func NewProcessManager(maxConcurrent int, captureDir, criSocket string) *ProcessManager {
	pm := &ProcessManager{
		captures:      make(map[string]*CaptureProcess),
		maxConcurrent: maxConcurrent,
		queue:         make(chan *CaptureRequest, 100),
		captureDir:    captureDir,
		criSocket:     criSocket,
	}
	
	// Ensure capture directory exists
	if err := os.MkdirAll(captureDir, 0755); err != nil {
		klog.ErrorS(err, "Failed to create capture directory", "dir", captureDir)
	}
	
	// Cleanup any orphaned tcpdump processes from previous runs
	pm.cleanupOrphanedCaptures()

	// Start queue processor
	go pm.processQueue()
	return pm
}

// cleanupOrphanedCaptures finds and kills any running tcpdump processes associated with this controller
func (pm *ProcessManager) cleanupOrphanedCaptures() {
	// Simple cleanup: kill all tcpdump processes managed by nsenter
	// A more robust approach would be to label processes, but tcpdump arg matching works for now
	// We look for "tcpdump -C ... -w <captureDir>/..."
	
	klog.InfoS("Cleaning up orphaned capture processes", "dir", pm.captureDir)
	
	// Use pgrep to find pids of tcpdump writing to our capture dir
	// escapes for regex? pm.captureDir might contain special chars. 
	// For simplicity, we just assume standard paths without regex special chars or we quote it?
	// pgrep -f matches the full command line.
	cmd := exec.Command("pgrep", "-f", fmt.Sprintf("tcpdump.*%s", pm.captureDir))
	output, err := cmd.Output()
	if err != nil {
		// No processes found (exit code 1) or other error
		return
	}
	
	pids := strings.Fields(string(output))
	for _, pidStr := range pids {
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		
		klog.InfoS("Killing orphaned tcpdump", "pid", pid)
		proc.Kill()
		proc.Wait() // release resources
	}
}

// processQueue processes capture requests from the queue
func (pm *ProcessManager) processQueue() {
	for req := range pm.queue {
		pm.mu.Lock()
		running := len(pm.captures)
		pm.mu.Unlock()

		// Wait until we have capacity
		for running >= pm.maxConcurrent {
			// Simple polling - in production you'd use a semaphore
			time.Sleep(100 * time.Millisecond)
			pm.mu.Lock()
			running = len(pm.captures)
			pm.mu.Unlock()
		}

		err := pm.doStartCapture(req.Ctx, req.Key, req.PodName, req.ContainerID, req.MaxFiles)
		if req.ResultCh != nil {
			req.ResultCh <- err
		}
	}
}

// StartCapture queues a capture request
func (pm *ProcessManager) StartCapture(ctx context.Context, key, podName, containerID string, maxFiles int) error {
	resultCh := make(chan error, 1)
	pm.queue <- &CaptureRequest{
		Key:         key,
		PodName:     podName,
		ContainerID: containerID,
		MaxFiles:    maxFiles,
		Ctx:         ctx,
		ResultCh:    resultCh,
	}
	return <-resultCh
}

// doStartCapture actually starts the tcpdump process
func (pm *ProcessManager) doStartCapture(ctx context.Context, key, podName, containerID string, maxFiles int) error {
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
	outputFile := filepath.Join(pm.captureDir, fmt.Sprintf("capture-%s.pcap", podName))
	
	// Use nsenter to enter the container's network namespace
	cmd := exec.CommandContext(captureCtx,
		"nsenter",
		"--net="+fmt.Sprintf("/proc/%d/ns/net", pid),
		"--",
		"tcpdump",
		"-C", "1",           // 1MB file size
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
		cmd:    cmd,
		cancel: cancel,
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
	if _, exists := pm.captures[key]; exists {
		delete(pm.captures, key)
		RecordCaptureActive(-1)
	}
	pm.mu.Unlock()

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
		capture.cmd.Process.Wait()
	}
	capture.cancel()

	// Clean up pcap files
	pm.cleanupFiles(key)
}

// cleanupFiles removes all pcap files for a Pod
func (pm *ProcessManager) cleanupFiles(key string) {
	// Extract pod name from key (namespace/name)
	parts := strings.Split(key, "/")
	if len(parts) != 2 {
		return
	}
	podName := parts[1]

	pattern := filepath.Join(pm.captureDir, fmt.Sprintf("capture-%s.pcap*", podName))
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
	case "containerd":
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
