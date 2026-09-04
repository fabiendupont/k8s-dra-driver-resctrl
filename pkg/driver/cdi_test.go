package driver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fabiendupont/k8s-dra-driver-resctrl/pkg/cache"
)

func makePartitions() [][]*cache.CachePartition {
	return [][]*cache.CachePartition{
		{
			{ID: "cache0-part0", ResctrlGroup: "dra-cache0-part0", Ways: 5, TotalWays: 20, CBM: "1f", Level: "L3", NUMANode: 0},
			{ID: "cache0-part1", ResctrlGroup: "dra-cache0-part1", Ways: 5, TotalWays: 20, CBM: "3e0", Level: "L3", NUMANode: 0},
		},
	}
}

func TestWriteCDISpecs(t *testing.T) {
	dir := t.TempDir()

	if err := WriteCDISpecs(dir, "/hook/binary", makePartitions()); err != nil {
		t.Fatalf("WriteCDISpecs: %v", err)
	}

	specPath := filepath.Join(dir, cdiVendor+"-"+cdiClass+".json")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("reading spec: %v", err)
	}

	var spec CDISpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parsing spec: %v", err)
	}

	if len(spec.Devices) != 2 {
		t.Fatalf("got %d devices, want 2", len(spec.Devices))
	}

	dev := spec.Devices[0]
	if dev.Name != "cache0-part0" {
		t.Errorf("device name = %q, want %q", dev.Name, "cache0-part0")
	}
	if len(dev.ContainerEdits.Hooks) != 1 {
		t.Fatalf("got %d hooks, want 1", len(dev.ContainerEdits.Hooks))
	}
	hook := dev.ContainerEdits.Hooks[0]
	if hook.HookName != "createRuntime" {
		t.Errorf("hookName = %q, want createRuntime", hook.HookName)
	}
	if hook.Path != "/hook/binary" {
		t.Errorf("hook path = %q, want /hook/binary", hook.Path)
	}
	groupArg := "--group=dra-cache0-part0"
	found := false
	for _, arg := range hook.Args {
		if arg == groupArg {
			found = true
		}
	}
	if !found {
		t.Errorf("hook args %v missing %q", hook.Args, groupArg)
	}
}

func TestWriteCDISpecs_WithMBA(t *testing.T) {
	dir := t.TempDir()

	perCache := [][]*cache.CachePartition{
		{
			{ID: "cache0-part0", ResctrlGroup: "dra-cache0-part0", CacheID: 0, Index: 0,
				Ways: 5, TotalWays: 20, CBM: "1f", Level: "L3", NUMANode: 0,
				MBAThrottle: 70, MBAMode: "percent"},
		},
	}

	if err := WriteCDISpecs(dir, "/hook/binary", perCache); err != nil {
		t.Fatalf("WriteCDISpecs: %v", err)
	}

	specPath := filepath.Join(dir, cdiVendor+"-"+cdiClass+".json")
	data, _ := os.ReadFile(specPath)
	var spec CDISpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parsing spec: %v", err)
	}

	// Expect 2 devices: cache0-part0 and mba0-part0.
	if len(spec.Devices) != 2 {
		t.Fatalf("got %d CDI devices, want 2", len(spec.Devices))
	}

	names := map[string]CDIDevice{}
	for _, d := range spec.Devices {
		names[d.Name] = d
	}

	mbaDev, ok := names["mba0-part0"]
	if !ok {
		t.Fatal("missing mba0-part0 CDI device")
	}
	if len(mbaDev.ContainerEdits.Hooks) != 1 || mbaDev.ContainerEdits.Hooks[0].HookName != "createRuntime" {
		t.Error("MBA device missing createRuntime hook")
	}

	foundThrottle := false
	for _, env := range mbaDev.ContainerEdits.Env {
		if env == "DRA_MBA_THROTTLE=70" {
			foundThrottle = true
		}
	}
	if !foundThrottle {
		t.Errorf("MBA CDI device env %v missing DRA_MBA_THROTTLE=70", mbaDev.ContainerEdits.Env)
	}
}

func TestCDIDeviceID(t *testing.T) {
	id := CDIDeviceID("cache0-part0")
	if !strings.HasSuffix(id, "=cache0-part0") {
		t.Errorf("CDIDeviceID = %q, want suffix =cache0-part0", id)
	}
}

func TestCleanupCDISpecs(t *testing.T) {
	dir := t.TempDir()
	_ = WriteCDISpecs(dir, "/hook/binary", makePartitions())

	CleanupCDISpecs(dir)

	specPath := filepath.Join(dir, cdiVendor+"-"+cdiClass+".json")
	if _, err := os.Stat(specPath); !os.IsNotExist(err) {
		t.Errorf("spec file still exists after cleanup")
	}

	// Calling again on missing file must not panic or error.
	CleanupCDISpecs(dir)
}
