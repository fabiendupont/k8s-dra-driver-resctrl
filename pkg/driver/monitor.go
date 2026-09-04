package driver

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/fabiendupont/k8s-dra-driver-resctrl/pkg/resctrl"
)

// MonitorTarget describes one resctrl group to be scraped.
type MonitorTarget struct {
	PartitionName string // metric label, e.g. "cache0-part0"
	Group         string // resctrl group directory name
	CacheGroupID  int
}

type mbmSample struct {
	local     uint64
	total     uint64
	timestamp time.Time
}

// Monitor periodically scrapes resctrl mon_data and updates Prometheus metrics.
type Monitor struct {
	mu          sync.Mutex
	resctrlRoot string
	interval    time.Duration
	targets     func() []MonitorTarget
	prevMBM     map[string]*mbmSample // key: "<group>/<cacheGroupID>"
}

// NewMonitor creates a Monitor. targets is called on every scrape to get the
// current set of groups to monitor (handles dynamic structured-mode allocs).
func NewMonitor(resctrlRoot string, interval time.Duration, targets func() []MonitorTarget) *Monitor {
	return &Monitor{
		resctrlRoot: resctrlRoot,
		interval:    interval,
		targets:     targets,
		prevMBM:     make(map[string]*mbmSample),
	}
}

// Run starts the scrape loop. It blocks until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	klog.InfoS("Monitoring started", "interval", m.interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.scrape()
		}
	}
}

func (m *Monitor) scrape() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for _, t := range m.targets() {
		data, err := resctrl.ReadMonData(t.Group, t.CacheGroupID)
		if err != nil {
			klog.V(5).InfoS("ReadMonData failed", "group", t.Group, "cacheGroupID", t.CacheGroupID, "error", err)
			continue
		}

		labels := []string{t.PartitionName, fmt.Sprintf("%d", t.CacheGroupID)}
		CacheOccupancyBytes.WithLabelValues(labels...).Set(float64(data.LLCOccupancy))

		key := fmt.Sprintf("%s/%d", t.Group, t.CacheGroupID)
		prev, hasPrev := m.prevMBM[key]
		m.prevMBM[key] = &mbmSample{local: data.MBMLocalBytes, total: data.MBMTotalBytes, timestamp: now}

		if !hasPrev {
			continue
		}
		elapsed := now.Sub(prev.timestamp).Seconds()
		if elapsed <= 0 {
			continue
		}
		// Guard against counter wrap.
		if data.MBMLocalBytes >= prev.local {
			rate := float64(data.MBMLocalBytes-prev.local) / elapsed
			MemBandwidthLocalBytesPerSec.WithLabelValues(labels...).Set(rate)
		}
		if data.MBMTotalBytes >= prev.total {
			rate := float64(data.MBMTotalBytes-prev.total) / elapsed
			MemBandwidthTotalBytesPerSec.WithLabelValues(labels...).Set(rate)
		}
	}
}
