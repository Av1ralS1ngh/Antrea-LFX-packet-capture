package controller

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	// AnnotationKey is the annotation that triggers packet capture
	AnnotationKey = "tcpdump.antrea.io"

	// CaptureDir is where pcap files are stored
	CaptureDir = "/captures"

	maxRetries = 5
)

// Controller watches Pods and manages packet captures based on annotations
type Controller struct {
	kubeClient kubernetes.Interface
	podLister  corelisters.PodLister
	podSynced  cache.InformerSynced
	queue      workqueue.RateLimitingInterface
	nodeName   string
	criSocket  string
	captureDir string

	// Process manager for tcpdump
	processManager *ProcessManager

	// Track which Pods have active captures
	mu             sync.Mutex
	activeCaptures map[string]int // key: namespace/name, value: maxFiles
}

// NewController creates a new capture controller
func NewController(
	kubeClient kubernetes.Interface,
	podInformer coreinformers.PodInformer,
	nodeName, criSocket, captureDir string,
	maxConcurrent int,
) *Controller {
	c := &Controller{
		kubeClient:     kubeClient,
		podLister:      podInformer.Lister(),
		podSynced:      podInformer.Informer().HasSynced,
		queue:          workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		nodeName:       nodeName,
		criSocket:      criSocket,
		captureDir:     captureDir,
		processManager: NewProcessManager(maxConcurrent, captureDir, criSocket),
		activeCaptures: make(map[string]int),
	}

	// Set up event handlers
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addPod,
		UpdateFunc: c.updatePod,
		DeleteFunc: c.deletePod,
	})

	return c
}

// podKey returns namespace/name for a Pod
func podKey(pod *corev1.Pod) string {
	return pod.Namespace + "/" + pod.Name
}

// addPod handles Pod add events
func (c *Controller) addPod(obj interface{}) {
	pod := obj.(*corev1.Pod)
	if pod.Spec.NodeName != c.nodeName {
		return
	}
	if _, ok := pod.Annotations[AnnotationKey]; ok {
		klog.V(2).InfoS("Pod added with capture annotation", "pod", podKey(pod))
		c.enqueuePod(pod)
	}
}

// updatePod handles Pod update events
func (c *Controller) updatePod(oldObj, newObj interface{}) {
	oldPod := oldObj.(*corev1.Pod)
	newPod := newObj.(*corev1.Pod)

	if newPod.Spec.NodeName != c.nodeName {
		return
	}

	oldVal, oldOk := oldPod.Annotations[AnnotationKey]
	newVal, newOk := newPod.Annotations[AnnotationKey]

	// Enqueue if annotation was added, removed, or changed
	if oldOk != newOk || oldVal != newVal {
		klog.V(2).InfoS("Pod annotation changed", "pod", podKey(newPod),
			"oldAnnotation", oldVal, "newAnnotation", newVal)
		c.enqueuePod(newPod)
	}
}

// deletePod handles Pod delete events
func (c *Controller) deletePod(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Couldn't get object from tombstone: %v", obj)
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			klog.Errorf("Tombstone contained non-Pod object: %v", tombstone.Obj)
			return
		}
	}

	if pod.Spec.NodeName != c.nodeName {
		return
	}

	klog.V(2).InfoS("Pod deleted", "pod", podKey(pod))
	c.enqueuePod(pod)
}

// enqueuePod adds a Pod to the work queue
func (c *Controller) enqueuePod(pod *corev1.Pod) {
	c.queue.Add(podKey(pod))
}

// Run starts the controller
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer c.queue.ShutDown()

	klog.Info("Starting capture controller")
	defer klog.Info("Shutting down capture controller")

	// Wait for caches to sync
	klog.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(ctx.Done(), c.podSynced) {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("Starting workers")
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	<-ctx.Done()
	return nil
}

// runWorker processes items from the queue
func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem handles a single item from the queue
func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	obj, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(obj)

	key, ok := obj.(string)
	if !ok {
		c.queue.Forget(obj)
		klog.Errorf("Expected string in workqueue but got %#v", obj)
		return true
	}

	err := c.syncPod(ctx, key)
	if err == nil {
		c.queue.Forget(obj)
		return true
	}

	if c.queue.NumRequeues(obj) < maxRetries {
		klog.ErrorS(err, "Error syncing Pod, retrying", "key", key)
		c.queue.AddRateLimited(obj)
		return true
	}

	klog.ErrorS(err, "Error syncing Pod, giving up", "key", key)
	c.queue.Forget(obj)
	return true
}

// syncPod is the main reconciliation logic
func (c *Controller) syncPod(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	pod, err := c.podLister.Pods(namespace).Get(name)
	if errors.IsNotFound(err) {
		// Pod was deleted, stop any active capture
		klog.V(2).InfoS("Pod not found, cleaning up", "key", key)
		c.stopCapture(key)
		return nil
	}
	if err != nil {
		return err
	}

	// Get annotation value
	annotationValue, hasAnnotation := pod.Annotations[AnnotationKey]

	c.mu.Lock()
	_, hasActiveCapture := c.activeCaptures[key]
	c.mu.Unlock()

	if !hasAnnotation {
		// Annotation removed, stop capture
		if hasActiveCapture {
			klog.InfoS("Annotation removed, stopping capture", "pod", key)
			c.stopCapture(key)
		}
		return nil
	}

	// Parse and validate annotation value
	maxFiles, err := strconv.Atoi(annotationValue)
	if err != nil || maxFiles <= 0 {
		klog.ErrorS(err, "Invalid annotation value, ignoring",
			"pod", key, "value", annotationValue)
		return nil // Don't retry, it's a user error
	}

	// Check if capture needs to be started or restarted
	c.mu.Lock()
	currentMaxFiles := c.activeCaptures[key]
	c.mu.Unlock()

	if hasActiveCapture && currentMaxFiles == maxFiles {
		// Capture already running with same config
		return nil
	}

	if hasActiveCapture {
		// Annotation value changed, restart capture
		klog.InfoS("Annotation value changed, restarting capture",
			"pod", key, "oldValue", currentMaxFiles, "newValue", maxFiles)
		c.stopCapture(key)
	}

	// Start new capture
	return c.startCapture(ctx, pod, maxFiles)
}

// startCapture begins a packet capture for the Pod
func (c *Controller) startCapture(ctx context.Context, pod *corev1.Pod, maxFiles int) error {
	key := podKey(pod)

	// Get container PID for network namespace
	containerID := ""
	if len(pod.Status.ContainerStatuses) > 0 {
		containerID = pod.Status.ContainerStatuses[0].ContainerID
	}
	if containerID == "" {
		return fmt.Errorf("no container ID found for pod %s", key)
	}

	// Start capture via process manager
	err := c.processManager.StartCapture(ctx, key, pod.Name, containerID, maxFiles)
	if err != nil {
		return fmt.Errorf("failed to start capture: %w", err)
	}

	c.mu.Lock()
	c.activeCaptures[key] = maxFiles
	c.mu.Unlock()

	klog.InfoS("Started packet capture", "pod", key, "maxFiles", maxFiles)
	return nil
}

// stopCapture stops an active capture and cleans up files
func (c *Controller) stopCapture(key string) {
	c.processManager.StopCapture(key)

	c.mu.Lock()
	delete(c.activeCaptures, key)
	c.mu.Unlock()

	klog.InfoS("Stopped packet capture", "pod", key)
}
