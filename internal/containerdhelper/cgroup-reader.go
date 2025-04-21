package containerdhelper

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/cgroups"
	"github.com/pkg/errors"
)

func GetCPUMetrics(cgroupRoot string, procRoot string, pid int) (nrBurst, burstSeconds float64, err error) {
	if mode := cgroups.Mode(); mode != cgroups.Unified {
		return 0, 0, fmt.Errorf("unknown cgroup mode: %v", mode)
	}
	cgroupBytes, err := os.ReadFile(fmt.Sprintf("%v/%v/cgroup", procRoot, pid))
	if err != nil {
		return 0, 0, errors.Wrap(err, "failed to get cgroup2 path")
	}
	text := string(cgroupBytes)
	parts := strings.SplitN(text, ":", 3)
	if len(parts) < 3 {
		return 0, 0, fmt.Errorf("invalid cgroup entry: %q", text)
	}
	if parts[0] != "0" || parts[1] != "" {
		return 0, 0, fmt.Errorf("invalid cgroup entry: %q", text)
	}
	path := strings.TrimSuffix(parts[2], "\n")

	fullPath := cgroupRoot + path + "/cpu.stat"
	// we need to parse the file manually because currently
	// cgroup package does not support reporting on burst stats
	stats, err := readKVStatsFile(fullPath)
	if err != nil {
		return 0, 0, err
	}

	nrBurst = float64(stats["nr_bursts"])
	burstSeconds = (time.Duration(stats["burst_usec"]) * time.Microsecond).Seconds()
	return
}

func readKVStatsFile(path string) (map[string]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	result := map[string]int64{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		name, value, err := parseKV(s.Text())
		if err != nil {
			return nil, fmt.Errorf("error while parsing %s (line=%q): %w", path, s.Text(), err)
		}
		result[name] = value
	}
	return result, s.Err()
}

func parseKV(raw string) (string, int64, error) {
	parts := strings.Fields(raw)
	if len(parts) != 2 {
		return "", 0, errors.New("invalid format")
	}
	value, err := strconv.ParseInt(parts[1], 10, 64)
	return parts[0], value, err
}
