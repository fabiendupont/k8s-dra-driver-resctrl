# k8s-dra-driver-resctrl

## Overview

Kubernetes DRA (Dynamic Resource Allocation) driver that exposes Linux `resctrl`
resources as DRA devices: L3/L2 cache partitions (CAT), memory bandwidth (MBA/SMBA),
and cache/bandwidth monitoring (CMT/MBM). Works on Intel (RDT), AMD (QoS), ARM (MPAM).

## Driver Name

`resctrl.fabiendupont.io`

## Project Layout

- `cmd/driver/` — Driver entrypoint; also CDI hook mode via `--cdi-hook`
- `pkg/resctrl/` — resctrl filesystem operations: CAT, MBA, SMBA, monitoring, L2
- `pkg/cache/` — Partition model, multi-size specs, NUMA/CPU/size sysfs lookup
- `pkg/driver/` — DRA kubelet plugin: state, ResourceSlice publishing, CDI specs,
  feature gate detection, WayAllocator (structured mode), monitoring scraper
- `deploy/helm/dra-resctrl/` — Helm chart
- `deployments/` — Raw manifests (DaemonSet, DeviceClasses, RBAC, NFD rule, examples)
- `docs/runbook.md` — Operational runbook

## Key Design Decisions

### Partition model
- `PartitionSpec{Ways, Count}` — supports heterogeneous partition sizes via `--partition-sizes=ways:count[,...]`
- `SpecsFromCount(count, totalWays)` provides backward compat for `--partition-count`
- Remainder ways distributed round-robin so no ways go to the default resctrl group

### DRA device types
- `cache<N>-part<M>` — CAT cache partition (L3)
- `mba<N>-part<M>` — MBA memory bandwidth device (shares resctrl group with cache device)
- `smba<N>-part<M>` — SMBA slow-memory bandwidth device (HBM systems)
- All three devices share the same resctrl group; CDI hook writes PID to `tasks` file

### resctrl schemata format
```
L3:<domainID>=<cbmHex>
L2:<domainID>=<l2cbmHex>   (pass-through full mask when L2 present)
MB:<domainID>=<throttle>
SMBA:<domainID>=<throttle>
```

### Key attributes
- `cacheGroupID` (int) — Physical cache domain ID, shared with other DRA drivers for
  topology coordinator `matchAttribute` (matches `k8s-dra-driver-deterministic-time-share`)
- `cacheSizeLabel` (string) — Human-readable size e.g. `"5MiB"`, `"2.5MiB"`
- `cpuList` (string) — CPUs sharing the cache domain from `shared_cpu_list` sysfs

### DRA structured parameters (dual mode)
- `ProbeFeatureGates()` detects `DRAPartitionableDevices` and `DRAConsumableCapacity`
  via dry-run ResourceSlice creates at startup
- **Basic mode** (default): pre-partitioned inventory, `AllocationState` manages fixed devices
- **Structured mode** (`DRAPartitionableDevices` enabled): `SharedCounters` + per-way-count
  devices with `AllowMultipleAllocations:true`; `WayAllocator` allocates consecutive CBM
  bits on demand in `NodePrepareResources`; per-claim CDI spec files written/removed

### Monitoring (CMT/MBM)
- Background `Monitor` goroutine scrapes `mon_data/mon_L3_<id>/` every `--monitor-interval`
- Publishes `cache_occupancy_bytes`, `memory_bandwidth_local_bytes_per_second`,
  `memory_bandwidth_total_bytes_per_second` as Prometheus GaugeVecs

## Build

```bash
make build   # bin/dra-resctrl
make test    # go test -race ./...
make image   # container image
```

## Helm

```bash
helm install dra-resctrl deploy/helm/dra-resctrl/ -n dra-resctrl --create-namespace
```
