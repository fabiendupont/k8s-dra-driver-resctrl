package driver

import (
	"context"
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/fabiendupont/k8s-dra-driver-resctrl/pkg/cache"
)

// formatCacheSize returns a human-readable cache size string (e.g. "5MiB", "2.5MiB", "512KiB").
func formatCacheSize(bytes int64) string {
	if bytes <= 0 {
		return "0B"
	}
	const (
		kib = int64(1024)
		mib = kib * 1024
		gib = mib * 1024
	)
	if bytes%gib == 0 {
		return fmt.Sprintf("%dGiB", bytes/gib)
	}
	if bytes%mib == 0 {
		return fmt.Sprintf("%dMiB", bytes/mib)
	}
	// One decimal place MiB (e.g. 2.5MiB): bytes * 10 must be exactly divisible by MiB.
	if bytes >= mib && (bytes*10)%mib == 0 {
		return fmt.Sprintf("%.1fMiB", float64(bytes)/float64(mib))
	}
	if bytes%kib == 0 {
		return fmt.Sprintf("%dKiB", bytes/kib)
	}
	return fmt.Sprintf("%dB", bytes)
}

// SlicePublisher publishes and updates ResourceSlices for cache partition devices.
type SlicePublisher struct {
	client     kubernetes.Interface
	driverName string
	nodeName   string
	caps       DRACapabilities
}

// NewSlicePublisher creates a publisher that manages ResourceSlices.
func NewSlicePublisher(client kubernetes.Interface, driverName, nodeName string, caps DRACapabilities) *SlicePublisher {
	return &SlicePublisher{
		client:     client,
		driverName: driverName,
		nodeName:   nodeName,
		caps:       caps,
	}
}

// PublishSlices creates or updates the ResourceSlice for cache partitions.
// In structured mode (DRAPartitionableDevices), it publishes SharedCounters and
// per-way-count devices; in basic mode it publishes the pre-partitioned inventory.
func (sp *SlicePublisher) PublishSlices(ctx context.Context, perCache [][]*cache.CachePartition) error {
	var devices []resourceapi.Device
	var sharedCounters []resourceapi.CounterSet

	if sp.caps.PartitionableDevices && len(perCache) > 0 {
		devices, sharedCounters = sp.buildStructuredDevices(perCache)
	} else {
		devices = sp.buildDevices(perCache)
	}

	ownerRef, err := sp.nodeOwnerReference(ctx)
	if err != nil {
		klog.ErrorS(err, "Could not set Node owner reference on ResourceSlice")
	}

	slice := &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fmt.Sprintf("%s-%s", sp.nodeName, sp.driverName),
			OwnerReferences: ownerRef,
		},
		Spec: resourceapi.ResourceSliceSpec{
			Driver:         sp.driverName,
			NodeName:       &sp.nodeName,
			SharedCounters: sharedCounters,
			Pool: resourceapi.ResourcePool{
				Name:               sp.nodeName,
				ResourceSliceCount: 1,
			},
			Devices: devices,
		},
	}

	_, err = sp.client.ResourceV1().ResourceSlices().Create(ctx, slice, metav1.CreateOptions{})
	if err == nil {
		klog.InfoS("Created ResourceSlice", "name", slice.Name, "devices", len(devices))
		return nil
	}
	if !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating ResourceSlice: %w", err)
	}

	existing, getErr := sp.client.ResourceV1().ResourceSlices().Get(ctx, slice.Name, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("fetching existing ResourceSlice after conflict: %w", getErr)
	}

	slice.ResourceVersion = existing.ResourceVersion
	if slice.OwnerReferences == nil && len(existing.OwnerReferences) > 0 {
		slice.OwnerReferences = existing.OwnerReferences
	}
	_, err = sp.client.ResourceV1().ResourceSlices().Update(ctx, slice, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating ResourceSlice: %w", err)
	}
	klog.InfoS("Updated ResourceSlice", "name", slice.Name, "devices", len(devices))
	return nil
}

