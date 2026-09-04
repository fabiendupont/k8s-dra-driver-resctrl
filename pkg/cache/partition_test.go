package cache

import (
	"strconv"
	"testing"

	"github.com/fabiendupont/k8s-dra-driver-resctrl/pkg/resctrl"
)

func testCATInfo(numWays, numCLOSIDs int, cacheIDs []int) *resctrl.CATInfo {
	var cbm uint64
	for i := 0; i < numWays; i++ {
		cbm |= 1 << uint(i)
	}
	return &resctrl.CATInfo{
		NumCLOSIDs: numCLOSIDs,
		CBMMask:    cbm,
		MinCBMBits: 1,
		CacheIDs:   cacheIDs,
	}
}

func TestPartitionCache(t *testing.T) {
	tests := []struct {
		name      string
		numWays   int
		closids   int
		cacheIDs  []int
		count     int
		wantParts int
		wantWays  int
		wantErr   bool
	}{
		{
			name:      "20 ways into 4 partitions",
			numWays:   20,
			closids:   16,
			cacheIDs:  []int{0},
			count:     4,
			wantParts: 4,
			wantWays:  5,
		},
		{
			name:      "12 ways into 3 partitions, 2 caches",
			numWays:   12,
			closids:   16,
			cacheIDs:  []int{0, 1},
			count:     3,
			wantParts: 3,
			wantWays:  4,
		},
		{
			name:      "single partition",
			numWays:   20,
			closids:   16,
			cacheIDs:  []int{0},
			count:     1,
			wantParts: 1,
			wantWays:  20,
		},
		{
			name:     "too many partitions for CLOSIDs",
			numWays:  20,
			closids:  5,
			cacheIDs: []int{0},
			count:    5,
			wantErr:  true,
		},
		{
			name:     "zero partitions",
			numWays:  20,
			closids:  16,
			cacheIDs: []int{0},
			count:    0,
			wantErr:  true,
		},
		{
			name:     "more partitions than ways",
			numWays:  4,
			closids:  16,
			cacheIDs: []int{0},
			count:    8,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := testCATInfo(tt.numWays, tt.closids, tt.cacheIDs)
			perCache, err := PartitionCache(info, SpecsFromCount(tt.count, info.NumWays()), nil, nil, nil)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(perCache) != len(tt.cacheIDs) {
				t.Fatalf("got %d cache domains, want %d", len(perCache), len(tt.cacheIDs))
			}

			for _, parts := range perCache {
				if len(parts) != tt.wantParts {
					t.Fatalf("got %d partitions, want %d", len(parts), tt.wantParts)
				}
				for _, p := range parts {
					if p.Ways != tt.wantWays {
						t.Errorf("partition %s: ways = %d, want %d", p.ID, p.Ways, tt.wantWays)
					}
					if p.TotalWays != tt.numWays {
						t.Errorf("partition %s: totalWays = %d, want %d", p.ID, p.TotalWays, tt.numWays)
					}
				}
			}
		})
	}
}

