package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fabiendupont/k8s-dra-driver-resctrl/pkg/resctrl"
)

func makeMonDir(t *testing.T, root, group string, cacheID int, llc, local, total uint64) {
	t.Helper()
	dir := filepath.Join(root, group, "mon_data", fmt.Sprintf("mon_L3_%d", cacheID))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	writeMonFile := func(name string, val uint64) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("%d\n", val)), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeMonFile("llc_occupancy", llc)
	writeMonFile("mbm_local_bytes", local)
	writeMonFile("mbm_total_bytes", total)
}

// resetMonMetrics resets the monitoring gauge vecs between tests.
func resetMonMetrics() {
	CacheOccupancyBytes.Reset()
	MemBandwidthLocalBytesPerSec.Reset()
	MemBandwidthTotalBytesPerSec.Reset()
}

func TestMonitor_Scrape_Occupancy(t *testing.T) {
	root := t.TempDir()
	resctrl.SetRoot(root)
	defer resctrl.SetRoot(resctrl.DefaultRoot)

	makeMonDir(t, root, "dra-cache0-part0", 0, 5*1024*1024, 1000, 2000)

	resetMonMetrics()

	targets := func() []MonitorTarget {
		return []MonitorTarget{{PartitionName: "cache0-part0", Group: "dra-cache0-part0", CacheGroupID: 0}}
	}
	mon := NewMonitor(root, time.Second, targets)
	mon.scrape()

	// Verify occupancy gauge was set.
	got := testutil.ToFloat64(CacheOccupancyBytes.With(prometheus.Labels{
		"partition":     "cache0-part0",
		"cache_group_id": "0",
	}))
	want := float64(5 * 1024 * 1024)
	if got != want {
		t.Errorf("CacheOccupancyBytes = %f, want %f", got, want)
	}

	// First scrape: no bandwidth rate yet (no previous sample).
	bw := testutil.ToFloat64(MemBandwidthLocalBytesPerSec.With(prometheus.Labels{
		"partition":     "cache0-part0",
		"cache_group_id": "0",
	}))
	if bw != 0 {
		t.Errorf("bandwidth rate on first scrape = %f, want 0 (no previous)", bw)
	}
}

func TestMonitor_Scrape_BandwidthRate(t *testing.T) {
	root := t.TempDir()
	resctrl.SetRoot(root)
	defer resctrl.SetRoot(resctrl.DefaultRoot)

	makeMonDir(t, root, "dra-cache0-part0", 0, 0, 1_000_000, 2_000_000)
	resetMonMetrics()

	targets := func() []MonitorTarget {
		return []MonitorTarget{{PartitionName: "cache0-part0", Group: "dra-cache0-part0", CacheGroupID: 0}}
	}
	mon := NewMonitor(root, time.Second, targets)

	// First scrape — records previous sample.
	mon.scrape()

	// Update mon_data with higher values to simulate elapsed bandwidth.
	makeMonDir(t, root, "dra-cache0-part0", 0, 0, 11_000_000, 22_000_000)

	// Manually inject a previous sample with a timestamp 1 second in the past
	// so rate computation is deterministic.
	key := "dra-cache0-part0/0"
	mon.mu.Lock()
	prev := mon.prevMBM[key]
	if prev != nil {
		prev.timestamp = prev.timestamp.Add(-time.Second)
	}
	mon.mu.Unlock()

	mon.scrape()

	labels := prometheus.Labels{"partition": "cache0-part0", "cache_group_id": "0"}
	localRate := testutil.ToFloat64(MemBandwidthLocalBytesPerSec.With(labels))
	totalRate := testutil.ToFloat64(MemBandwidthTotalBytesPerSec.With(labels))

	if localRate <= 0 {
		t.Errorf("MemBandwidthLocalBytesPerSec = %f, want > 0", localRate)
	}
	if totalRate <= 0 {
		t.Errorf("MemBandwidthTotalBytesPerSec = %f, want > 0", totalRate)
	}
}

func TestMonitor_Scrape_CounterWrap(t *testing.T) {
	root := t.TempDir()
	resctrl.SetRoot(root)
	defer resctrl.SetRoot(resctrl.DefaultRoot)

	makeMonDir(t, root, "dra-cache0-part0", 0, 0, 1_000_000, 2_000_000)
	resetMonMetrics()

	targets := func() []MonitorTarget {
		return []MonitorTarget{{PartitionName: "cache0-part0", Group: "dra-cache0-part0", CacheGroupID: 0}}
	}
	mon := NewMonitor(root, time.Second, targets)
	mon.scrape()

	// Simulate counter wrap: new value < old value.
	makeMonDir(t, root, "dra-cache0-part0", 0, 0, 500_000, 1_000_000)
	key := "dra-cache0-part0/0"
	mon.mu.Lock()
	if prev := mon.prevMBM[key]; prev != nil {
		prev.timestamp = prev.timestamp.Add(-time.Second)
	}
	mon.mu.Unlock()

	// Before scrape: record current rate (should be 0 from first scrape).
	before := testutil.ToFloat64(MemBandwidthLocalBytesPerSec.With(prometheus.Labels{
		"partition": "cache0-part0", "cache_group_id": "0",
	}))

	mon.scrape()

	// Rate should not have been updated (counter wrapped — skip).
	after := testutil.ToFloat64(MemBandwidthLocalBytesPerSec.With(prometheus.Labels{
		"partition": "cache0-part0", "cache_group_id": "0",
	}))
	if after != before {
		t.Errorf("counter wrap: rate changed from %f to %f, should be unchanged", before, after)
	}
}
