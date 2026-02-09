package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/Av1ralS1ngh/antrea-packet-capture/pkg/apis/packetcapture"
)

const (
	// CaptureDir is where pcap files are stored.
	CaptureDir = "/captures"

	maxRetries      = 5
	defaultMaxFiles = 3

	PhasePending   = "Pending"
	PhaseRunning   = "Running"
	PhaseCompleted = "Completed"
	PhaseFailed    = "Failed"
)

var packetCaptureGVR = schema.GroupVersionResource{
	Group:    "antrea.io",
	Version:  "v1alpha1",
	Resource: "packetcaptures",
}

// CaptureState tracks a running capture on a Pod.
type CaptureState struct {
	captureKey  string
	fileLocation string
	filePattern  string
	timeout     time.Duration
	stopTimer   *time.Timer
}

// Controller watches PacketCapture resources and manages packet captures.
type Controller struct {
	pcClient   dynamic.Interface
	podLister  corelisters.PodLister
	podSynced  cache.InformerSynced
	pcInformer cache.SharedIndexInformer
	pcSynced   cache.InformerSynced
	queue      workqueue.RateLimitingInterface
	nodeName   string
	criSocket  string
	captureDir string

	// Process manager for tcpdump
	processManager *ProcessManager

	mu             sync.Mutex
	activeCaptures map[string]*CaptureState        // key: namespace/name
	capturePods    map[string]map[string]struct{} // captureKey -> pod keys
}

// NewController creates a new capture controller.
func NewController(
	pcClient dynamic.Interface,
	podInformer coreinformers.PodInformer,
	pcInformer cache.SharedIndexInformer,
	nodeName, criSocket, captureDir string,
	maxConcurrent int,
) *Controller {
	c := &Controller{
		pcClient:       pcClient,
		podLister:      podInformer.Lister(),
		podSynced:      podInformer.Informer().HasSynced,
		pcInformer:     pcInformer,
		pcSynced:       pcInformer.HasSynced,
		queue:          workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		nodeName:       nodeName,
		criSocket:      criSocket,
		captureDir:     captureDir,
		processManager: NewProcessManager(maxConcurrent, captureDir, criSocket),
		activeCaptures: make(map[string]*CaptureState),
		capturePods:    make(map[string]map[string]struct{}),
	}

	pcInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addPacketCapture,
		UpdateFunc: c.updatePacketCapture,
		DeleteFunc: c.deletePacketCapture,
	})

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

// addPacketCapture handles PacketCapture add events.
func (c *Controller) addPacketCapture(obj interface{}) {
	c.enqueuePacketCapture(obj)
}

// updatePacketCapture handles PacketCapture update events.
func (c *Controller) updatePacketCapture(_, newObj interface{}) {
	c.enqueuePacketCapture(newObj)
}

// deletePacketCapture handles PacketCapture delete events.
func (c *Controller) deletePacketCapture(obj interface{}) {
	c.enqueuePacketCapture(obj)
}

// addPod handles Pod add events.
func (c *Controller) addPod(obj interface{}) {
	pod := obj.(*corev1.Pod)
	c.enqueueMatchingPacketCaptures(pod)
}

// updatePod handles Pod update events.
func (c *Controller) updatePod(oldObj, newObj interface{}) {
	oldPod := oldObj.(*corev1.Pod)
	newPod := newObj.(*corev1.Pod)

	c.enqueueMatchingPacketCaptures(oldPod)
	c.enqueueMatchingPacketCaptures(newPod)
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

	c.enqueueMatchingPacketCaptures(pod)
}

// enqueuePacketCapture adds a PacketCapture to the work queue.
func (c *Controller) enqueuePacketCapture(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		klog.ErrorS(err, "Failed to get key for PacketCapture")
		return
	}
	c.queue.Add(key)
}