func TestPartitionCacheNonOverlapping(t *testing.T) {
	info := testCATInfo(20, 16, []int{0})
	perCache, err := PartitionCache(info, SpecsFromCount(4, info.NumWays()), nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	parts := perCache[0]
	for i := 0; i < len(parts); i++ {
		cbmI, _ := strconv.ParseUint(parts[i].CBM, 16, 64)
		for j := i + 1; j < len(parts); j++ {
			cbmJ, _ := strconv.ParseUint(parts[j].CBM, 16, 64)
			if cbmI&cbmJ != 0 {
				t.Errorf("partitions %s (cbm=%s) and %s (cbm=%s) overlap",
					parts[i].ID, parts[i].CBM, parts[j].ID, parts[j].CBM)
			}
		}
	}

	var combined uint64
	for _, p := range parts {
		cbm, _ := strconv.ParseUint(p.CBM, 16, 64)
		combined |= cbm
	}
	if combined != info.CBMMask {
		t.Errorf("combined CBM %x does not cover full mask %x", combined, info.CBMMask)
	}
}

func TestFormatSchemata(t *testing.T) {
	tests := []struct {
		cacheID int
		cbm     string
		want    string
	}{
		{0, "1f", "L3:0=1f"},
		{1, "3e0", "L3:1=3e0"},
		{0, "fffff", "L3:0=fffff"},
	}

	for _, tt := range tests {
		got := FormatSchemata(tt.cacheID, tt.cbm)
		if got != tt.want {
			t.Errorf("FormatSchemata(%d, %q) = %q, want %q", tt.cacheID, tt.cbm, got, tt.want)
		}
	}
}

func TestPartitionCacheRemainderDistribution(t *testing.T) {
	// 20 ways / 3 partitions = 6 base + 2 remainder → partitions get 7, 7, 6 ways.
	info := testCATInfo(20, 16, []int{0})
	perCache, err := PartitionCache(info, SpecsFromCount(3, info.NumWays()), nil, nil, nil)
	if err != nil {
		t.Fatalf("PartitionCache: %v", err)
	}

	parts := perCache[0]
	if len(parts) != 3 {
		t.Fatalf("got %d partitions, want 3", len(parts))
	}
	wantWays := []int{7, 7, 6}
	for i, p := range parts {
		if p.Ways != wantWays[i] {
			t.Errorf("partition %d: ways = %d, want %d", i, p.Ways, wantWays[i])
		}
	}

	// Verify no ways are wasted: CBM union must cover full mask.
	var combined uint64
	for _, p := range parts {
		cbm, _ := strconv.ParseUint(p.CBM, 16, 64)
		combined |= cbm
	}
	if combined != info.CBMMask {
		t.Errorf("combined CBM %x does not cover full mask %x (ways wasted)", combined, info.CBMMask)
	}
}

func TestPartitionCacheExactDivision(t *testing.T) {
	// 20 ways / 4 partitions = 5 each, no remainder.
	info := testCATInfo(20, 16, []int{0})
	perCache, err := PartitionCache(info, SpecsFromCount(4, info.NumWays()), nil, nil, nil)
	if err != nil {
		t.Fatalf("PartitionCache: %v", err)
	}
	for _, p := range perCache[0] {
		if p.Ways != 5 {
			t.Errorf("partition %s: ways = %d, want 5", p.ID, p.Ways)
		}
	}
}

func TestApplyMBA(t *testing.T) {
	info := testCATInfo(20, 16, []int{0, 1})
	perCache, err := PartitionCache(info, SpecsFromCount(4, info.NumWays()), nil, nil, nil)
	if err != nil {
		t.Fatalf("PartitionCache: %v", err)
	}

	ApplyMBA(perCache, 70, "percent")

	for _, parts := range perCache {
		for _, p := range parts {
			if p.MBAThrottle != 70 {
				t.Errorf("partition %s MBAThrottle = %d, want 70", p.ID, p.MBAThrottle)
			}
			if p.MBAMode != "percent" {
				t.Errorf("partition %s MBAMode = %q, want percent", p.ID, p.MBAMode)
			}
		}
	}
}

func TestApplySMBA(t *testing.T) {
	info := testCATInfo(20, 16, []int{0})
	perCache, err := PartitionCache(info, SpecsFromCount(4, info.NumWays()), nil, nil, nil)
	if err != nil {
		t.Fatalf("PartitionCache: %v", err)
	}
	ApplySMBA(perCache, 50, "percent")
	for _, parts := range perCache {
		for _, p := range parts {
			if p.SMBAThrottle != 50 {
				t.Errorf("partition %s SMBAThrottle = %d, want 50", p.ID, p.SMBAThrottle)
			}
			if p.SMBAMode != "percent" {
				t.Errorf("partition %s SMBAMode = %q, want percent", p.ID, p.SMBAMode)
			}
		}
	}
}

func TestSMBADeviceID(t *testing.T) {
	p := &CachePartition{CacheID: 0, Index: 2}
	if got := SMBADeviceID(p); got != "smba0-part2" {
		t.Errorf("SMBADeviceID = %q, want smba0-part2", got)
	}
}

func TestFormatCombinedSchemata(t *testing.T) {
	p := &CachePartition{CacheID: 0, CBM: "1f"}
	got := FormatCombinedSchemata(p)
	if got != "L3:0=1f" {
		t.Errorf("L3 only: got %q, want %q", got, "L3:0=1f")
	}

	p.MBAThrottle = 50
	p.MBAMode = "percent"
	got = FormatCombinedSchemata(p)
	want := "L3:0=1f\nMB:0=50"
	if got != want {
		t.Errorf("L3+MBA: got %q, want %q", got, want)
	}

	p.SMBAThrottle = 30
	p.SMBAMode = "percent"
	got = FormatCombinedSchemata(p)
	want = "L3:0=1f\nMB:0=50\nSMBA:0=30"
	if got != want {
		t.Errorf("L3+MBA+SMBA: got %q, want %q", got, want)
	}

	p2 := &CachePartition{CacheID: 1, CBM: "3e0", L2CBM: "ff"}
	got = FormatCombinedSchemata(p2)
	want = "L3:1=3e0\nL2:1=ff"
	if got != want {
		t.Errorf("L3+L2: got %q, want %q", got, want)
	}
}

func TestMBADeviceID(t *testing.T) {
	p := &CachePartition{CacheID: 1, Index: 3}
	got := MBADeviceID(p)
	want := "mba1-part3"
	if got != want {
		t.Errorf("MBADeviceID = %q, want %q", got, want)
	}
}

func TestUtilization(t *testing.T) {
	p := &CachePartition{Ways: 5, TotalWays: 20}
	u := p.Utilization()
	if u != 0.25 {
		t.Errorf("Utilization() = %f, want 0.25", u)
	}
}
