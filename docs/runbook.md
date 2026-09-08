# DRA Cache Partition Driver — Operational Runbook

## Hardware Requirements

- **CPU**: Intel (RDT/CAT), AMD (QoS), or ARM (MPAM) with L3 CAT support
- **BIOS**: Intel RDT must be enabled in BIOS (look for "Intel Resource Director Technology" or "Cache Allocation Technology")
- **Kernel**: Linux 5.10+ with `CONFIG_X86_CPU_RESCTRL=y` (x86) or equivalent; check with `zcat /proc/config.gz | grep RESCTRL`
- **Verify hardware support**: `ls /sys/fs/resctrl/info/L3/` — must exist and contain `cbm_mask`, `num_closids`, `min_cbm_bits`

---

## 1. Verifying resctrl Groups Are Present

After the driver starts, it creates one resctrl group per partition:

```bash
ls /sys/fs/resctrl/ | grep dra-
# Expected output example (4 partitions on 1 cache domain):
# dra-cache0-part0
# dra-cache0-part1
# dra-cache0-part2
# dra-cache0-part3
```

Check the schemata (cache bit mask) for a partition:

```bash
cat /sys/fs/resctrl/dra-cache0-part0/schemata
# Expected: L3:0=1f  (5 ways, cache domain 0)
```

Check which PIDs are in a partition's resctrl group:

```bash
cat /sys/fs/resctrl/dra-cache0-part0/tasks
```

---

## 2. Recovering from a Driver Crash with Orphaned Groups

If the driver crashes or is killed before cleanup, resctrl groups remain on the node. They are harmless but consume CLOSIDs.

**Detect orphaned groups:**

```bash
ls /sys/fs/resctrl/ | grep dra-
```

**Check if any group has active tasks:**

```bash
for g in $(ls /sys/fs/resctrl/ | grep dra-); do
  tasks=$(cat /sys/fs/resctrl/$g/tasks 2>/dev/null | wc -w)
  echo "$g: $tasks tasks"
done
```

**Remove orphaned groups** (move tasks to root first if any remain):

```bash
for g in $(ls /sys/fs/resctrl/ | grep dra-); do
  # Move any remaining tasks to root group
  cat /sys/fs/resctrl/$g/tasks > /sys/fs/resctrl/tasks 2>/dev/null || true
  rmdir /sys/fs/resctrl/$g
  echo "Removed $g"
done
```

The driver also runs `RecoverAllocations` at startup to reconcile state with existing ResourceClaims, so restarting the driver DaemonSet pod is usually sufficient.

---

## 3. Increasing the Partition Count

The number of partitions per cache domain is controlled by `--partition-count` (default: 4).

**Constraints:**
- `partition_count ≤ num_closids - 1` (one CLOSID reserved for the default group)
- `total_ways / partition_count ≥ min_cbm_bits` (minimum contiguous ways per partition)

Check limits on a node:

```bash
cat /sys/fs/resctrl/info/L3/num_closids   # max CLOSIDs
cat /sys/fs/resctrl/info/L3/min_cbm_bits  # minimum ways per partition
cat /sys/fs/resctrl/info/L3/cbm_mask      # full mask (popcount = total ways)
```

To change the partition count, update the Helm values or DaemonSet args:

```yaml
# values.yaml
driver:
  partitionCount: 8
```

or in the raw manifest:

```yaml
args:
  - --partition-count=8
```

Then rolling-restart the DaemonSet. The driver will delete and recreate all resctrl groups on restart.

---

## 4. CLOSID Exhaustion

**Symptom:** Driver logs `partition count N exceeds available CLOSIDs M`. The driver fails to start.

**Diagnosis:**

```bash
cat /sys/fs/resctrl/info/L3/num_closids
```

Common values: 16 (most Intel Xeon), 4-8 (some older CPUs).

**Remediation:**
- Reduce `--partition-count` so that `partition_count ≤ num_closids - 1`.
- Ensure no other software is consuming CLOSIDs (check `ls /sys/fs/resctrl/` for non-dra groups).
- On some Intel platforms, CLOSIDs can be increased via BIOS settings.

---

## 5. Checking Driver Health

**Liveness endpoint** (checks resctrl mount + CDI spec file):

```bash
kubectl exec -n dra-cache-partition <pod> -- curl -s http://localhost:8081/healthz | jq .
```

Healthy response:
```json
{"status":"healthy","checks":{"cdi_spec":"ok","resctrl":"ok"}}
```

Unhealthy response (503) includes which check failed:
```json
{"status":"unhealthy","checks":{"cdi_spec":"missing: stat /var/run/cdi/...: no such file or directory","resctrl":"ok"}}
```

**Readiness endpoint** (checks if driver has completed initialization):

```bash
kubectl exec -n dra-cache-partition <pod> -- curl -s -o /dev/null -w "%{http_code}" http://localhost:8081/readyz
```

Returns `200` when ready, `503` while initializing.

---

## 6. Reading Driver Metrics

The driver exposes Prometheus metrics on `:8081/metrics`.

**Key metrics:**