// enqueueMatchingPacketCaptures adds matching PacketCapture resources for a Pod.
func (c *Controller) enqueueMatchingPacketCaptures(pod *corev1.Pod) {
	if pod.Spec.NodeName != c.nodeName {
		return
	}

	objs := c.pcInformer.GetStore().List()
	for _, obj := range objs {
		pc, _, err := c.packetCaptureFromObject(obj)
		if err != nil {
			klog.ErrorS(err, "Failed to parse PacketCapture")
			continue
		}
		if pc.Namespace != pod.Namespace {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(&pc.Spec.PodSelector)
		if err != nil {
			continue
		}
		if selector.Matches(labelsForPod(pod)) {
			c.queue.Add(pc.Namespace + "/" + pc.Name)
		}
	}
}

func labelsForPod(pod *corev1.Pod) labels.Set {
	if pod.Labels == nil {
		return labels.Set{}
	}
	return labels.Set(pod.Labels)
}

// Run starts the controller.
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer c.queue.ShutDown()

	klog.Info("Starting capture controller")
	defer klog.Info("Shutting down capture controller")

	klog.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(ctx.Done(), c.podSynced, c.pcSynced) {
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

	err := c.syncPacketCapture(ctx, key)
	if err == nil {
		c.queue.Forget(obj)
		return true
	}

	if c.queue.NumRequeues(obj) < maxRetries {
		klog.ErrorS(err, "Error syncing PacketCapture, retrying", "key", key)
		c.queue.AddRateLimited(obj)
		return true
	}

	klog.ErrorS(err, "Error syncing PacketCapture, giving up", "key", key)
	c.queue.Forget(obj)
	return true
}

// syncPacketCapture is the main reconciliation logic.
func (c *Controller) syncPacketCapture(ctx context.Context, key string) error {
	pc, _, exists, err := c.packetCaptureByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		c.cleanupCapture(key)
		return nil
	}

	if pc.Status.Phase == PhaseCompleted {
		return nil
	}

	selector, err := metav1.LabelSelectorAsSelector(&pc.Spec.PodSelector)
	if err != nil {
		return c.ensurePacketCaptureStatus(ctx, key, pc.Status, packetcapture.PacketCaptureStatus{
			Phase:   PhaseFailed,
			Message: fmt.Sprintf("Invalid podSelector: %v", err),
		})
	}

	pods, err := c.podLister.Pods(pc.Namespace).List(selector)
	if err != nil {
		return err
	}

	desired := make(map[string]*corev1.Pod)
	for _, pod := range pods {
		if pod.Spec.NodeName != c.nodeName {
			continue
		}
		desired[podKey(pod)] = pod
	}


	if err := c.reconcileCapture(ctx, key, pc, desired); err != nil {
		return err
	}

	activeCount := c.activeCountForCapture(key)
	if len(desired) == 0 {
		if pc.Status.NodeName != "" && pc.Status.NodeName != c.nodeName {
			return nil
		}
		if pc.Status.Phase == PhaseRunning || pc.Status.Phase == PhaseCompleted {
			return nil
		}
		if pc.Status.Phase != PhaseCompleted {
			return c.ensurePacketCaptureStatus(ctx, key, pc.Status, packetcapture.PacketCaptureStatus{
				Phase:   PhasePending,
				Message: "No matching pods on this node",
			})
		}
		return nil
	}

	if activeCount > 0 {
		fileLocation := c.firstFileLocationForCapture(key)
		return c.ensurePacketCaptureStatus(ctx, key, pc.Status, packetcapture.PacketCaptureStatus{
			Phase:        PhaseRunning,
			FileLocation: fileLocation,
			NodeName:     c.nodeName,
		})
	}

	if pc.Status.Phase == PhaseCompleted {
		return nil
	}

	return c.ensurePacketCaptureStatus(ctx, key, pc.Status, packetcapture.PacketCaptureStatus{
		Phase:   PhasePending,
		Message: "Waiting for capture to start",
		NodeName: c.nodeName,
	})
}

