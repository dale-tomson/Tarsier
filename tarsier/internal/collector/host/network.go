package host

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type netIOSnapshot struct {
	rxBytes uint64
	txBytes uint64
}

// isExcludedInterface checks if interface is loopback or a virtual container network bridge/tap.
func isExcludedInterface(name string) bool {
	if name == "lo" {
		return true
	}
	prefixes := []string{"veth", "docker", "br-", "flannel", "cni", "cali", "tun", "tap"}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// readNetIO parses /proc/net/dev and sums rx/tx bytes across physical host network interfaces.
func readNetIO(procDir string) (netIOSnapshot, error) {
	path := filepath.Join(procDir, "net", "dev")
	f, err := os.Open(path)
	if err != nil {
		return netIOSnapshot{}, fmt.Errorf("open net/dev: %w", err)
	}
	defer f.Close()

	var snap netIOSnapshot
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		colonIdx := strings.IndexByte(line, ':')
		if colonIdx == -1 {
			continue
		}

		ifaceName := strings.TrimSpace(line[:colonIdx])
		if isExcludedInterface(ifaceName) {
			continue
		}

		fields := strings.Fields(line[colonIdx+1:])
		if len(fields) < 16 {
			continue
		}

		// Field 0: bytes received
		// Field 8: bytes transmitted
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}

		snap.rxBytes += rx
		snap.txBytes += tx
	}

	if err := scanner.Err(); err != nil {
		return netIOSnapshot{}, fmt.Errorf("scan net/dev: %w", err)
	}
	return snap, nil
}
