package driver

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"k8s.io/klog/v2"

	"github.com/fabiendupont/k8s-dra-driver-resctrl/pkg/cache"
)

const (
	cdiVendor  = "resctrl.fabiendupont.io"
	cdiClass   = "partition"
	cdiVersion = "0.6.0"
)

// CDISpec is a minimal CDI spec structure.
type CDISpec struct {
	CDIVersion string      `json:"cdiVersion"`
	Kind       string      `json:"kind"`
	Devices    []CDIDevice `json:"devices"`
}

// CDIDevice is a device entry in a CDI spec.
type CDIDevice struct {
	Name           string           `json:"name"`
	ContainerEdits CDIContainerEdit `json:"containerEdits"`
}

// CDIContainerEdit defines modifications to apply to a container.
type CDIContainerEdit struct {
	Env   []string  `json:"env,omitempty"`
	Hooks []CDIHook `json:"hooks,omitempty"`
}

// CDIHook is an OCI lifecycle hook.
type CDIHook struct {
	HookName string   `json:"hookName"`
	Path     string   `json:"path"`
	Args     []string `json:"args,omitempty"`
}

// CDIDeviceID returns the CDI device ID for a partition.
func CDIDeviceID(partitionID string) string {
	return cdiVendor + "/" + cdiClass + "=" + partitionID
}

// WriteCDISpecs writes CDI spec files for all cache partitions to the given directory.
func WriteCDISpecs(cdiDir, hookBinaryPath string, perCache [][]*cache.CachePartition) error {
	if err := os.MkdirAll(cdiDir, 0755); err != nil {
		return fmt.Errorf("creating CDI directory: %w", err)
	}

	resctrlHook := func(group string) CDIHook {
		return CDIHook{
			HookName: "createRuntime",
			Path:     hookBinaryPath,
			Args:     []string{hookBinaryPath, "--cdi-hook", "--group=" + group},
		}
	}

	var devices []CDIDevice
	for _, parts := range perCache {
		for _, p := range parts {
			// CAT cache partition device.
			dev := CDIDevice{
				Name: p.ID,
				ContainerEdits: CDIContainerEdit{
					Env: []string{
						fmt.Sprintf("DRA_CACHE_PARTITION=%s", p.ID),
						fmt.Sprintf("DRA_CACHE_WAYS=%d", p.Ways),
						fmt.Sprintf("DRA_CACHE_LEVEL=%s", p.Level),
						fmt.Sprintf("DRA_CACHE_NUMA_NODE=%d", p.NUMANode),
						fmt.Sprintf("DRA_CACHE_CBM=%s", p.CBM),
					},
					Hooks: []CDIHook{resctrlHook(p.ResctrlGroup)},
				},
			}
			devices = append(devices, dev)

			// MBA bandwidth device — same resctrl group, different env vars.
			if p.MBAThrottle > 0 {
				mbaDev := CDIDevice{
					Name: cache.MBADeviceID(p),
					ContainerEdits: CDIContainerEdit{
						Env: []string{
							fmt.Sprintf("DRA_MBA_PARTITION=%s", cache.MBADeviceID(p)),
							fmt.Sprintf("DRA_MBA_THROTTLE=%d", p.MBAThrottle),
							fmt.Sprintf("DRA_MBA_MODE=%s", p.MBAMode),
							fmt.Sprintf("DRA_MBA_DOMAIN=%d", p.CacheID),
							fmt.Sprintf("DRA_MBA_NUMA_NODE=%d", p.NUMANode),
						},
						Hooks: []CDIHook{resctrlHook(p.ResctrlGroup)},
					},
				}
				devices = append(devices, mbaDev)
			}

			// SMBA device — slow memory bandwidth (HBM), same resctrl group.
			if p.SMBAThrottle > 0 {
				smbaDev := CDIDevice{
					Name: cache.SMBADeviceID(p),
					ContainerEdits: CDIContainerEdit{
						Env: []string{
							fmt.Sprintf("DRA_SMBA_PARTITION=%s", cache.SMBADeviceID(p)),
							fmt.Sprintf("DRA_SMBA_THROTTLE=%d", p.SMBAThrottle),
							fmt.Sprintf("DRA_SMBA_MODE=%s", p.SMBAMode),
							fmt.Sprintf("DRA_SMBA_DOMAIN=%d", p.CacheID),
							fmt.Sprintf("DRA_SMBA_NUMA_NODE=%d", p.NUMANode),
						},
						Hooks: []CDIHook{resctrlHook(p.ResctrlGroup)},
					},
				}
				devices = append(devices, smbaDev)
			}
		}
	}

	spec := CDISpec{
		CDIVersion: cdiVersion,
		Kind:       cdiVendor + "/" + cdiClass,
		Devices:    devices,
	}

	specPath := filepath.Join(cdiDir, cdiVendor+"-"+cdiClass+".json")
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling CDI spec: %w", err)
	}

	if err := os.WriteFile(specPath, data, 0640); err != nil {
		return fmt.Errorf("writing CDI spec %s: %w", specPath, err)
	}

	klog.InfoS("Wrote CDI spec", "path", specPath, "devices", len(devices))
	return nil
}

