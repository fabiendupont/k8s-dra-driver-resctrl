# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-09-08

Initial release of `k8s-dra-driver-resctrl` (`resctrl.fabiendupont.io`), exposing Linux `resctrl` resources as Kubernetes Dynamic Resource Allocation (DRA) devices.

### Added

- **Cache Allocation Technology (CAT)**
  - L3 cache domain partitioning supporting equal-sized partitions (`--partition-count`) and heterogeneous multi-size specs (`--partition-sizes=ways:count[,...]`).
  - Bitmask computation ensuring valid contiguous CBMs with remainder way round-robin distribution to avoid unassigned ways in default group.
  - NUMA node, CPU list (`shared_cpu_list`), and cache size lookups via sysfs.
- **Memory Bandwidth Allocation (MBA & SMBA)**
  - Memory Bandwidth Allocation support with `--mba-bandwidth` cap, auto-detecting percent vs MBps mode.
  - Slow Memory Bandwidth Allocation (`--smba-bandwidth`) for Intel Xeon platforms with High Bandwidth Memory (HBM).
  - Unified schemata generation combining L3 CAT, MBA, and SMBA per partition.
- **L2 CAT Pass-Through**
  - Detection and automatic pass-through of full L2 mask when L2 CAT is active on the platform.
- **Cache & Memory Bandwidth Monitoring (CMT / MBM)**
  - Background scraper goroutine reading `mon_data/` counters at `--monitor-interval`.
  - Prometheus metrics: `cache_occupancy_bytes`, `memory_bandwidth_local_bytes_per_second`, and `memory_bandwidth_total_bytes_per_second`.
- **DRA Structured Parameters**
  - Startup probe (`ProbeFeatureGates`) for `DRAPartitionableDevices` and `DRAConsumableCapacity` using dry-run ResourceSlice creations.
  - Structured mode using `SharedCounters` and per-way-count devices with `AllowMultipleAllocations: true`.
  - On-demand first-fit consecutive CBM bit allocation via `WayAllocator` in `NodePrepareResources`.
  - Automatic fallback to pre-partitioned inventory mode when structured feature gates are disabled.
- **Topology Coordination & Node Discovery**
  - Shared `cacheGroupID` attribute for cross-driver alignment via Node Partition Topology Coordinator.
  - Node Feature Discovery (NFD) rule targeting nodes with resctrl L3 CAT capability.
  - Named DeviceClass configurations for common sizing tiers (quarter, third, half, 4MiB, 8MiB).
- **CDI Hook & OCI Integration**
  - CDI hook mode (`dra-resctrl --cdi-hook`) invoked by container runtimes (CRI-O / containerd) to assign container PIDs into resctrl `tasks` file.
  - Per-claim and per-partition CDI specification generation (`/var/run/cdi`).
- **Production Hardening & Operations**
  - Health and readiness endpoints (`/healthz`, `/readyz`) on configurable port (`--health-port`).
  - Task draining back to root group prior to resctrl group deletion.
  - Gated shutdown sequence to clean up ResourceSlices and resctrl groups on termination.
  - Operational runbook covering group inspection, CLOSID exhaustion, monitoring, and recovery.
- **Supply Chain Security & Release Pipeline**
  - Keyless container image signing with Cosign.
  - SPDX JSON Software Bill of Materials (SBOM) generation and attachment to releases.
  - Trivy vulnerability scanning integrated into CI pipeline.
  - Multi-architecture container images (`linux/amd64`, `linux/arm64`) with native Go cross-compilation.
