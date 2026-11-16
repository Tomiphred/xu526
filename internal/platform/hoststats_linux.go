package platform

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// hostStats reads load averages and memory (MB) from /proc.
func hostStats() (load1, load5 float64, totalMB, availMB int64) {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) >= 2 {
			load1, _ = strconv.ParseFloat(f[0], 64)
			load5, _ = strconv.ParseFloat(f[1], 64)
		}
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		defer f.Close()
		s := bufio.NewScanner(f)
		for s.Scan() {
			fields := strings.Fields(s.Text())
			if len(fields) < 2 {
				continue
			}
			kb, _ := strconv.ParseInt(fields[1], 10, 64)
			switch fields[0] {
			case "MemTotal:":
				totalMB = kb / 1024
			case "MemAvailable:":
				availMB = kb / 1024
			}
		}
	}
	return
}