func (c *Controller) reconcileCapture(ctx context.Context, captureKey string, pc *packetcapture.PacketCapture, desired map[string]*corev1.Pod) error {
	existing := c.capturePodsForKey(captureKey)
	for podKey := range existing {
		if _, ok := desired[podKey]; !ok {
			c.stopCapture(podKey, "", "")
		}
	}

	for podKey, pod := range desired {
		state := c.getCaptureState(podKey)
		if state != nil {
			if state.captureKey != captureKey {
				klog.InfoS("Pod already captured by another PacketCapture", "pod", podKey, "packetCapture", state.captureKey)
			}
			continue
		}
		if err := c.startCapture(ctx, captureKey, pc, pod); err != nil {
			return err
		}
	}

	return nil
}

func (c *Controller) startCapture(ctx context.Context, captureKey string, pc *packetcapture.PacketCapture, pod *corev1.Pod) error {
	key := podKey(pod)

	containerID := ""
	if len(pod.Status.ContainerStatuses) > 0 {
		containerID = pod.Status.ContainerStatuses[0].ContainerID
	}
	if containerID == "" {
		return fmt.Errorf("no container ID found for pod %s", key)
	}

	fileLocation := c.captureFileLocation(pc.Name, pod.Name)

	err := c.processManager.StartCapture(ctx, key, pc.Name, pod.Name, containerID, defaultMaxFiles)
	if err != nil {
		if errors.Is(err, ErrMaxConcurrent) {
			klog.InfoS("Max concurrent captures reached, retrying later", "packetCapture", captureKey, "pod", key)
			c.queue.AddAfter(captureKey, 2*time.Second)
			return nil
		}
		return fmt.Errorf("failed to start capture: %w", err)
	}

	state := &CaptureState{
		captureKey:   captureKey,
		fileLocation: fileLocation,
		filePattern:  c.captureFilePattern(pc.Name, pod.Name),
		timeout:      pc.Spec.Timeout.Duration,
	}
	if state.timeout > 0 {
		state.stopTimer = time.AfterFunc(state.timeout, func() {
			c.stopCaptureForTimeout(key)
		})
	}

	c.mu.Lock()
	c.activeCaptures[key] = state
	if c.capturePods[captureKey] == nil {
		c.capturePods[captureKey] = make(map[string]struct{})
	}
	c.capturePods[captureKey][key] = struct{}{}
	c.mu.Unlock()

	klog.InfoS("Started packet capture", "packetCapture", captureKey, "pod", key, "file", fileLocation, "timeout", state.timeout)
	return nil
}

func (c *Controller) stopCaptureForTimeout(podKey string) {
	c.mu.Lock()
	state := c.activeCaptures[podKey]
	c.mu.Unlock()
	if state == nil {
		return
	}

	message := fmt.Sprintf("Capture timed out after %s", state.timeout.String())
	c.stopCapture(podKey, PhaseCompleted, message)
}

func (c *Controller) stopCapture(podKey, phase, message string) {
	c.mu.Lock()
	state := c.activeCaptures[podKey]
	if state != nil {
		delete(c.activeCaptures, podKey)
		if state.stopTimer != nil {
			state.stopTimer.Stop()
		}
		pods := c.capturePods[state.captureKey]
		if pods != nil {
			delete(pods, podKey)
			if len(pods) == 0 {
				delete(c.capturePods, state.captureKey)
			}
		}
	}
	c.mu.Unlock()

	if state == nil {
		return
	}

	c.processManager.StopCapture(podKey)

	if phase == "" {
		return
	}

	if c.activeCountForCapture(state.captureKey) != 0 {
		return
	}

	status := packetcapture.PacketCaptureStatus{
		Phase:        phase,
		FileLocation: c.latestCaptureFile(state.filePattern, state.fileLocation),
		Message:      message,
		NodeName:     c.nodeName,
	}
	if err := c.updatePacketCaptureStatus(context.Background(), state.captureKey, status); err != nil {
		klog.ErrorS(err, "Failed to update PacketCapture status", "packetCapture", state.captureKey)
	}
}

