# k8s-dra-driver-resctrl

A Kubernetes [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) driver that exposes Linux `resctrl` resources — L3/L2 cache partitions, memory bandwidth allocation, and monitoring — as DRA devices.

Supported hardware: Intel (RDT/CAT/MBA), AMD (QoS Extensions), ARM (MPAM).

## Features

| resctrl capability | DRA device type | Flag |
|---|---|---|
| L3 Cache Allocation (CAT) | `cache<N>-part<M>` | `--partition-count` / `--partition-sizes` |
| L2 CAT (pass-through) | L2 attributes on CAT device | auto-detected |
| Memory Bandwidth Allocation (MBA) | `mba<N>-part<M>` | `--mba-bandwidth` |
| Slow Memory Bandwidth (SMBA, HBM) | `smba<N>-part<M>` | `--smba-bandwidth` |
| Cache Monitoring (CMT/MBM) | Prometheus metrics | `--monitor-interval` |

> **L2 is pass-through, not partitioned.** When resctrl exposes L2 CAT, each
> partition group is given the full L2 mask (so the required `L2:` schemata line
> is accepted by the kernel) and L2 geometry is surfaced as attributes on the CAT
> device. L2 is typically a small per-core cache, so slicing it yields little
> pod-to-pod isolation while consuming the same shared CLOSID budget as L3 —
> partitioning targets the shared L3 instead.

## How It Works

```
L3 Cache domain 0 — 20 ways, 4 partitions:

|---part0---|---part1---|---part2---|---part3---|
  ways 0-4    ways 5-9   ways 10-14  ways 15-19
  CBM=0x1f   CBM=0x3e0  CBM=0x7c00  CBM=0xf8000
```

1. **Discover** — Read `/sys/fs/resctrl/info/` for CAT, MBA, SMBA, and monitoring capabilities.
2. **Partition** — Divide cache ways into slices (equal or heterogeneous via `--partition-sizes`). Create a resctrl group per partition with the appropriate schemata (`L3:`, `L2:`, `MB:`, `SMBA:`).
3. **Advertise** — Publish partitions as DRA devices in a `ResourceSlice`, each with attributes for size, topology, and bandwidth.
4. **Allocate** — The scheduler selects a device for each `ResourceClaim`. On pod start, kubelet calls `NodePrepareResources`; the driver records the allocation and returns CDI device IDs.
5. **Enforce** — CRI-O executes the `createRuntime` CDI hook, which writes the container PID to `/sys/fs/resctrl/<group>/tasks`.
6. **Monitor** — A background goroutine scrapes `mon_data/` per group and exposes LLC occupancy and bandwidth metrics on `/metrics`.
7. **Release** — `NodeUnprepareResources` frees the allocation for reuse.

## Prerequisites

