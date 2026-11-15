package host

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dale-tomson/Tarsier/internal/model"
)

var defaultExcludeFS = map[string]struct{}{
	"tmpfs":       {},
	"overlay":     {},
	"squashfs":    {},
	"devtmpfs":    {},
	"proc":        {},
	"sysfs":       {},
	"cgroup":      {},
	"cgroup2":     {},
	"devpts":      {},
	"mqueue":      {},
	"hugetlbfs":   {},
	"debugfs":     {},
	"tracefs":     {},
	"securityfs":  {},
	"fusectl":     {},
	"nsfs":        {},
	"autofs":      {},
	"binfmt_misc": {},
	"configfs":    {},
	"ramfs":       {},
	"pstore":      {},
	"bpf":         {},
}

// Regex for physical block devices in diskstats (whole disks, no partitions)
var physicalDiskRe = regexp.MustCompile(`^(sd[a-z]+|nvme[0-9]+n[0-9]+|vd[a-z]+|xvd[a-z]+|mmcblk[0-9]+)$`)

type diskIOSnapshot struct {
	readBytes  uint64
	writeBytes uint64
}

// discoverMounts reads /proc/1/mountinfo (or /proc/mounts) and returns deduplicated non-pseudo mount points.
func discoverMounts(procDir string, configuredDisks []string, excludeFS []string) ([]string, error) {
	if len(configuredDisks) > 0 {
		return append([]string(nil), configuredDisks...), nil
	}

	excludes := make(map[string]struct{}, len(defaultExcludeFS)+len(excludeFS))
	for k := range defaultExcludeFS {
		excludes[k] = struct{}{}
	}
	for _, fs := range excludeFS {
		excludes[strings.ToLower(strings.TrimSpace(fs))] = struct{}{}
	}

	// Try /proc/1/mountinfo first, then /proc/mounts
	mountinfoPath := filepath.Join(procDir, "1", "mountinfo")
	if _, err := os.Stat(mountinfoPath); os.IsNotExist(err) {
		mountinfoPath = filepath.Join(procDir, "mountinfo")
	}

	if _, err := os.Stat(mountinfoPath); err == nil {
		return parseMountinfo(mountinfoPath, excludes)
	}

	mountsPath := filepath.Join(procDir, "mounts")
	if _, err := os.Stat(mountsPath); err == nil {
		return parseMounts(mountsPath, excludes)
	}

	// Fallback to root
	return []string{"/"}, nil
}

func parseMountinfo(path string, excludes map[string]struct{}) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open mountinfo: %w", err)
	}
	defer f.Close()

	// Map majorMinor -> shortest mountpoint
	devMap := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}

		majorMinor := fields[2]
		mountPoint := fields[4]

		// Find separator "-"
		hyphenIdx := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				hyphenIdx = i
				break
			}
		}
		if hyphenIdx == -1 || hyphenIdx+1 >= len(fields) {
			continue
		}

		fsType := strings.ToLower(fields[hyphenIdx+1])
		if _, excluded := excludes[fsType]; excluded {
			continue
		}

		if existing, exists := devMap[majorMinor]; !exists {
			devMap[majorMinor] = mountPoint
		} else if len(mountPoint) < len(existing) {
			devMap[majorMinor] = mountPoint
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan mountinfo: %w", err)
	}

	mounts := make([]string, 0, len(devMap))
	for _, m := range devMap {
		mounts = append(mounts, m)
	}
	if len(mounts) == 0 {
		mounts = append(mounts, "/")
	}
	return mounts, nil
}

func parseMounts(path string, excludes map[string]struct{}) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open mounts: %w", err)
	}
	defer f.Close()

	devMap := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}

		dev := fields[0]
		mountPoint := fields[1]
		fsType := strings.ToLower(fields[2])

		if _, excluded := excludes[fsType]; excluded {
			continue
		}

		if existing, exists := devMap[dev]; !exists {
			devMap[dev] = mountPoint
		} else if len(mountPoint) < len(existing) {
			devMap[dev] = mountPoint
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan mounts: %w", err)
	}

	mounts := make([]string, 0, len(devMap))
	for _, m := range devMap {
		mounts = append(mounts, m)
	}
	if len(mounts) == 0 {
		mounts = append(mounts, "/")
	}
	return mounts, nil
}

// readDiskSpace inspects filesystem space for mounts using statfs.
func readDiskSpace(mounts []string, hostRoot string, now int64) []model.MetricSample {
	var samples []model.MetricSample
	for _, mount := range mounts {
		targetPath := mount
		if hostRoot != "" && hostRoot != "/" {
			targetPath = filepath.Join(hostRoot, mount)
		}

		stat, err := statfsFunc(targetPath)
		if err != nil {
			continue
		}

		totalBytes := stat.Blocks * stat.Bsize
		availBytes := stat.Bavail * stat.Bsize
		var usedBytes uint64
		if stat.Blocks >= stat.Bfree {
			usedBytes = (stat.Blocks - stat.Bfree) * stat.Bsize
		}

		var usedPct float64
		denom := usedBytes + availBytes
		if denom > 0 {
			usedPct = (float64(usedBytes) / float64(denom)) * 100.0
		}

		entity := "disk:" + mount
		samples = append(samples,
			model.MetricSample{Entity: entity, Metric: "disk_total_bytes", TS: now, Value: float64(totalBytes)},
			model.MetricSample{Entity: entity, Metric: "disk_avail_bytes", TS: now, Value: float64(availBytes)},
			model.MetricSample{Entity: entity, Metric: "disk_used_bytes", TS: now, Value: float64(usedBytes)},
			model.MetricSample{Entity: entity, Metric: "disk_used_pct", TS: now, Value: usedPct},
		)
	}
	return samples
}

// readDiskIO parses /proc/diskstats and returns total read and write bytes across whole physical devices.
func readDiskIO(procDir string) (diskIOSnapshot, error) {
	path := filepath.Join(procDir, "diskstats")
	f, err := os.Open(path)
	if err != nil {
		return diskIOSnapshot{}, fmt.Errorf("open diskstats: %w", err)
	}
	defer f.Close()

	var snap diskIOSnapshot
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}

		devName := fields[2]
		if !physicalDiskRe.MatchString(devName) {
			continue
		}

		// Field 5: sectors read (index 5)
		// Field 9: sectors written (index 9)
		sectorsRead, err1 := strconv.ParseUint(fields[5], 10, 64)
		sectorsWritten, err2 := strconv.ParseUint(fields[9], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}

		snap.readBytes += sectorsRead * 512
		snap.writeBytes += sectorsWritten * 512
	}

	if err := scanner.Err(); err != nil {
		return diskIOSnapshot{}, fmt.Errorf("scan diskstats: %w", err)
	}
	return snap, nil
}
