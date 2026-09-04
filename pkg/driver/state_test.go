package driver

import (
	"testing"

	"github.com/fabiendupont/k8s-dra-driver-resctrl/pkg/cache"
)

func makeState(ids ...string) *AllocationState {
	var perCache [][]*cache.CachePartition
	for _, id := range ids {
		perCache = append(perCache, []*cache.CachePartition{{ID: id, ResctrlGroup: "dra-" + id}})
	}
	return NewAllocationState(perCache)
}

func TestAllocate(t *testing.T) {
	s := makeState("cache0-part0", "cache0-part1")

	if err := s.Allocate("cache0-part0", "claim-1"); err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if s.AllocatedCount() != 1 {
		t.Fatalf("AllocatedCount = %d, want 1", s.AllocatedCount())
	}

	// Idempotent: same claim re-allocating same partition is fine.
	if err := s.Allocate("cache0-part0", "claim-1"); err != nil {
		t.Fatalf("idempotent Allocate: %v", err)
	}

	// Different claim on already-allocated partition should fail.
	if err := s.Allocate("cache0-part0", "claim-2"); err == nil {
		t.Fatal("expected error allocating to second claim, got nil")
	}

	// Unknown partition.
	if err := s.Allocate("no-such-partition", "claim-1"); err == nil {
		t.Fatal("expected error for unknown partition, got nil")
	}
}

func TestRelease(t *testing.T) {
	s := makeState("cache0-part0")
	_ = s.Allocate("cache0-part0", "claim-1")

	if err := s.Release("cache0-part0"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if s.AllocatedCount() != 0 {
		t.Fatalf("AllocatedCount = %d after release, want 0", s.AllocatedCount())
	}

	if err := s.Release("no-such"); err == nil {
		t.Fatal("expected error releasing unknown partition")
	}
}

func TestReleaseByClaimUID(t *testing.T) {
	s := makeState("cache0-part0", "cache0-part1", "cache0-part2")
	_ = s.Allocate("cache0-part0", "claim-A")
	_ = s.Allocate("cache0-part1", "claim-A")
	_ = s.Allocate("cache0-part2", "claim-B")

	released := s.ReleaseByClaimUID("claim-A")
	if len(released) != 2 {
		t.Fatalf("ReleaseByClaimUID returned %d partitions, want 2", len(released))
	}
	if s.AllocatedCount() != 1 {
		t.Fatalf("AllocatedCount = %d after releasing claim-A, want 1", s.AllocatedCount())
	}
}

func TestPartitionsByClaimUID(t *testing.T) {
	s := makeState("cache0-part0", "cache0-part1")
	_ = s.Allocate("cache0-part0", "claim-1")
	_ = s.Allocate("cache0-part1", "claim-2")

	parts := s.PartitionsByClaimUID("claim-1")
	if len(parts) != 1 || parts[0].ID != "cache0-part0" {
		t.Fatalf("PartitionsByClaimUID returned %v, want [{cache0-part0}]", parts)
	}
}

func TestAvailablePartitions(t *testing.T) {
	s := makeState("cache0-part0", "cache0-part1")
	_ = s.Allocate("cache0-part0", "claim-1")

	avail := s.AvailablePartitions()
	if len(avail) != 1 || avail[0].ID != "cache0-part1" {
		t.Fatalf("AvailablePartitions = %v, want [{cache0-part1}]", avail)
	}
}