- Kubernetes 1.32+ (DRA v1 GA; `resource.k8s.io/v1` API)
- CPU with L3 CAT support (see [Hardware](#hardware-compatibility))
- resctrl filesystem mounted: `mount -t resctrl resctrl /sys/fs/resctrl`
- Go 1.26+ to build from source

### Hardware Compatibility

| Platform | Technology | resctrl |
|---|---|---|
| Intel Xeon (v4+) | RDT / CAT / MBA | Yes |
| Intel Xeon (Sapphire Rapids HBM) | + SMBA | Yes |
| AMD EPYC | QoS Extensions / L3 CAT | Yes |
| ARM Neoverse+ | MPAM | Yes (kernel 6.x+) |

## Project Layout

```
cmd/driver/      Driver entrypoint (also CDI hook via --cdi-hook)
pkg/resctrl/     resctrl filesystem operations (CAT, MBA, SMBA, monitoring)
pkg/cache/       Partition model, sizing, NUMA/CPU discovery
pkg/driver/      DRA plugin, ResourceSlice publishing, CDI, monitoring, feature probing
deploy/helm/     Helm chart (deploy/helm/dra-resctrl/)
deployments/     Raw manifests (DaemonSet, DeviceClasses, RBAC, NFD rule, examples)
docs/            Operational runbook
```

## Build

```bash
make build      # bin/dra-resctrl
make test       # unit tests with -race
make image      # container image with podman
```

## Deploy

### Helm

```bash
helm install dra-resctrl deploy/helm/dra-resctrl/ \
  -n dra-resctrl --create-namespace \
  --set driver.partitionCount=4
```

### Raw manifests

```bash
kubectl apply -f deployments/rbac.yaml
kubectl apply -f deployments/device-class.yaml
kubectl apply -f deployments/device-classes-sized.yaml   # named size tiers
kubectl apply -f deployments/topology-rule.yaml          # topology coordinator
kubectl apply -f deployments/node-feature-rule.yaml      # optional, requires NFD
kubectl apply -f deployments/daemonset.yaml
```

## Configuration

| Flag | Default | Description |
|---|---|---|
| `--partition-count` | `4` | Equal partitions per cache domain |
| `--partition-sizes` | `` | Heterogeneous sizes: `ways:count[,...]` e.g. `2:4,4:2` |
| `--mba-bandwidth` | `-1` | MBA throttle (auto: 100% in percent mode, skip in MBps) |
| `--smba-bandwidth` | `-1` | SMBA throttle for slow memory (HBM systems) |
| `--monitor-interval` | `10s` | CMT/MBM scrape interval for Prometheus metrics |
| `--resctrl-root` | `/sys/fs/resctrl` | resctrl filesystem mount path |
| `--socket` | `.../resctrl.fabiendupont.io/plugin.sock` | DRA plugin socket |
| `--health-port` | `8081` | `/healthz`, `/readyz`, `/metrics` port |
| `--cdi-dir` | `/var/run/cdi` | CDI spec directory |

## Usage

### Quick start — any cache partition

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: my-cache
spec:
  devices:
    requests:
      - name: cache
        exactly:
          deviceClassName: cache-partition-any
```

### Named size tiers (install `device-classes-sized.yaml` first)

```yaml
deviceClassName: cache-partition-quarter   # ≥ 25% of L3 ways (portable)
deviceClassName: cache-partition-half      # ≥ 50% of L3 ways
deviceClassName: cache-partition-4mib      # ≥ 4 MiB absolute
```

### Size by bytes (CEL selector)

```yaml
selectors:
  - cel:
      expression: >
        device.attributes['resctrl.fabiendupont.io'].cacheSizeBytes >= 5242880
```

### Reference from a Pod

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: latency-sensitive-app
spec:
  containers:
    - name: app
      image: my-app:latest
      resources:
        claims:
          - name: cache
  resourceClaims:
    - name: cache
      resourceClaimName: my-cache
```

## Device Attributes

All attributes are in the `resctrl.fabiendupont.io` domain.

### Cache partition device (`cache<N>-part<M>`)

| Attribute | Type | Description |
|---|---|---|
| `cacheGroupID` | int | Physical L3 cache domain ID — matches across DRA drivers for topology coordination |
| `cacheLevel` | string | `"L3"` |
| `cacheWays` | int | Ways assigned to this partition |
| `cacheTotalWays` | int | Total ways in the domain |
| `cacheSizeBytes` | int | Partition size in bytes |
| `cacheWaySizeBytes` | int | Size of one way in bytes (allocation granularity) |
| `cacheSizeLabel` | string | Human-readable size (e.g. `"5MiB"`, `"2.5MiB"`) |
| `minPartitionSizeBytes` | int | Hardware minimum partition size |
| `cbmHex` | string | Cache Bit Mask in hex |
| `numaNode` | int | NUMA node (-1 if unknown) |
| `cpuList` | string | CPUs sharing this cache domain (e.g. `"0-11"`) |
| `resctrlGroup` | string | resctrl group directory name (debug) |
| `l2Ways` | int | L2 ways (when L2 CAT present) |
| `l2TotalWays` | int | Total L2 ways |
| `l2CbmHex` | string | L2 Cache Bit Mask |

### MBA device (`mba<N>-part<M>`)

| Attribute | Type | Description |
|---|---|---|
| `mbaDomainID` | int | Memory bandwidth domain |
| `mbaBandwidth` | int | Throttle value |
| `mbaMode` | string | `"percent"` or `"mbps"` |
| `numaNode` | int | NUMA node |

### SMBA device (`smba<N>-part<M>`)

Same attributes as MBA (`smbaDomainID`, `smbaBandwidth`, `smbaMode`, `numaNode`).

## Metrics

Exposed on `:8081/metrics`.

| Metric | Type | Description |
|---|---|---|
| `dra_cache_partition_partitions_total` | gauge | Advertised partitions |
| `dra_cache_partition_partitions_allocated` | gauge | Allocated partitions |
| `dra_cache_partition_prepare_total{result}` | counter | NodePrepareResources calls |
| `dra_cache_partition_unprepare_total{result}` | counter | NodeUnprepareResources calls |
| `dra_cache_partition_prepare_duration_seconds` | histogram | Prepare latency |
| `dra_cache_partition_unprepare_duration_seconds` | histogram | Unprepare latency |
| `dra_cache_partition_resctrl_group_create_total{result}` | counter | Group creation attempts |
| `dra_cache_partition_resctrl_group_delete_total{result}` | counter | Group deletion attempts |
| `dra_cache_partition_cache_occupancy_bytes{partition,cache_group_id}` | gauge | LLC bytes in use (CMT) |
| `dra_cache_partition_memory_bandwidth_local_bytes_per_second{...}` | gauge | Local DRAM bandwidth (MBM) |
| `dra_cache_partition_memory_bandwidth_total_bytes_per_second{...}` | gauge | Total bandwidth (MBM) |

## DRA Structured Parameters

On clusters where `DRAPartitionableDevices` is enabled, the driver auto-detects this at startup and switches to structured mode:

- Publishes `SharedCounters` (total ways per domain) and one device per way-count (`cache0-1way` … `cache0-20way`) with `AllowMultipleAllocations: true`
- The scheduler tracks way consumption; no pre-partitioning needed
- resctrl groups are created on demand in `NodePrepareResources` using first-fit consecutive-bit allocation
- Falls back to inventory (pre-partitioned) mode automatically when the feature gate is absent

## Topology Coordinator Integration

Apply `deployments/topology-rule.yaml` to register the driver with the [Node Partition Topology Coordinator](https://github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator):

```bash
kubectl apply -f deployments/topology-rule.yaml
```

The `cacheGroupID` integer attribute is shared with other resctrl-aware DRA drivers (e.g. `k8s-dra-driver-deterministic-time-share`), allowing the coordinator to co-schedule resources on the same physical cache domain via `matchAttribute`.

## Node Feature Discovery

```bash
kubectl apply -f deployments/node-feature-rule.yaml
```

Adds label `resctrl.fabiendupont.io/resctrl-cat: "true"` to nodes with L3 CAT support. The DaemonSet uses this as a `nodeSelector`.

## Troubleshooting

See [docs/runbook.md](docs/runbook.md) for detailed operational guidance including resctrl group inspection, CLOSID exhaustion, CMT/MBM verification, and recovery procedures.

### Quick checks

```bash
# Is resctrl mounted?
mount | grep resctrl

# Are groups present?
ls /sys/fs/resctrl/ | grep dra-

# Driver logs
kubectl -n dra-resctrl logs -l app=dra-resctrl

# ResourceSlice published?
kubectl get resourceslices -o wide

# Claim allocated?
kubectl get resourceclaim <name> -o yaml | grep -A5 status
```

## License

See [LICENSE](LICENSE) for details.
