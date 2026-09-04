package cache

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/fabiendupont/k8s-dra-driver-resctrl/pkg/resctrl"
)

// PartitionSpec describes one size tier: Count partitions each with Ways cache ways.
type PartitionSpec struct {
	Ways  int
	Count int
}

// ParsePartitionSizes parses "--partition-sizes=1:8,2:4,4:2" into PartitionSpec slices.
func ParsePartitionSizes(s string) ([]PartitionSpec, error) {
	var specs []PartitionSpec
	for _, token := range strings.Split(s, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		parts := strings.SplitN(token, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid partition spec %q: expected ways:count", token)
		}
		ways, err := strconv.Atoi(parts[0])
		if err != nil || ways <= 0 {
			return nil, fmt.Errorf("invalid ways %q in spec %q", parts[0], token)
		}
		count, err := strconv.Atoi(parts[1])
		if err != nil || count <= 0 {
			return nil, fmt.Errorf("invalid count %q in spec %q", parts[1], token)
		}
		specs = append(specs, PartitionSpec{Ways: ways, Count: count})
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("no partition specs found in %q", s)
	}
	return specs, nil
}

// SpecsFromCount derives PartitionSpecs from a single count, distributing
// remainder ways round-robin so no ways go to the default resctrl group.
// Returns nil for count <= 0, which PartitionCache rejects with an error.
func SpecsFromCount(count, totalWays int) []PartitionSpec {
	if count <= 0 {
		return nil
	}
	base := totalWays / count
	remainder := totalWays % count
	var specs []PartitionSpec
	if remainder > 0 {
		specs = append(specs, PartitionSpec{Ways: base + 1, Count: remainder})
	}
	specs = append(specs, PartitionSpec{Ways: base, Count: count - remainder})
	return specs
}

// PartitionCache creates cache partitions according to the given specs.
// Specs are applied in order; partition index is global across all specs.
// cacheToCPU may be nil (CPUList will be empty).
func PartitionCache(info *resctrl.CATInfo, specs []PartitionSpec, cacheToNUMA map[int]int, cacheToSize map[int]int64, cacheToCPU map[int]string) ([][]*CachePartition, error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("no partition specs provided")
	}

	totalWays := info.NumWays()

	// Validate specs.
	totalRequested := 0
	totalCount := 0
	for _, s := range specs {
		if s.Ways < info.MinCBMBits {
			return nil, fmt.Errorf("spec ways %d is below hardware minimum %d contiguous bits", s.Ways, info.MinCBMBits)
		}
		if s.Ways > totalWays {
			return nil, fmt.Errorf("spec ways %d exceeds total ways %d", s.Ways, totalWays)
		}
		totalRequested += s.Ways * s.Count
		totalCount += s.Count
	}
	if totalRequested > totalWays {
		return nil, fmt.Errorf("total requested ways %d exceeds available ways %d", totalRequested, totalWays)
	}
	if totalCount > info.NumCLOSIDs-1 {
		return nil, fmt.Errorf("total partition count %d exceeds available CLOSIDs %d (one reserved for default)", totalCount, info.NumCLOSIDs-1)
	}

	var allPartitions [][]*CachePartition

	for _, cacheID := range info.CacheIDs {
		var (
			partitions []*CachePartition
			startWay   int
			globalIdx  int
		)

		var totalSizeBytes int64
		if cacheToSize != nil {
			totalSizeBytes = cacheToSize[cacheID]
		}
		var waySizeBytes int64
		if totalSizeBytes > 0 && totalWays > 0 {
			waySizeBytes = totalSizeBytes / int64(totalWays)
		}

		numaNode := -1
		if cacheToNUMA != nil {
			if n, ok := cacheToNUMA[cacheID]; ok {
				numaNode = n
			}
		}

		cpuList := ""
		if cacheToCPU != nil {
			cpuList = cacheToCPU[cacheID]
		}

		for _, spec := range specs {
			for j := 0; j < spec.Count; j++ {
				cbm := buildCBM(startWay, spec.Ways)
				startWay += spec.Ways

				var sizeBytes int64
				if waySizeBytes > 0 {
					sizeBytes = int64(spec.Ways) * waySizeBytes
				}

				partitions = append(partitions, &CachePartition{
					ID:           fmt.Sprintf("cache%d-part%d", cacheID, globalIdx),
					CacheID:      cacheID,
					Index:        globalIdx,
					Ways:         spec.Ways,
					TotalWays:    totalWays,
					CBM:          fmt.Sprintf("%x", cbm),
					SizeBytes:    sizeBytes,
					WaySizeBytes: waySizeBytes,
					MinCBMBits:   info.MinCBMBits,
					Level:        "L3",
					NUMANode:     numaNode,
					CPUList:      cpuList,
					ResctrlGroup: fmt.Sprintf("dra-cache%d-part%d", cacheID, globalIdx),
				})
				globalIdx++
			}
		}
		allPartitions = append(allPartitions, partitions)
	}

	return allPartitions, nil
}