func (sp *SlicePublisher) buildDevices(perCache [][]*cache.CachePartition) []resourceapi.Device {
	all := cache.AllPartitions(perCache)
	devices := make([]resourceapi.Device, 0, len(all)*2)

	for _, p := range all {
		// CAT cache partition device.
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"cacheGroupID": {
				IntValue: int64Ptr(int64(p.CacheID)),
			},
			"cacheWays": {
				IntValue: int64Ptr(int64(p.Ways)),
			},
			"cacheTotalWays": {
				IntValue: int64Ptr(int64(p.TotalWays)),
			},
			"cacheSizeBytes": {
				IntValue: int64Ptr(p.SizeBytes),
			},
			"cbmHex": {
				StringValue: stringPtr(p.CBM),
			},
			"cacheLevel": {
				StringValue: stringPtr(p.Level),
			},
			"numaNode": {
				IntValue: int64Ptr(int64(p.NUMANode)),
			},
		}
		if p.WaySizeBytes > 0 {
			attrs["cacheWaySizeBytes"] = resourceapi.DeviceAttribute{
				IntValue: int64Ptr(p.WaySizeBytes),
			}
			attrs["cacheSizeLabel"] = resourceapi.DeviceAttribute{
				StringValue: stringPtr(formatCacheSize(p.SizeBytes)),
			}
			if p.MinCBMBits > 0 {
				attrs["minPartitionSizeBytes"] = resourceapi.DeviceAttribute{
					IntValue: int64Ptr(int64(p.MinCBMBits) * p.WaySizeBytes),
				}
			}
		}
		if p.CPUList != "" {
			attrs["cpuList"] = resourceapi.DeviceAttribute{StringValue: stringPtr(p.CPUList)}
		}
		if p.ResctrlGroup != "" {
			attrs["resctrlGroup"] = resourceapi.DeviceAttribute{StringValue: stringPtr(p.ResctrlGroup)}
		}
		dev := resourceapi.Device{Name: p.ID, Attributes: attrs}
		devices = append(devices, dev)

		// L2 CAT attributes on the cache device (informational; same CLOSID group).
		if p.L2Ways > 0 {
			attrs["l2Ways"] = resourceapi.DeviceAttribute{IntValue: int64Ptr(int64(p.L2Ways))}
			attrs["l2TotalWays"] = resourceapi.DeviceAttribute{IntValue: int64Ptr(int64(p.L2TotalWays))}
			attrs["l2CbmHex"] = resourceapi.DeviceAttribute{StringValue: stringPtr(p.L2CBM)}
		}

		// MBA bandwidth device — shares the same resctrl group as the CAT partition.
		if p.MBAThrottle > 0 {
			mbaDev := resourceapi.Device{
				Name: cache.MBADeviceID(p),
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"mbaDomainID":  {IntValue: int64Ptr(int64(p.CacheID))},
					"mbaBandwidth": {IntValue: int64Ptr(int64(p.MBAThrottle))},
					"mbaMode":      {StringValue: stringPtr(p.MBAMode)},
					"numaNode":     {IntValue: int64Ptr(int64(p.NUMANode))},
					"resctrlGroup": {StringValue: stringPtr(p.ResctrlGroup)},
				},
			}
			devices = append(devices, mbaDev)
		}

		// SMBA device — slow memory bandwidth (HBM systems), same resctrl group.
		if p.SMBAThrottle > 0 {
			smbaDev := resourceapi.Device{
				Name: cache.SMBADeviceID(p),
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"smbaDomainID":  {IntValue: int64Ptr(int64(p.CacheID))},
					"smbaBandwidth": {IntValue: int64Ptr(int64(p.SMBAThrottle))},
					"smbaMode":      {StringValue: stringPtr(p.SMBAMode)},
					"numaNode":      {IntValue: int64Ptr(int64(p.NUMANode))},
					"resctrlGroup":  {StringValue: stringPtr(p.ResctrlGroup)},
				},
			}
			devices = append(devices, smbaDev)
		}
	}

	return devices
}


// SliceName returns the ResourceSlice name for this publisher.
func (sp *SlicePublisher) SliceName() string {
	return fmt.Sprintf("%s-%s", sp.nodeName, sp.driverName)
}

