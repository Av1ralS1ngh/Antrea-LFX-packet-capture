package controller

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	annotationKey = "tcpdump.antrea.io"
	maxRetries    = 5
)

// CaptureState tracks a running capture on a Pod.
type CaptureState struct {
	fileLocation string
	filePattern  string
}

// Controller watches Pods and manages packet captures.
type Controller struct {
	podLister  corelisters.PodLister
	podSynced  cache.InformerSynced
	queue      workqueue.RateLimitingInterface
	nodeName   string
	criSocket  string
	captureDir string

	// Process manager for tcpdump
	processManager *ProcessManager

	mu             sync.Mutex
	activeCaptures map[string]*CaptureState // key: namespace/name
}

// NewController creates a new capture controller.
func NewController(
	podInformer coreinformers.PodInformer,
	nodeName, criSocket, captureDir string,
	maxConcurrent int,
) *Controller {
	c := &Controller{
		podLister:      podInformer.Lister(),
		podSynced:      podInformer.Informer().HasSynced,
		queue:          workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		nodeName:       nodeName,
		criSocket:      criSocket,
		captureDir:     captureDir,
		processManager: NewProcessManager(maxConcurrent, captureDir, criSocket),
		activeCaptures: make(map[string]*CaptureState),
	}

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addPod,
		UpdateFunc: c.updatePod,
		DeleteFunc: c.deletePod,
	})

	return c
}

// podKey returns namespace/name for a Pod.
func podKey(pod *corev1.Pod) string {
	return pod.Namespace + "/" + pod.Name
}

// addPod handles Pod add events.
func (c *Controller) addPod(obj interface{}) {
	pod := obj.(*corev1.Pod)
	c.enqueuePod(pod)
}

// updatePod handles Pod update events.
func (c *Controller) updatePod(oldObj, newObj interface{}) {
	oldPod := oldObj.(*corev1.Pod)
	newPod := newObj.(*corev1.Pod)
	c.enqueuePod(oldPod)
	c.enqueuePod(newPod)
}

// deletePod handles Pod delete events.
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
	c.enqueuePod(pod)
}

// enqueuePod adds a Pod to the work queue.
func (c *Controller) enqueuePod(pod *corev1.Pod) {
	if pod.Spec.NodeName != c.nodeName {
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		klog.ErrorS(err, "Failed to get key for Pod")
		return
	}
	c.queue.Add(key)
}

// Run starts the controller.
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer c.queue.ShutDown()

	klog.Info("Starting capture controller")
	defer klog.Info("Shutting down capture controller")

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

// runWorker processes items from the queue.
func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem handles a single item from the queue.
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

// syncPod is the main reconciliation logic.
func (c *Controller) syncPod(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	pod, err := c.podLister.Pods(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			c.stopCapture(key)
			return nil
		}
		return err
	}

	if pod.Spec.NodeName != c.nodeName {
		c.stopCapture(key)
		return nil
	}

	annoValue, hasAnno := pod.Annotations[annotationKey]
	if !hasAnno {
		c.stopCapture(key)
		return nil
	}

	maxFiles, err := parseMaxFiles(annoValue)
	if err != nil {
		klog.ErrorS(err, "Invalid annotation value", "pod", key, "annotation", annotationKey, "value", annoValue)
		c.stopCapture(key)
		return nil
	}

	if c.getCaptureState(key) != nil {
		return nil
	}

	return c.startCapture(ctx, key, pod, maxFiles)
}

func parseMaxFiles(value string) (int, error) {
	maxFiles, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid max files %q: %w", value, err)
	}
	if maxFiles <= 0 {
		return 0, fmt.Errorf("max files must be > 0, got %d", maxFiles)
	}
	return maxFiles, nil
}

func (c *Controller) startCapture(ctx context.Context, key string, pod *corev1.Pod, maxFiles int) error {
	containerID := ""
	if len(pod.Status.ContainerStatuses) > 0 {
		containerID = pod.Status.ContainerStatuses[0].ContainerID
	}
	if containerID == "" {
		return fmt.Errorf("no container ID found for pod %s", key)
	}

	fileLocation := c.captureFileLocation(pod.Name)

	err := c.processManager.StartCapture(ctx, key, pod.Name, containerID, maxFiles)
	if err != nil {
		if errors.Is(err, ErrMaxConcurrent) {
			klog.InfoS("Max concurrent captures reached, retrying later", "pod", key)
			c.queue.AddAfter(key, 2*time.Second)
			return nil
		}
		return fmt.Errorf("failed to start capture: %w", err)
	}

	state := &CaptureState{
		fileLocation: fileLocation,
		filePattern:  c.captureFilePattern(pod.Name),
	}

	c.mu.Lock()
	c.activeCaptures[key] = state
	c.mu.Unlock()

	klog.InfoS("Started packet capture", "pod", key, "file", fileLocation, "maxFiles", maxFiles)
	return nil
}

func (c *Controller) stopCapture(podKey string) {
	c.mu.Lock()
	state := c.activeCaptures[podKey]
	if state != nil {
		delete(c.activeCaptures, podKey)
	}
	c.mu.Unlock()

	if state == nil {
		return
	}

	c.processManager.StopCapture(podKey)
	c.processManager.CleanupCaptureFilesForPod(state.filePattern)
}

func (c *Controller) captureFileLocation(podName string) string {
	return filepath.Join(c.captureDir, fmt.Sprintf("capture-%s.pcap", podName))
}

func (c *Controller) captureFilePattern(podName string) string {
	return filepath.Join(c.captureDir, fmt.Sprintf("capture-%s.pcap*", podName))
}

func (c *Controller) getCaptureState(podKey string) *CaptureState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeCaptures[podKey]
}
