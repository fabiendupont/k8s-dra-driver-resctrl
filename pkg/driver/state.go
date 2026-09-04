package driver

import (
	"fmt"
	"sync"

	"github.com/fabiendupont/k8s-dra-driver-resctrl/pkg/cache"
)

// PartitionState tracks whether a cache partition is available or allocated.
type PartitionState struct {
	Partition cache.CachePartition
	Allocated bool
	ClaimUID  string
}

// AllocationState manages the availability and allocation of cache partitions.
type AllocationState struct {
	mu         sync.Mutex
	partitions map[string]*PartitionState // keyed by partition ID
}

// NewAllocationState creates state from the given partitions, with all partitions available.
func NewAllocationState(perCache [][]*cache.CachePartition) *AllocationState {
	partitions := make(map[string]*PartitionState)
	for _, parts := range perCache {
		for _, p := range parts {
			partitions[p.ID] = &PartitionState{Partition: *p}
		}
	}
	return &AllocationState{partitions: partitions}
}

// AvailablePartitions returns all unallocated partitions.
func (a *AllocationState) AvailablePartitions() []cache.CachePartition {
	a.mu.Lock()
	defer a.mu.Unlock()

	var available []cache.CachePartition
	for _, p := range a.partitions {
		if !p.Allocated {
			available = append(available, p.Partition)
		}
	}
	return available
}

// Allocate marks a partition as allocated for a given claim.
func (a *AllocationState) Allocate(partitionID, claimUID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	p, ok := a.partitions[partitionID]
	if !ok {
		return fmt.Errorf("partition %q not found", partitionID)
	}
	if p.Allocated {
		if p.ClaimUID == claimUID {
			return nil
		}
		return fmt.Errorf("partition %q already allocated to claim %q", partitionID, p.ClaimUID)
	}

	p.Allocated = true
	p.ClaimUID = claimUID
	return nil
}

// Release marks a partition as available again.
func (a *AllocationState) Release(partitionID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	p, ok := a.partitions[partitionID]
	if !ok {
		return fmt.Errorf("partition %q not found", partitionID)
	}

	p.Allocated = false
	p.ClaimUID = ""
	return nil
}

// ReleaseByClaimUID releases all partitions allocated to a given claim.
func (a *AllocationState) ReleaseByClaimUID(claimUID string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	var released []string
	for id, p := range a.partitions {
		if p.Allocated && p.ClaimUID == claimUID {
			p.Allocated = false
			p.ClaimUID = ""
			released = append(released, id)
		}
	}
	return released
}

// AllocatedCount returns the number of currently allocated partitions.
func (a *AllocationState) AllocatedCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	count := 0
	for _, p := range a.partitions {
		if p.Allocated {
			count++
		}
	}
	return count
}

// PartitionsByClaimUID returns all partitions allocated to a given claim.
func (a *AllocationState) PartitionsByClaimUID(claimUID string) []cache.CachePartition {
	a.mu.Lock()
	defer a.mu.Unlock()

	var result []cache.CachePartition
	for _, p := range a.partitions {
		if p.Allocated && p.ClaimUID == claimUID {
			result = append(result, p.Partition)
		}
	}
	return result
}
