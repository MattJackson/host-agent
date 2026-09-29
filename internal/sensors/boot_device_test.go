package sensors

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBootDeviceNamesFindsParentDisk(t *testing.T) {
	root := t.TempDir()
	mounts := filepath.Join(root, "mounts")
	if err := os.WriteFile(mounts, []byte("/dev/sdk1 /boot vfat rw 0 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	classBlock := filepath.Join(root, "sys", "class", "block")
	if err := os.MkdirAll(classBlock, 0o755); err != nil {
		t.Fatal(err)
	}
	device := filepath.Join(root, "sys", "devices", "pci0000:00", "block", "sdk", "sdk1")
	if err := os.MkdirAll(device, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(device, filepath.Join(classBlock, "sdk1")); err != nil {
		t.Fatal(err)
	}

	names, err := BootDeviceNames(mounts, classBlock)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sdk", "sdk1"} {
		if _, ok := names[name]; !ok {
			t.Errorf("BootDeviceNames() missing %q in %v", name, names)
		}
	}
	if !IsExcludedDevice("/dev/sdk", names) {
		t.Fatal("whole boot disk was not excluded")
	}
}

func TestBootDeviceNamesNoBootMount(t *testing.T) {
	mounts := filepath.Join(t.TempDir(), "mounts")
	if err := os.WriteFile(mounts, []byte("/dev/sda1 / ext4 rw 0 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	names, err := BootDeviceNames(mounts, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Fatalf("BootDeviceNames() = %v, want empty", names)
	}
}
