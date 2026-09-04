package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	drav1 "k8s.io/kubelet/pkg/apis/dra/v1"

	"github.com/fabiendupont/k8s-dra-driver-resctrl/pkg/cache"
	"github.com/fabiendupont/k8s-dra-driver-resctrl/pkg/driver"
	"github.com/fabiendupont/k8s-dra-driver-resctrl/pkg/resctrl"
)

const driverName = "resctrl.fabiendupont.io"

func main() {
	// CDI hook mode: invoked by CRI-O as an OCI hook to assign the container
	// PID to a resctrl group. Must run before flag.Parse().
	if len(os.Args) > 1 && os.Args[1] == "--cdi-hook" {
		if err := runCDIHook(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "cdi-hook: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	var (
		socketPath     string
		registryDir    string
		nodeName       string
		partitionCount int
		partitionSizes string
		resctrlRoot    string
		healthPort     int
		cdiDir         string
		mbaBandwidth   int
	)

	flag.StringVar(&socketPath, "socket", "/var/lib/kubelet/plugins/resctrl.fabiendupont.io/plugin.sock", "DRA plugin gRPC socket path")
	flag.StringVar(&registryDir, "registry-dir", "/var/lib/kubelet/plugins_registry", "Kubelet plugin registry directory")
	flag.StringVar(&nodeName, "node-name", "", "Kubernetes node name")
	flag.IntVar(&partitionCount, "partition-count", 4, "Number of equal cache partitions per cache domain (ignored when --partition-sizes is set)")
	flag.StringVar(&partitionSizes, "partition-sizes", "", "Heterogeneous partition sizes as ways:count[,...] e.g. 1:8,2:4,4:2 (overrides --partition-count)")
	flag.StringVar(&resctrlRoot, "resctrl-root", "/sys/fs/resctrl", "Root path of the resctrl filesystem")
	flag.IntVar(&healthPort, "health-port", 8081, "Port for health check endpoints (/healthz, /readyz)")
	flag.StringVar(&cdiDir, "cdi-dir", "/var/run/cdi", "Directory for CDI spec files")
	flag.IntVar(&mbaBandwidth, "mba-bandwidth", -1, "Per-partition MBA bandwidth cap (-1=auto: 100 in percent mode, skip in MBps mode)")
	var smbaBandwidth int
	flag.IntVar(&smbaBandwidth, "smba-bandwidth", -1, "Per-partition SMBA slow-memory bandwidth cap (-1=auto)")
	var monitorInterval time.Duration
	flag.DurationVar(&monitorInterval, "monitor-interval", 10*time.Second, "Interval for scraping resctrl mon_data (CMT/MBM monitoring)")

	klog.InitFlags(nil)
	flag.Parse()

	if nodeName == "" {
		nodeName = os.Getenv("NODE_NAME")
	}
	if nodeName == "" {
		klog.Fatal("--node-name or NODE_NAME required")
	}

	resctrl.SetRoot(resctrlRoot)

	if !resctrl.IsAvailable() {
		klog.Fatalf("resctrl/CAT not available at %s", resctrlRoot)
	}

	info, err := resctrl.Info()
	if err != nil {
		klog.Fatalf("Failed to read resctrl info: %v", err)
	}

	klog.InfoS("Detected cache allocation capabilities",
		"numCLOSIDs", info.NumCLOSIDs,
		"totalWays", info.NumWays(),
		"cbmMask", fmt.Sprintf("%x", info.CBMMask),
		"minCBMBits", info.MinCBMBits,
		"cacheIDs", info.CacheIDs,
	)

	cacheToNUMA, numaErr := cache.LookupCacheNUMA("", "")
	if numaErr != nil {
		klog.InfoS("NUMA lookup unavailable, cache partitions will have numaNode=-1", "error", numaErr)
	}

	cacheToSize, sizeErr := cache.LookupCacheSizes("")
	if sizeErr != nil {
		klog.InfoS("Cache size lookup unavailable, cacheSizeBytes will be 0", "error", sizeErr)
	}

	cacheToCPU, cpuErr := cache.LookupCacheCPUList("")
	if cpuErr != nil {
		klog.InfoS("CPU list lookup unavailable, cpuList attribute will be empty", "error", cpuErr)
	}

	// Build partition specs: explicit --partition-sizes takes priority over --partition-count.
	var specs []cache.PartitionSpec
	if partitionSizes != "" {
		specs, err = cache.ParsePartitionSizes(partitionSizes)
		if err != nil {
			klog.Fatalf("Invalid --partition-sizes: %v", err)
		}
	} else {
		specs = cache.SpecsFromCount(partitionCount, info.NumWays())
	}
	klog.InfoS("Partition specs", "specs", specs)

	perCache, err := cache.PartitionCache(info, specs, cacheToNUMA, cacheToSize, cacheToCPU)
	if err != nil {
		klog.Fatalf("Failed to partition cache: %v", err)
	}

	// Configure MBA if available.
	if resctrl.IsMBAAvailable() {
		mbaInfo, mbaErr := resctrl.MBA()
		if mbaErr != nil {
			klog.InfoS("MBA info unavailable, skipping MBA devices", "error", mbaErr)
		} else {
			throttle := mbaBandwidth
			if throttle < 0 {
				if mbaInfo.CtrlInPercentages {
					throttle = 100
				} else {
					throttle = 0
				}
			}
			if throttle > 0 {
				cache.ApplyMBA(perCache, throttle, mbaInfo.MBAMode())
				klog.InfoS("MBA configured", "mode", mbaInfo.MBAMode(), "throttle", throttle)
			} else {
				klog.InfoS("MBA available but not configured (set --mba-bandwidth to enable in MBps mode)")
			}
		}
	} else {
		klog.InfoS("MBA not available on this node, skipping MBA devices")
	}

	// Configure SMBA if available.
	if resctrl.IsSMBAAvailable() {
		smbaInfo, smbaErr := resctrl.SMBA()
		if smbaErr != nil {
			klog.InfoS("SMBA info unavailable, skipping SMBA devices", "error", smbaErr)
		} else {
			throttle := smbaBandwidth
			if throttle < 0 {
				if smbaInfo.CtrlInPercentages {
					throttle = 100
				} else {
					throttle = 0
				}
			}
			if throttle > 0 {
				cache.ApplySMBA(perCache, throttle, smbaInfo.MBAMode())
				klog.InfoS("SMBA configured", "mode", smbaInfo.MBAMode(), "throttle", throttle)
			} else {
				klog.InfoS("SMBA available but not configured (set --smba-bandwidth to enable in MBps mode)")
			}
		}
	} else {
		klog.InfoS("SMBA not available on this node, skipping SMBA devices")
	}

	// Configure L2 CAT if available (pass-through full mask so schemata writes succeed).
	if resctrl.IsL2Available() {
		l2Info, l2Err := resctrl.InfoL2()
		if l2Err != nil {
			klog.InfoS("L2 CAT info unavailable", "error", l2Err)
		} else {
			cache.ApplyL2(perCache, l2Info)
			klog.InfoS("L2 CAT configured", "totalWays", l2Info.NumWays(), "cbmMask", fmt.Sprintf("%x", l2Info.CBMMask))
		}
	} else {
		klog.InfoS("L2 CAT not available on this node")
	}

	// Install hook binary (needed in both modes).
	pluginDir := filepath.Dir(socketPath)
	if err := os.MkdirAll(pluginDir, 0750); err != nil {
		klog.Fatalf("Failed to create plugin directory: %v", err)
	}
	hookBinaryPath, err := driver.InstallHookBinary(pluginDir)
	if err != nil {
		klog.Fatalf("Failed to install hook binary: %v", err)
	}

	driver.RegisterMetrics()

	kubeClient, err := buildKubeClient()
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// Probe DRA feature gates.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	caps := driver.ProbeFeatureGates(ctx, kubeClient, driverName, nodeName)

	publisher := driver.NewSlicePublisher(kubeClient, driverName, nodeName, caps)

	var (
		state    *driver.AllocationState
		wayAlloc *driver.WayAllocator
	)

	if caps.PartitionableDevices {
		// Structured mode: no pre-created groups; on-demand allocation per claim.
		klog.InfoS("DRA mode: structured (DRAPartitionableDevices enabled)")

		totalWaysPerDomain := make(map[int]int)
		for _, cacheID := range info.CacheIDs {
			totalWaysPerDomain[cacheID] = info.NumWays()
		}
		wayAlloc = driver.NewWayAllocator(totalWaysPerDomain)

		state = driver.NewAllocationState(nil) // empty; not used in structured mode
	} else {
		// Basic mode: pre-partition and pre-create resctrl groups at startup.
		klog.InfoS("DRA mode: basic inventory (pre-partitioned)")

		allPartitions := cache.AllPartitions(perCache)
		klog.InfoS("Partitioned cache domains",
			"cacheIDs", len(perCache),
			"totalPartitions", len(allPartitions),
		)
		driver.PartitionsTotal.Set(float64(len(allPartitions)))

		for _, parts := range perCache {
			for _, p := range parts {
				schemata := cache.FormatCombinedSchemata(p)
				if err := resctrl.CreateGroup(p.ResctrlGroup, schemata); err != nil {
					driver.ResctrlGroupCreateTotal.WithLabelValues("error").Inc()
					klog.Fatalf("Failed to create resctrl group %s: %v", p.ResctrlGroup, err)
				}
				driver.ResctrlGroupCreateTotal.WithLabelValues("success").Inc()
				klog.V(2).InfoS("Created resctrl group", "group", p.ResctrlGroup)
			}
		}
		defer func() {
			for _, parts := range perCache {
				for _, p := range parts {
					if err := resctrl.DeleteGroup(p.ResctrlGroup); err != nil {
						driver.ResctrlGroupDeleteTotal.WithLabelValues("error").Inc()
						klog.ErrorS(err, "Failed to delete resctrl group during shutdown", "group", p.ResctrlGroup)
					} else {
						driver.ResctrlGroupDeleteTotal.WithLabelValues("success").Inc()
					}
				}
			}
		}()

		state = driver.NewAllocationState(perCache)

		if err := driver.WriteCDISpecs(cdiDir, hookBinaryPath, perCache); err != nil {
			klog.Fatalf("Failed to write CDI specs: %v", err)
		}
		defer driver.CleanupCDISpecs(cdiDir)

		recovered, recoverErr := driver.RecoverAllocations(ctx, kubeClient, driverName, nodeName, state)
		if recoverErr != nil {
			klog.ErrorS(recoverErr, "Partial failure recovering allocations", "recovered", recovered)
		}
		if recovered > 0 {
			driver.PartitionsAllocated.Set(float64(recovered))
			klog.InfoS("Recovered partitions from previous allocations", "partitions", recovered)
		}
	}

	if err := publisher.PublishSlices(ctx, perCache); err != nil {
		klog.Fatalf("Failed to publish ResourceSlices: %v", err)
	}

	drv := driver.NewDriver(driverName, nodeName, kubeClient, state, publisher,
		caps, wayAlloc, info, hookBinaryPath, cdiDir)

	if caps.PartitionableDevices {
		drv.RecoverStructuredAllocs(resctrlRoot)
	}

	healthServer := driver.NewHealthServer(healthPort)
	go func() {
		if err := healthServer.Serve(ctx); err != nil {
			klog.Fatalf("Health server error: %v", err)
		}
	}()

	healthServer.MarkReady()

	registrar := driver.NewRegistrar(driverName, socketPath)
	go func() {
		if err := registrar.Serve(ctx, registryDir); err != nil {
			klog.Fatalf("Registration server error: %v", err)
		}
	}()

	// Start CMT/MBM monitoring if the hardware supports it.
	if resctrl.IsMonitoringAvailable() {
		klog.InfoS("CMT/MBM monitoring available", "features", resctrl.MonitoringFeatures(), "interval", monitorInterval)
		var targets func() []driver.MonitorTarget
		if caps.PartitionableDevices {
			targets = drv.StructuredTargets
		} else {
			allParts := cache.AllPartitions(perCache)
			fixed := make([]driver.MonitorTarget, 0, len(allParts))
			for _, p := range allParts {
				fixed = append(fixed, driver.MonitorTarget{
					PartitionName: p.ID,
					Group:         p.ResctrlGroup,
					CacheGroupID:  p.CacheID,
				})
			}
			targets = func() []driver.MonitorTarget { return fixed }
		}
		mon := driver.NewMonitor(resctrlRoot, monitorInterval, targets)
		go mon.Run(ctx)
	} else {
		klog.InfoS("CMT/MBM monitoring not available on this node")
	}

	grpcErr := runGRPCServer(ctx, socketPath, drv)

	// Use a fresh context for cleanup: the signal context is already cancelled
	// by the time the gRPC server returns (or immediately on klog.Fatalf).
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()
	if err := publisher.DeleteSlice(cleanupCtx); err != nil {
		klog.ErrorS(err, "Failed to delete ResourceSlice during shutdown")
	}

	if grpcErr != nil {
		klog.Fatalf("gRPC server error: %v", grpcErr)
	}
}

// runCDIHook is the CDI hook entry point. It reads OCI state from stdin to
// get the container PID, then writes that PID to the resctrl group's tasks file.
func runCDIHook(args []string) error {
	var group string
	for _, arg := range args {
		if strings.HasPrefix(arg, "--group=") {
			group = arg[len("--group="):]
		}
	}
	if group == "" {
		return fmt.Errorf("--group is required")
	}

	var state struct {
		PID int `json:"pid"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&state); err != nil {
		return fmt.Errorf("reading OCI state: %w", err)
	}
	if state.PID == 0 {
		return fmt.Errorf("OCI state has no PID")
	}

	if err := resctrl.AssignPID(group, state.PID); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: assigning pid %d to resctrl group %s: %v\n", state.PID, group, err)
		return fmt.Errorf("assigning pid %d to resctrl group %s: %w", state.PID, group, err)
	}

	fmt.Fprintf(os.Stderr, "Assigned container pid %d to resctrl group %s\n", state.PID, group)
	return nil
}

func runGRPCServer(ctx context.Context, socketPath string, drv *driver.Driver) error {
	_ = os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}

	server := grpc.NewServer(
		grpc.MaxConcurrentStreams(64),
		grpc.MaxRecvMsgSize(4<<20), // 4 MiB
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
			Time:              30 * time.Second,
			Timeout:           10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	drav1.RegisterDRAPluginServer(server, drv)

	go func() {
		<-ctx.Done()
		klog.InfoS("Shutting down gRPC server")
		server.GracefulStop()
	}()

	klog.InfoS("gRPC server listening", "socket", socketPath)
	return server.Serve(listener)
}

func buildKubeClient() (kubernetes.Interface, error) {
	var client kubernetes.Interface
	var lastErr error

	for attempt := 0; attempt < 10; attempt++ {
		config, err := rest.InClusterConfig()
		if err != nil {
			lastErr = fmt.Errorf("building in-cluster config: %w", err)
			klog.InfoS("Waiting for in-cluster config", "attempt", attempt+1, "error", err)
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}

		client, err = kubernetes.NewForConfig(config)
		if err != nil {
			lastErr = fmt.Errorf("creating kubernetes client: %w", err)
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}

		_, err = client.Discovery().ServerVersion()
		if err != nil {
			lastErr = fmt.Errorf("verifying API server connectivity: %w", err)
			klog.InfoS("API server not reachable yet", "attempt", attempt+1, "error", err)
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}

		return client, nil
	}
	return nil, lastErr
}
