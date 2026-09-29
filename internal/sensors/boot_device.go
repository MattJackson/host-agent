package sensors

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BootDeviceNames reads the host's /boot mount entry and resolves its block
// device through sysfs. Reading /proc and /sys does not read the mounted media.
// Both the partition name and parent disk name are returned because SMART
// tools report whole disks while mount tables commonly report a partition.
func BootDeviceNames(mountsPath, sysClassBlock string) (map[string]struct{}, error) {
	f, err := os.Open(mountsPath)
	if err != nil {
		return nil, fmt.Errorf("open host mounts: %w", err)
	}
	defer f.Close()

	var source string
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 2 && fields[1] == "/boot" {
			source = fields[0]
			break
		}
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("read host mounts: %w", err)
	}
	if source == "" || !strings.HasPrefix(source, "/dev/") {
		return nil, nil
	}

	names := map[string]struct{}{filepath.Base(source): {}}
	base := filepath.Base(source)
	resolved, err := filepath.EvalSymlinks(filepath.Join(sysClassBlock, base))
	if err != nil {
		return names, nil // retain the exact source name when sysfs cannot resolve it
	}
	parts := strings.Split(filepath.ToSlash(resolved), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "block" {
			names[parts[i+1]] = struct{}{}
			break
		}
	}
	return names, nil
}

func IsExcludedDevice(device string, names map[string]struct{}) bool {
	_, ok := names[filepath.Base(device)]
	return ok
}
