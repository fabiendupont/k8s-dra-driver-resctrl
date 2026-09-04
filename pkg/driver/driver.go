package driver

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	drav1 "k8s.io/kubelet/pkg/apis/dra/v1"

	"github.com/fabiendupont/k8s-dra-driver-resctrl/pkg/resctrl"
)

// structuredAlloc records an on-demand resctrl group created in structured mode.
type structuredAlloc struct {
	cacheID      int
	ways         int
	cbm          uint64
	resctrlGroup string
	cdiSpecPath  string
}

// Driver implements the DRA kubelet plugin DRAPluginServer interface.
type Driver struct {
	drav1.UnimplementedDRAPluginServer
	driverName string
	nodeName   string
	client     kubernetes.Interface
	state      *AllocationState
	publisher  *SlicePublisher

	// Structured-parameters mode (DRAPartitionableDevices).
	caps        DRACapabilities
	wayAlloc    *WayAllocator
	catInfo     *resctrl.CATInfo
	hookBinPath string
	cdiDir      string

	// Per-claim allocation map for structured mode.
	structuredMu     sync.Mutex
	structuredAllocs map[string]*structuredAlloc // claimUID → alloc
}

// NewDriver creates a new DRA driver instance.
// In basic mode, wayAlloc and catInfo may be nil.
func NewDriver(
	driverName, nodeName string,
	client kubernetes.Interface,
	state *AllocationState,
	publisher *SlicePublisher,
	caps DRACapabilities,
	wayAlloc *WayAllocator,
	catInfo *resctrl.CATInfo,
	hookBinPath, cdiDir string,
) *Driver {
	return &Driver{
		driverName:       driverName,
		nodeName:         nodeName,
		client:           client,
		state:            state,
		publisher:        publisher,
		caps:             caps,
		wayAlloc:         wayAlloc,
		catInfo:          catInfo,
		hookBinPath:      hookBinPath,
		cdiDir:           cdiDir,
		structuredAllocs: make(map[string]*structuredAlloc),
	}
}

func (d *Driver) NodePrepareResources(ctx context.Context, req *drav1.NodePrepareResourcesRequest) (*drav1.NodePrepareResourcesResponse, error) {
	start := time.Now()
	defer PrepareDuration.Observe(time.Since(start).Seconds())

	resp := &drav1.NodePrepareResourcesResponse{
		Claims: make(map[string]*drav1.NodePrepareResourceResponse),
	}
	for _, claim := range req.Claims {
		if d.caps.PartitionableDevices {
			resp.Claims[claim.Uid] = d.prepareClaimStructured(ctx, claim)
		} else {
			resp.Claims[claim.Uid] = d.prepareClaim(ctx, claim)
		}
	}
	return resp, nil
}

