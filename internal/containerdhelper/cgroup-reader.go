package containerdhelper

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
)

func GetCPUMetrics(cgroupFolder string, procRoot string, pid int) (nrBurst, burstSeconds float64, err error) {
	// we need to parse the file manually because currently
	// cgroup package from containerd does not support reporting on burst stats
	stats, err := readKVStatsFile(cgroupFolder + "/cpu.stat")
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