| Metric | Description |
|---|---|
| `dra_cache_partition_partitions_total` | Total partitions advertised |
| `dra_cache_partition_partitions_allocated` | Currently allocated partitions |
| `dra_cache_partition_prepare_total{result}` | NodePrepareResources call count |
| `dra_cache_partition_unprepare_total{result}` | NodeUnprepareResources call count |
| `dra_cache_partition_prepare_duration_seconds` | Prepare call latency histogram |
| `dra_cache_partition_unprepare_duration_seconds` | Unprepare call latency histogram |
| `dra_cache_partition_resctrl_group_create_total{result}` | resctrl group creation count |
| `dra_cache_partition_resctrl_group_delete_total{result}` | resctrl group deletion count |

**Scrape metrics manually:**

```bash
kubectl exec -n dra-cache-partition <pod> -- curl -s http://localhost:8081/metrics | grep dra_cache_partition
```

---

## 7. Requesting Cache by Size

Users never need to know the hardware partition sizes. Cluster admins install named DeviceClasses; users reference them by name.

### Install the size-tier DeviceClasses (one-time, cluster admin)

```bash
kubectl apply -f deployments/device-classes-sized.yaml
```

This creates:

| DeviceClass | Meaning |
|---|---|
| `cache-partition-any` | Any L3 partition (just needs isolation) |
| `cache-partition-quarter` | ≥ 25% of L3 cache ways |
| `cache-partition-third` | ≥ 33% of L3 cache ways |
| `cache-partition-half` | ≥ 50% of L3 cache ways |
| `cache-partition-4mib` | ≥ 4 MiB absolute |
| `cache-partition-8mib` | ≥ 8 MiB absolute |
| `cache-partition-mba` | Any MBA bandwidth device |

Fraction-based classes (`-quarter`, `-third`, `-half`) work on any CPU model. Absolute-size classes (`-4mib`, `-8mib`) are hardware-specific — verify partition sizes match before using.

### User workflow

```yaml
# User just references the class by name — no hardware knowledge needed.
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: my-cache
spec:
  devices:
    requests:
      - name: cache
        exactly:
          deviceClassName: cache-partition-quarter
```

### Discover what sizes are available on a node

```bash
kubectl get resourceslices -o json | \
  jq -r '.items[].spec.devices[] |
    select(has("attributes")) |
    "\(.name)\t\(.attributes.cacheSizeLabel.string // "N/A")\t\(.attributes.cacheSizeBytes.int // 0)B"'
```

Example output:
```
cache0-part0    5MiB    5242880B
cache0-part1    5MiB    5242880B
cache0-part2    5MiB    5242880B
cache0-part3    5MiB    5242880B
mba0-part0      (MBA device)
```

### Hardware-aware custom DeviceClass

If the preset classes don't fit, admins can create custom ones. Use the fraction-based pattern to stay portable:

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: cache-partition-20pct
spec:
  selectors:
    - cel:
        expression: >
          device.driver == 'cache-partition.fabiendupont.io' &&
          device.attributes['cache-partition.fabiendupont.io'].cacheWays * 5 >=
          device.attributes['cache-partition.fabiendupont.io'].cacheTotalWays
```

### Hardware constraints users don't need to worry about

The driver enforces these automatically:
- **Way granularity**: partition sizes are always multiples of one cache way (can be non-round, e.g. 2.5 MiB on a 30 MB / 12-way cache). `cacheWaySizeBytes` attribute shows this.
- **Minimum size**: the `minPartitionSizeBytes` attribute shows the hardware floor (= `min_cbm_bits × way_size`). A request below this floor will find no matching devices.
- **No wasted ways**: the driver distributes all ways round-robin across partitions so the first few partitions may be one way larger than the rest.

---

## 8. Memory Bandwidth Allocation (MBA)

MBA is automatically detected at startup. On nodes where `ls /sys/fs/resctrl/info/MB/` succeeds, the driver publishes `mba<N>-part<M>` DRA devices alongside `cache<N>-part<M>` devices. Both device types share the same resctrl group.

**Detect MBA support:**

```bash
ls /sys/fs/resctrl/info/MB/
cat /sys/fs/resctrl/info/MB/ctrl_in_percentages  # 1 = percent mode, 0 = MBps mode
cat /sys/fs/resctrl/info/MB/min_bandwidth        # minimum bandwidth value
cat /sys/fs/resctrl/info/MB/bandwidth_gran       # step granularity
```

**Enable MBA throttling** — set `--mba-bandwidth` on the driver:

```yaml
# values.yaml (Helm)
driver:
  mbaBandwidth: 70   # 70% in percent mode, or 70 MBps in MBps mode
```

or in the raw manifest args:
```
- --mba-bandwidth=70
```

When `--mba-bandwidth=-1` (default):
- Percent mode: configures 100% (no throttling, but MBA schemata is written and `mba*` devices are advertised)
- MBps mode: MBA skipped (no `mba*` devices) — set an explicit value

**Verify MBA schemata on a group:**

```bash
cat /sys/fs/resctrl/dra-cache0-part0/schemata
# Expected (with MBA enabled):
# L3:0=1f
# MB:0=70
```

**Claim an MBA device** (bandwidth control without cache isolation):

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: mba-claim
spec:
  devices:
    requests:
    - name: mba
      deviceClassName: cache-partition.fabiendupont.io
      selectors:
      - cel:
          expression: device.attributes["mbaMode"] == "percent"
```