// prepareClaim handles basic (inventory) mode.
func (d *Driver) prepareClaim(ctx context.Context, claim *drav1.Claim) *drav1.NodePrepareResourceResponse {
	rc, err := d.client.ResourceV1().ResourceClaims(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
	if err != nil {
		PrepareTotal.WithLabelValues("error").Inc()
		return &drav1.NodePrepareResourceResponse{
			Error: fmt.Sprintf("fetching ResourceClaim %s/%s: %v", claim.Namespace, claim.Name, err),
		}
	}
	if rc.Status.Allocation == nil {
		PrepareTotal.WithLabelValues("error").Inc()
		return &drav1.NodePrepareResourceResponse{
			Error: fmt.Sprintf("ResourceClaim %s/%s has no allocation", claim.Namespace, claim.Name),
		}
	}

	var devices []*drav1.Device
	for _, result := range rc.Status.Allocation.Devices.Results {
		if result.Driver != d.driverName || result.Pool != d.nodeName {
			continue
		}
		if err := d.state.Allocate(result.Device, claim.Uid); err != nil {
			klog.ErrorS(err, "Failed to allocate partition", "device", result.Device, "claim", claim.Uid)
			continue
		}
		devices = append(devices, &drav1.Device{
			RequestNames: []string{result.Request},
			PoolName:     result.Pool,
			DeviceName:   result.Device,
			CdiDeviceIds: []string{CDIDeviceID(result.Device)},
		})
	}

	if len(devices) == 0 {
		PrepareTotal.WithLabelValues("error").Inc()
		return &drav1.NodePrepareResourceResponse{
			Error: fmt.Sprintf("no devices for driver %s in claim %s", d.driverName, claim.Uid),
		}
	}
	PartitionsAllocated.Set(float64(d.state.AllocatedCount()))
	PrepareTotal.WithLabelValues("success").Inc()
	klog.InfoS("Prepared claim", "claim", claim.Uid, "partitions", len(devices))
	return &drav1.NodePrepareResourceResponse{Devices: devices}
}

// prepareClaimStructured handles structured-parameters mode: allocates CBM bits
// on demand and creates the resctrl group for this claim.
func (d *Driver) prepareClaimStructured(ctx context.Context, claim *drav1.Claim) *drav1.NodePrepareResourceResponse {
	rc, err := d.client.ResourceV1().ResourceClaims(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
	if err != nil {
		PrepareTotal.WithLabelValues("error").Inc()
		return &drav1.NodePrepareResourceResponse{
			Error: fmt.Sprintf("fetching ResourceClaim %s/%s: %v", claim.Namespace, claim.Name, err),
		}
	}
	if rc.Status.Allocation == nil {
		PrepareTotal.WithLabelValues("error").Inc()
		return &drav1.NodePrepareResourceResponse{
			Error: fmt.Sprintf("ResourceClaim %s/%s has no allocation", claim.Namespace, claim.Name),
		}
	}

	var devices []*drav1.Device
	for _, result := range rc.Status.Allocation.Devices.Results {
		if result.Driver != d.driverName || result.Pool != d.nodeName {
			continue
		}
		cacheID, ways, parseErr := parseCacheWayDeviceName(result.Device)
		if parseErr != nil {
			// Could be an MBA device — skip (handled separately).
			continue
		}

		cbm, allocErr := d.wayAlloc.Allocate(cacheID, ways)
		if allocErr != nil {
			PrepareTotal.WithLabelValues("error").Inc()
			return &drav1.NodePrepareResourceResponse{
				Error: fmt.Sprintf("allocating ways for %s: %v", result.Device, allocErr),
			}
		}

		cbmHex := fmt.Sprintf("%x", cbm)
		groupName := fmt.Sprintf("dra-c%d-%s", cacheID, sanitizeUID(claim.Uid))
		mbaThrottle := 0
		if d.catInfo != nil {
			// MBA throttle is not per-claim in structured mode; use 0 (no throttle).
			// Users claiming an MBA device control bandwidth separately.
			_ = mbaThrottle
		}
		schemata := buildStructuredSchemata(cacheID, cbmHex, 0)
		if createErr := resctrl.CreateGroup(groupName, schemata); createErr != nil {
			d.wayAlloc.Release(cacheID, cbm)
			PrepareTotal.WithLabelValues("error").Inc()
			return &drav1.NodePrepareResourceResponse{
				Error: fmt.Sprintf("creating resctrl group %s: %v", groupName, createErr),
			}
		}
		ResctrlGroupCreateTotal.WithLabelValues("success").Inc()

		cdiDeviceID, specPath, cdiErr := writeClaimCDISpec(d.cdiDir, d.hookBinPath, claim.Uid, groupName, cacheID, ways, cbmHex)
		if cdiErr != nil {
			_ = resctrl.DeleteGroup(groupName)
			d.wayAlloc.Release(cacheID, cbm)
			PrepareTotal.WithLabelValues("error").Inc()
			return &drav1.NodePrepareResourceResponse{
				Error: fmt.Sprintf("writing CDI spec for claim %s: %v", claim.Uid, cdiErr),
			}
		}

		d.structuredMu.Lock()
		d.structuredAllocs[claim.Uid] = &structuredAlloc{
			cacheID: cacheID, ways: ways, cbm: cbm,
			resctrlGroup: groupName, cdiSpecPath: specPath,
		}
		d.structuredMu.Unlock()

		devices = append(devices, &drav1.Device{
			RequestNames: []string{result.Request},
			PoolName:     result.Pool,
			DeviceName:   result.Device,
			CdiDeviceIds: []string{cdiDeviceID},
		})
	}

	if len(devices) == 0 {
		PrepareTotal.WithLabelValues("error").Inc()
		return &drav1.NodePrepareResourceResponse{
			Error: fmt.Sprintf("no cache-way devices for driver %s in claim %s", d.driverName, claim.Uid),
		}
	}
	PrepareTotal.WithLabelValues("success").Inc()
	klog.InfoS("Prepared claim (structured)", "claim", claim.Uid, "devices", len(devices))
	return &drav1.NodePrepareResourceResponse{Devices: devices}
}

func (d *Driver) NodeUnprepareResources(ctx context.Context, req *drav1.NodeUnprepareResourcesRequest) (*drav1.NodeUnprepareResourcesResponse, error) {
	start := time.Now()
	defer UnprepareDuration.Observe(time.Since(start).Seconds())

	resp := &drav1.NodeUnprepareResourcesResponse{
		Claims: make(map[string]*drav1.NodeUnprepareResourceResponse),
	}
	for _, claim := range req.Claims {
		if d.caps.PartitionableDevices {
			resp.Claims[claim.Uid] = d.unprepareClaimStructured(claim)
		} else {
			resp.Claims[claim.Uid] = d.unprepareClaim(ctx, claim)
		}
	}
	return resp, nil
}

func (d *Driver) unprepareClaim(_ context.Context, claim *drav1.Claim) *drav1.NodeUnprepareResourceResponse {
	released := d.state.ReleaseByClaimUID(claim.Uid)
	PartitionsAllocated.Set(float64(d.state.AllocatedCount()))
	UnprepareTotal.WithLabelValues("success").Inc()
	klog.InfoS("Unprepared claim", "claim", claim.Uid, "partitions", released)
	return &drav1.NodeUnprepareResourceResponse{}
}

func (d *Driver) unprepareClaimStructured(claim *drav1.Claim) *drav1.NodeUnprepareResourceResponse {
	d.structuredMu.Lock()
	alloc, ok := d.structuredAllocs[claim.Uid]
	if ok {
		delete(d.structuredAllocs, claim.Uid)
	}
	d.structuredMu.Unlock()

	if !ok {
		klog.V(4).InfoS("No structured alloc found for claim during unprepare", "claim", claim.Uid)
		UnprepareTotal.WithLabelValues("success").Inc()
		return &drav1.NodeUnprepareResourceResponse{}
	}

	if err := resctrl.DeleteGroup(alloc.resctrlGroup); err != nil {
		ResctrlGroupDeleteTotal.WithLabelValues("error").Inc()
		klog.ErrorS(err, "Failed to delete resctrl group", "group", alloc.resctrlGroup)
	} else {
		ResctrlGroupDeleteTotal.WithLabelValues("success").Inc()
	}
	d.wayAlloc.Release(alloc.cacheID, alloc.cbm)
	removeClaimCDISpec(alloc.cdiSpecPath)

	UnprepareTotal.WithLabelValues("success").Inc()
	klog.InfoS("Unprepared claim (structured)", "claim", claim.Uid, "group", alloc.resctrlGroup)
	return &drav1.NodeUnprepareResourceResponse{}
}

// StructuredTargets returns the current on-demand allocations as MonitorTargets.
// Safe to call concurrently.
func (d *Driver) StructuredTargets() []MonitorTarget {
	d.structuredMu.Lock()
	defer d.structuredMu.Unlock()
	targets := make([]MonitorTarget, 0, len(d.structuredAllocs))
	for claimUID, alloc := range d.structuredAllocs {
		targets = append(targets, MonitorTarget{
			PartitionName: fmt.Sprintf("claim-%s", claimUID[:8]),
			Group:         alloc.resctrlGroup,
			CacheGroupID:  alloc.cacheID,
		})
	}
	return targets
}

// RecoverStructuredAllocs rebuilds structuredAllocs from existing resctrl groups
// at driver startup after a crash. Call before accepting gRPC connections.
func (d *Driver) RecoverStructuredAllocs(resctrlRoot string) {
	if !d.caps.PartitionableDevices {
		return
	}
	if err := d.wayAlloc.RebuildFromResctrl(resctrlRoot, "dra-c"); err != nil {
		klog.ErrorS(err, "Failed to rebuild WayAllocator from resctrl — some ways may be double-allocated until pods exit")
	}
	klog.InfoS("WayAllocator state rebuilt from resctrl")
}

// parseCacheWayDeviceName parses "cache0-5way" into cacheID=0, ways=5.
func parseCacheWayDeviceName(name string) (cacheID, ways int, err error) {
	// Format: cache<C>-<W>way
	if !strings.HasPrefix(name, "cache") || !strings.HasSuffix(name, "way") {
		return 0, 0, fmt.Errorf("not a cache-way device name: %q", name)
	}
	inner := strings.TrimPrefix(name, "cache")
	inner = strings.TrimSuffix(inner, "way")
	parts := strings.SplitN(inner, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("malformed cache-way device name: %q", name)
	}
	cacheID, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("bad cacheID in %q: %w", name, err)
	}
	ways, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("bad way count in %q: %w", name, err)
	}
	return cacheID, ways, nil
}

// sanitizeUID strips hyphens from a UUID so it's safe to use in a filename/dirname.
func sanitizeUID(uid string) string {
	return strings.ReplaceAll(uid, "-", "")
}

// buildStructuredSchemata formats L3 (and optionally MB) schemata for a single domain.
func buildStructuredSchemata(cacheID int, cbmHex string, mbaThrottle int) string {
	lines := []string{fmt.Sprintf("L3:%d=%s", cacheID, cbmHex)}
	if mbaThrottle > 0 {
		lines = append(lines, fmt.Sprintf("MB:%d=%d", cacheID, mbaThrottle))
	}
	return strings.Join(lines, "\n")
}
