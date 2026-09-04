package driver

import (
	"context"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/fabiendupont/k8s-dra-driver-resctrl/pkg/cache"
)

const (
	testDriver = "resctrl.fabiendupont.io"
	testNode   = "node-1"
)

func testPartitions() [][]*cache.CachePartition {
	return [][]*cache.CachePartition{
		{
			{ID: "cache0-part0", ResctrlGroup: "dra-cache0-part0", Ways: 5, TotalWays: 20},
			{ID: "cache0-part1", ResctrlGroup: "dra-cache0-part1", Ways: 5, TotalWays: 20},
		},
	}
}

func TestFormatCacheSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{5 * 1024 * 1024, "5MiB"},
		{2621440, "2.5MiB"},   // 2.5 MiB (12-way 30MB cache: 2.5MB/way)
		{512 * 1024, "512KiB"},
		{1024 * 1024 * 1024, "1GiB"},
		{20 * 1024 * 1024, "20MiB"},
		{0, "0B"},
	}
	for _, tt := range tests {
		got := formatCacheSize(tt.bytes)
		if got != tt.want {
			t.Errorf("formatCacheSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

func TestPublishSlices_Create(t *testing.T) {
	client := fake.NewClientset()
	sp := NewSlicePublisher(client, testDriver, testNode, DRACapabilities{})

	if err := sp.PublishSlices(context.Background(), testPartitions()); err != nil {
		t.Fatalf("PublishSlices: %v", err)
	}

	sliceName := sp.SliceName()
	slice, err := client.ResourceV1().ResourceSlices().Get(context.Background(), sliceName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting slice: %v", err)
	}
	if len(slice.Spec.Devices) != 2 {
		t.Errorf("got %d devices, want 2", len(slice.Spec.Devices))
	}
}

func TestPublishSlices_Update(t *testing.T) {
	client := fake.NewClientset()
	sp := NewSlicePublisher(client, testDriver, testNode, DRACapabilities{})

	// Pre-create a slice so the second publish hits the update path.
	existing := &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: sp.SliceName()},
		Spec: resourceapi.ResourceSliceSpec{
			Driver:   testDriver,
			NodeName: func() *string { s := testNode; return &s }(),
			Pool:     resourceapi.ResourcePool{Name: testNode, ResourceSliceCount: 1},
		},
	}
	if _, err := client.ResourceV1().ResourceSlices().Create(context.Background(), existing, metav1.CreateOptions{}); err != nil {
		t.Fatalf("pre-creating slice: %v", err)
	}

	if err := sp.PublishSlices(context.Background(), testPartitions()); err != nil {
		t.Fatalf("PublishSlices on existing: %v", err)
	}

	slice, err := client.ResourceV1().ResourceSlices().Get(context.Background(), sp.SliceName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting updated slice: %v", err)
	}
	if len(slice.Spec.Devices) != 2 {
		t.Errorf("got %d devices after update, want 2", len(slice.Spec.Devices))
	}
}

func TestPublishSlices_StructuredMode(t *testing.T) {
	client := fake.NewClientset()
	caps := DRACapabilities{PartitionableDevices: true}
	sp := NewSlicePublisher(client, testDriver, testNode, caps)

	if err := sp.PublishSlices(context.Background(), testPartitions()); err != nil {
		t.Fatalf("PublishSlices structured: %v", err)
	}

	slice, err := client.ResourceV1().ResourceSlices().Get(context.Background(), sp.SliceName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting slice: %v", err)
	}

	// Should have SharedCounters.
	if len(slice.Spec.SharedCounters) == 0 {
		t.Fatal("structured mode: expected SharedCounters, got none")
	}

	// Should have one device per way count (1..20).
	if len(slice.Spec.Devices) != 20 {
		t.Fatalf("structured mode: got %d devices, want 20 (1..20 ways)", len(slice.Spec.Devices))
	}

	// Check that allowMultipleAllocations is set.
	for _, d := range slice.Spec.Devices {
		if d.AllowMultipleAllocations == nil || !*d.AllowMultipleAllocations {
			t.Errorf("device %s: AllowMultipleAllocations not set", d.Name)
		}
	}
}

func TestPublishSlices_WithMBA(t *testing.T) {
	client := fake.NewClientset()
	sp := NewSlicePublisher(client, testDriver, testNode, DRACapabilities{})

	// Partitions with MBA configured.
	perCache := [][]*cache.CachePartition{
		{
			{ID: "cache0-part0", ResctrlGroup: "dra-cache0-part0", Ways: 5, TotalWays: 20, CacheID: 0, Index: 0, MBAThrottle: 70, MBAMode: "percent"},
		},
	}

	if err := sp.PublishSlices(context.Background(), perCache); err != nil {
		t.Fatalf("PublishSlices: %v", err)
	}

	slice, err := client.ResourceV1().ResourceSlices().Get(context.Background(), sp.SliceName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting slice: %v", err)
	}

	// Expect 2 devices: cache0-part0 and mba0-part0.
	if len(slice.Spec.Devices) != 2 {
		t.Fatalf("got %d devices, want 2 (1 CAT + 1 MBA)", len(slice.Spec.Devices))
	}

	names := map[string]bool{}
	for _, d := range slice.Spec.Devices {
		names[d.Name] = true
	}
	if !names["cache0-part0"] {
		t.Error("missing cache0-part0 device")
	}
	if !names["mba0-part0"] {
		t.Error("missing mba0-part0 device")
	}
}

func TestDeleteSlice(t *testing.T) {
	client := fake.NewClientset()
	sp := NewSlicePublisher(client, testDriver, testNode, DRACapabilities{})
	_ = sp.PublishSlices(context.Background(), testPartitions())

	if err := sp.DeleteSlice(context.Background()); err != nil {
		t.Fatalf("DeleteSlice: %v", err)
	}

	slices, err := client.ResourceV1().ResourceSlices().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing slices: %v", err)
	}
	if len(slices.Items) != 0 {
		t.Errorf("slice still exists after delete")
	}
}