// CleanupCDISpecs removes CDI spec files written by the driver.
func CleanupCDISpecs(cdiDir string) {
	specPath := filepath.Join(cdiDir, cdiVendor+"-"+cdiClass+".json")
	if err := os.Remove(specPath); err != nil && !os.IsNotExist(err) {
		klog.ErrorS(err, "Failed to remove CDI spec", "path", specPath)
	}
}

// InstallHookBinary copies the driver binary to the host-accessible plugin
// directory so CRI-O can execute it as a CDI hook.
func InstallHookBinary(hostPluginDir string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving self: %w", err)
	}

	hookPath := filepath.Join(hostPluginDir, "dra-cache-partition-hook")

	srcFile, err := os.Open(self)
	if err != nil {
		return "", fmt.Errorf("opening self binary: %w", err)
	}
	defer func() { _ = srcFile.Close() }()

	dstFile, err := os.OpenFile(hookPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return "", fmt.Errorf("creating hook binary: %w", err)
	}

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		_ = dstFile.Close()
		return "", fmt.Errorf("writing hook binary: %w", err)
	}
	if err := dstFile.Close(); err != nil {
		return "", fmt.Errorf("finalizing hook binary: %w", err)
	}

	klog.InfoS("Installed hook binary", "path", hookPath)
	return hookPath, nil
}

// CachePartitionSizeBytes estimates partition size based on ways and total cache size.
func CachePartitionSizeBytes(ways, totalWays int, totalSizeBytes int64) int64 {
	if totalWays == 0 {
		return 0
	}
	return totalSizeBytes * int64(ways) / int64(totalWays)
}

// writeClaimCDISpec writes a per-claim CDI spec file for structured-parameters mode.
// Returns the CDI device ID to pass back to the kubelet and the path of the written file.
func writeClaimCDISpec(cdiDir, hookBinaryPath, claimUID, resctrlGroup string, cacheID, ways int, cbmHex string) (cdiDeviceID, specPath string, err error) {
	if err := os.MkdirAll(cdiDir, 0755); err != nil {
		return "", "", fmt.Errorf("creating CDI directory: %w", err)
	}

	deviceName := fmt.Sprintf("cache%d-%dway-%s", cacheID, ways, claimUID[:8])
	cdiDeviceID = cdiVendor + "/" + cdiClass + "=" + deviceName

	spec := CDISpec{
		CDIVersion: cdiVersion,
		Kind:       cdiVendor + "/" + cdiClass,
		Devices: []CDIDevice{{
			Name: deviceName,
			ContainerEdits: CDIContainerEdit{
				Env: []string{
					fmt.Sprintf("DRA_CACHE_PARTITION=%s", deviceName),
					fmt.Sprintf("DRA_CACHE_WAYS=%d", ways),
					fmt.Sprintf("DRA_CACHE_CBM=%s", cbmHex),
					fmt.Sprintf("DRA_CACHE_LEVEL=L3"),
					fmt.Sprintf("DRA_CACHE_DOMAIN=%d", cacheID),
				},
				Hooks: []CDIHook{{
					HookName: "createRuntime",
					Path:     hookBinaryPath,
					Args:     []string{hookBinaryPath, "--cdi-hook", "--group=" + resctrlGroup},
				}},
			},
		}},
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", "", fmt.Errorf("marshaling CDI spec: %w", err)
	}

	specPath = filepath.Join(cdiDir, cdiVendor+"-claim-"+claimUID[:8]+".json")
	if err := os.WriteFile(specPath, data, 0640); err != nil {
		return "", "", fmt.Errorf("writing CDI spec %s: %w", specPath, err)
	}

	klog.V(4).InfoS("Wrote per-claim CDI spec", "path", specPath, "device", deviceName)
	return cdiDeviceID, specPath, nil
}

// removeClaimCDISpec removes a per-claim CDI spec file created by writeClaimCDISpec.
func removeClaimCDISpec(specPath string) {
	if specPath == "" {
		return
	}
	if err := os.Remove(specPath); err != nil && !os.IsNotExist(err) {
		klog.ErrorS(err, "Failed to remove per-claim CDI spec", "path", specPath)
	}
}