**Claim both cache and bandwidth** — use two ResourceClaims (one for `cache*`, one for `mba*`) referencing the same partition index. The CDI hook for each writes the container PID to the same resctrl group's tasks file, which is idempotent.

---

## 9. Cache Occupancy and Bandwidth Monitoring (CMT/MBM)

When the hardware supports Cache Monitoring Technology (CMT) and Memory Bandwidth Monitoring (MBM), the driver scrapes per-partition metrics from resctrl `mon_data/` and exposes them on `/metrics`.

**Check hardware support:**
```bash
ls /sys/fs/resctrl/info/L3_MON/
cat /sys/fs/resctrl/info/L3_MON/mon_features
# Expected: llc_occupancy, mbm_local_bytes, mbm_total_bytes
```

**Metrics available** (labeled by `partition` and `cache_group_id`):

| Metric | Description |
|---|---|
| `dra_cache_partition_cache_occupancy_bytes` | Bytes of L3 currently in use by this partition |
| `dra_cache_partition_memory_bandwidth_local_bytes_per_second` | Local DRAM bandwidth rate |
| `dra_cache_partition_memory_bandwidth_total_bytes_per_second` | Total memory bandwidth rate |

**Scrape interval**: controlled by `--monitor-interval` (default `10s`). The driver logs `"Monitoring unavailable"` at startup if CMT/MBM is not present on the node.

---

## 10. Slow Memory Bandwidth Allocation (SMBA)

On Intel Xeon systems with on-package HBM (e.g. Sapphire Rapids HBM), SMBA controls bandwidth to slow (DRAM) memory separately from on-package bandwidth.

**Check support:**
```bash
ls /sys/fs/resctrl/info/SMBA/
```

**Enable via flag** (same logic as `--mba-bandwidth`):
```yaml
driver:
  smbaBandwidth: 50   # percent in percent mode
```

SMBA publishes `smba<N>-part<M>` DRA devices alongside CAT and MBA devices. The CDI hook writes the same resctrl group; the SMBA line is appended to the schemata:
```
L3:0=1f
MB:0=70
SMBA:0=50
```

---

## 11. L2 CAT (pass-through)

When L2 CAT is present (`info/L2/`), the driver applies the full L2 CBM mask to every resctrl group so that the kernel accepts the schemata write. No per-partition L2 splitting is performed — L2 domains are per-core and too granular to partition alongside L3.

L2 information is exposed as informational device attributes: `l2Ways`, `l2TotalWays`, `l2CbmHex`. These can be used in CEL selectors for topology but do not constrain allocation.

---

## 12. Known Limitations

- **One partition per ResourceClaim**: The driver allocates exactly one cache partition per claim. Multiple partitions per workload require multiple claims.
- **No dynamic repartitioning**: The partition layout is set at driver startup and requires a restart to change (basic mode). Structured mode allocates on demand.
- **CDI hook stderr**: Hook error output goes to CRI-O logs (`journalctl -u crio`), not to driver logs. Check CRI-O logs if containers fail to start with cache partition claims.
- **L2 partitioning**: L2 CAT is pass-through only — all partitions share the full L2 CBM. Independent L2 isolation is not supported.
- **MBM counter wrap**: MBM bandwidth counters are 64-bit monotonic; the driver skips rate computation on a wrap to avoid negative values. One sample (10s by default) is lost per wrap event.

---

## 13. Build and Multi-Arch Architecture (CI & Release)

The container image supports both `linux/amd64` and `linux/arm64`.

### Native Go Cross-Compilation vs QEMU Emulation

In CI/CD environments (GitHub Actions `ubuntu-latest` x86_64 runners):
- Full user-space QEMU emulation of an `arm64` container toolchain to run `go build` takes ~14 minutes due to emulation overhead.
- The `Dockerfile` uses Docker Buildx multi-stage native cross-compilation:
  ```dockerfile
  FROM --platform=$BUILDPLATFORM registry.access.redhat.com/ubi10/go-toolset:1.26 AS builder
  ARG TARGETARCH
  ...
  RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -buildvcs=false -o dra-resctrl ./cmd/driver
  ```
  The builder stage runs natively on the runner architecture (`BUILDPLATFORM`) and utilizes Go's native cross-compiler targeting `TARGETARCH`. The resulting binary is then copied into the target-architecture `ubi-micro` base image. This reduces arm64 build times from ~14 minutes to under a minute in CI.

### Local Multi-Arch Build

```bash
# Build amd64
podman build --platform linux/amd64 -t dra-resctrl:amd64 .

# Build arm64 (fast cross-compile)
podman build --platform linux/arm64 --build-arg TARGETARCH=arm64 -t dra-resctrl:arm64 .
```