func FormatSchemata(cacheID int, cbm string) string {
	return fmt.Sprintf("L3:%d=%s", cacheID, cbm)
}

// ApplyMBA sets MBAThrottle and MBAMode on every partition in perCache.
func ApplyMBA(perCache [][]*CachePartition, throttle int, mode string) {
	for _, parts := range perCache {
		for _, p := range parts {
			p.MBAThrottle = throttle
			p.MBAMode = mode
		}
	}
}

// FormatCombinedSchemata returns the resctrl schemata for a partition,
// including L2, MBA, and SMBA lines when configured.
func FormatCombinedSchemata(p *CachePartition) string {
	lines := []string{FormatSchemata(p.CacheID, p.CBM)}
	if p.L2CBM != "" {
		lines = append(lines, fmt.Sprintf("L2:%d=%s", p.CacheID, p.L2CBM))
	}
	if p.MBAThrottle > 0 {
		lines = append(lines, resctrl.FormatMBASchemata(p.CacheID, p.MBAThrottle))
	}
	if p.SMBAThrottle > 0 {
		lines = append(lines, resctrl.FormatSMBASchemata(p.CacheID, p.SMBAThrottle))
	}
	return strings.Join(lines, "\n")
}

// FormatCombinedSchemataStr formats schemata directly from components (structured mode).
func FormatCombinedSchemataStr(cacheID int, cbm string, mbaThrottle int) string {
	lines := []string{FormatSchemata(cacheID, cbm)}
	if mbaThrottle > 0 {
		lines = append(lines, resctrl.FormatMBASchemata(cacheID, mbaThrottle))
	}
	return strings.Join(lines, "\n")
}

// MBADeviceID returns the DRA device ID for the MBA device of a partition.
func MBADeviceID(p *CachePartition) string {
	return fmt.Sprintf("mba%d-part%d", p.CacheID, p.Index)
}

// ApplySMBA sets SMBAThrottle and SMBAMode on every partition in perCache.
func ApplySMBA(perCache [][]*CachePartition, throttle int, mode string) {
	for _, parts := range perCache {
		for _, p := range parts {
			p.SMBAThrottle = throttle
			p.SMBAMode = mode
		}
	}
}

// SMBADeviceID returns the DRA device ID for the SMBA device of a partition.
func SMBADeviceID(p *CachePartition) string {
	return fmt.Sprintf("smba%d-part%d", p.CacheID, p.Index)
}

// ApplyL2 sets L2 fields on every partition. It assigns the full L2 CBM mask
// so that group schemata writes succeed on platforms requiring an L2 line.
func ApplyL2(perCache [][]*CachePartition, l2Info *resctrl.CATInfo) {
	totalWays := l2Info.NumWays()
	fullCBM := fmt.Sprintf("%x", l2Info.CBMMask)
	for _, parts := range perCache {
		for _, p := range parts {
			p.L2Ways = totalWays
			p.L2TotalWays = totalWays
			p.L2CBM = fullCBM
		}
	}
}

func AllPartitions(perCache [][]*CachePartition) []*CachePartition {
	var all []*CachePartition
	for _, parts := range perCache {
		all = append(all, parts...)
	}
	return all
}

func buildCBM(startWay, numWays int) uint64 {
	var cbm uint64
	for i := startWay; i < startWay+numWays; i++ {
		cbm |= 1 << uint(i)
	}
	return cbm
}
