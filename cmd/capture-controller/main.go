package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/Av1ralS1ngh/antrea-packet-capture/pkg/controller"
)

func main() {
	var (
		criSocket     string
		captureDir    string
		maxConcurrent int
	)
	flag.StringVar(&criSocket, "cri-socket", "", "Path to CRI socket (auto-detected if empty)")
	flag.StringVar(&captureDir, "capture-dir", "/capture", "Directory to store pcap files")
	flag.IntVar(&maxConcurrent, "max-concurrent", 5, "Maximum concurrent captures")

	klog.InitFlags(nil)
	flag.Parse()
	defer klog.Flush()

	// Get node name from environment (set via downward API)
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		klog.Fatal("NODE_NAME environment variable must be set")
	}

	// Auto-detect CRI socket if not specified
	if criSocket == "" {
		knownSockets := []string{
			"/run/containerd/containerd.sock",
			"/run/crio/crio.sock",
			"/var/run/dockershim.sock",
		}
		for _, s := range knownSockets {
			if _, err := os.Stat(s); err == nil {
				criSocket = "unix://" + s
				klog.InfoS("Auto-detected CRI socket", "path", criSocket)
				break
			}
		}
	}
	if criSocket == "" {
		klog.Warning("Could not detect CRI socket, set --cri-socket flag if needed")
	}

	// Build Kubernetes client config
	config, err := rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig for local development
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = os.Getenv("HOME") + "/.kube/config"
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			klog.Fatalf("Failed to build kubeconfig: %v", err)
		}
	}

	// Create Kubernetes clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// Create shared informer factory (cluster-wide Pod watch)
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)
	podInformer := informerFactory.Core().V1().Pods()

	// Create the controller
	ctrl := controller.NewController(
		podInformer,
		nodeName,
		criSocket,
		captureDir,
		maxConcurrent,
	)

	// Set up signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		klog.Info("Received shutdown signal, stopping controller...")
		cancel()
	}()

	// Start informers
	informerFactory.Start(ctx.Done())

	// Run the controller
	if err := ctrl.Run(ctx, 2); err != nil {
		klog.Fatalf("Error running controller: %v", err)
	}
}