// DeleteSlice removes the ResourceSlice from the API server.
func (sp *SlicePublisher) DeleteSlice(ctx context.Context) error {
	name := sp.SliceName()
	err := sp.client.ResourceV1().ResourceSlices().Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("deleting ResourceSlice %s: %w", name, err)
	}
	klog.InfoS("Deleted ResourceSlice", "name", name)
	return nil
}

func (sp *SlicePublisher) nodeOwnerReference(ctx context.Context) ([]metav1.OwnerReference, error) {
	node, err := sp.client.CoreV1().Nodes().Get(ctx, sp.nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting node %s: %w", sp.nodeName, err)
	}
	return []metav1.OwnerReference{
		{
			APIVersion: "v1",
			Kind:       "Node",
			Name:       node.Name,
			UID:        node.UID,
		},
	}, nil
}

// buildStructuredDevices produces SharedCounters and per-way-count devices
// for use when DRAPartitionableDevices is enabled. One device is published
// for each (cacheID, wayCount) pair; AllowMultipleAllocations lets the
// scheduler allocate the same device to concurrent claims, bounded by the
// shared way-pool counter.
func (sp *SlicePublisher) buildStructuredDevices(perCache [][]*cache.CachePartition) ([]resourceapi.Device, []resourceapi.CounterSet) {
	if len(perCache) == 0 {
		return nil, nil
	}

	// Build the shared counter set: one counter per cache domain.
	counters := make(map[string]resourceapi.Counter)
	for _, parts := range perCache {
		if len(parts) == 0 {
			continue
		}
		p := parts[0]
		key := fmt.Sprintf("cache%d", p.CacheID)
		counters[key] = resourceapi.Counter{
			Value: *resource.NewQuantity(int64(p.TotalWays), resource.DecimalSI),
		}
	}
	sharedCounters := []resourceapi.CounterSet{{Name: "way-pool", Counters: counters}}

	// Build one device per (cacheID, wayCount).
	var devices []resourceapi.Device
	for _, parts := range perCache {
		if len(parts) == 0 {
			continue
		}
		ref := parts[0]
		totalWays := ref.TotalWays
		counterKey := fmt.Sprintf("cache%d", ref.CacheID)

		for ways := 1; ways <= totalWays; ways++ {
			sizeBytes := ref.WaySizeBytes * int64(ways)
			attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"cacheGroupID": {IntValue: int64Ptr(int64(ref.CacheID))},
				"cacheWays":     {IntValue: int64Ptr(int64(ways))},
				"cacheTotalWays": {IntValue: int64Ptr(int64(totalWays))},
				"cacheSizeBytes": {IntValue: int64Ptr(sizeBytes)},
				"cacheLevel":    {StringValue: stringPtr("L3")},
				"numaNode":      {IntValue: int64Ptr(int64(ref.NUMANode))},
			}
			if ref.WaySizeBytes > 0 {
				attrs["cacheWaySizeBytes"] = resourceapi.DeviceAttribute{IntValue: int64Ptr(ref.WaySizeBytes)}
				attrs["cacheSizeLabel"] = resourceapi.DeviceAttribute{StringValue: stringPtr(formatCacheSize(sizeBytes))}
				if ref.MinCBMBits > 0 {
					attrs["minPartitionSizeBytes"] = resourceapi.DeviceAttribute{
						IntValue: int64Ptr(int64(ref.MinCBMBits) * ref.WaySizeBytes),
					}
				}
			}
			if ref.CPUList != "" {
				attrs["cpuList"] = resourceapi.DeviceAttribute{StringValue: stringPtr(ref.CPUList)}
			}

			allowMulti := true
			devices = append(devices, resourceapi.Device{
				Name:       fmt.Sprintf("cache%d-%dway", ref.CacheID, ways),
				Attributes: attrs,
				ConsumesCounters: []resourceapi.DeviceCounterConsumption{{
					CounterSet: "way-pool",
					Counters: map[string]resourceapi.Counter{
						counterKey: {Value: *resource.NewQuantity(int64(ways), resource.DecimalSI)},
					},
				}},
				AllowMultipleAllocations: &allowMulti,
			})
		}
	}
	return devices, sharedCounters
}

func int64Ptr(v int64) *int64 {
	return &v
}

func stringPtr(v string) *string {
	return &v
}
