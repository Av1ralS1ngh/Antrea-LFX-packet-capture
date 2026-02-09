package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
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
		httpAddr      string
	)
	flag.StringVar(&criSocket, "cri-socket", "", "Path to CRI socket (auto-detected if empty)")
	flag.StringVar(&captureDir, "capture-dir", "/captures", "Directory to store pcap files")
	flag.IntVar(&maxConcurrent, "max-concurrent", 5, "Maximum concurrent captures")
	flag.StringVar(&httpAddr, "http-addr", "0.0.0.0:8090", "HTTP listen address for pcap downloads (empty to disable)")

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

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create dynamic Kubernetes client: %v", err)
	}

	// Create shared informer factory (cluster-wide Pod watch)
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)
	podInformer := informerFactory.Core().V1().Pods()

	pcGVR := schema.GroupVersionResource{Group: "antrea.io", Version: "v1alpha1", Resource: "packetcaptures"}
	pcInformerFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClient, 0, v1.NamespaceAll, nil)
	pcInformer := pcInformerFactory.ForResource(pcGVR).Informer()

	// Create the controller
	ctrl := controller.NewController(
		dynamicClient,
		podInformer,
		pcInformer,
		nodeName,
		criSocket,
		captureDir,
		maxConcurrent,
	)

	// Register metrics
	controller.RegisterMetrics()

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
	pcInformerFactory.Start(ctx.Done())

	if httpAddr != "" {
		go startHTTPServer(ctx, httpAddr, captureDir)
	}

	// Run the controller
	if err := ctrl.Run(ctx, 2); err != nil {
		klog.Fatalf("Error running controller: %v", err)
	}
}

func startHTTPServer(ctx context.Context, addr, captureDir string) {
	mux := http.NewServeMux()
	mux.Handle("/captures/", http.StripPrefix("/captures/", http.FileServer(http.Dir(captureDir))))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	klog.InfoS("Starting HTTP server", "addr", addr, "captureDir", captureDir)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		klog.ErrorS(err, "HTTP server failed", "addr", addr)
	}
}