func (c *Controller) cleanupCapture(captureKey string) {
	pods := c.capturePodsForKey(captureKey)
	for podKey := range pods {
		c.stopCapture(podKey, "", "")
	}

	_, name, err := cache.SplitMetaNamespaceKey(captureKey)
	if err != nil {
		klog.ErrorS(err, "Failed to parse PacketCapture key", "key", captureKey)
		return
	}

	c.processManager.CleanupCaptureFilesForCapture(name)
}

func (c *Controller) captureFileLocation(captureName, podName string) string {
	return filepath.Join(c.captureDir, fmt.Sprintf("capture-%s-%s.pcap", captureName, podName))
}

func (c *Controller) captureFilePattern(captureName, podName string) string {
	return filepath.Join(c.captureDir, fmt.Sprintf("capture-%s-%s.pcap*", captureName, podName))
}

func (c *Controller) packetCaptureFromObject(obj interface{}) (*packetcapture.PacketCapture, *unstructured.Unstructured, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, nil, fmt.Errorf("expected Unstructured but got %T", obj)
	}
	pc := &packetcapture.PacketCapture{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, pc); err != nil {
		return nil, nil, err
	}
	return pc, u, nil
}

func (c *Controller) packetCaptureByKey(key string) (*packetcapture.PacketCapture, *unstructured.Unstructured, bool, error) {
	obj, exists, err := c.pcInformer.GetStore().GetByKey(key)
	if err != nil {
		return nil, nil, false, err
	}
	if !exists {
		return nil, nil, false, nil
	}
	pc, u, err := c.packetCaptureFromObject(obj)
	if err != nil {
		return nil, nil, false, err
	}
	return pc, u, true, nil
}

func (c *Controller) capturePodsForKey(captureKey string) map[string]struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[string]struct{})
	for podKey := range c.capturePods[captureKey] {
		result[podKey] = struct{}{}
	}
	return result
}

func (c *Controller) getCaptureState(podKey string) *CaptureState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeCaptures[podKey]
}

func (c *Controller) activeCountForCapture(captureKey string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.capturePods[captureKey])
}

func (c *Controller) firstFileLocationForCapture(captureKey string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for podKey := range c.capturePods[captureKey] {
		if state := c.activeCaptures[podKey]; state != nil {
			return c.latestCaptureFile(state.filePattern, state.fileLocation)
		}
	}
	return ""
}

func (c *Controller) latestCaptureFile(pattern, fallback string) string {
	files, err := filepath.Glob(pattern)
	if err != nil || len(files) == 0 {
		return fallback
	}

	latest := ""
	latestTime := time.Time{}
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			continue
		}
		if info.ModTime().After(latestTime) {
			latestTime = info.ModTime()
			latest = file
		}
	}
	if latest == "" {
		return fallback
	}
	return latest
}

func (c *Controller) ensurePacketCaptureStatus(ctx context.Context, key string, current, desired packetcapture.PacketCaptureStatus) error {
	if current.NodeName != "" && current.NodeName != c.nodeName {
		return nil
	}
	if statusEqual(current, desired) {
		return nil
	}
	return c.updatePacketCaptureStatus(ctx, key, desired)
}

func statusEqual(a, b packetcapture.PacketCaptureStatus) bool {
	return a.Phase == b.Phase && a.FileLocation == b.FileLocation && a.Message == b.Message && a.NodeName == b.NodeName
}

func (c *Controller) updatePacketCaptureStatus(ctx context.Context, key string, status packetcapture.PacketCaptureStatus) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		obj, err := c.pcClient.Resource(packetCaptureGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}

		current := &packetcapture.PacketCapture{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, current); err != nil {
			return err
		}
		if current.Status.NodeName != "" && current.Status.NodeName != c.nodeName {
			return nil
		}
		if statusEqual(current.Status, status) {
			return nil
		}

		statusMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
		if err != nil {
			return err
		}
		if err := unstructured.SetNestedField(obj.Object, statusMap, "status"); err != nil {
			return err
		}

		_, err = c.pcClient.Resource(packetCaptureGVR).Namespace(namespace).UpdateStatus(ctx, obj, metav1.UpdateOptions{})
		return err
	})
}
