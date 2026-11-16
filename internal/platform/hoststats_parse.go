package platform

import (
	"regexp"
	"strconv"
	"strings"
)

// parseLoadavg reads sysctl vm.loadavg: "{ 1.92 2.10 2.31 }".
func parseLoadavg(s string) (l1, l5 float64) {
	f := strings.Fields(strings.Trim(strings.TrimSpace(s), "{}"))
	if len(f) >= 2 {
		l1, _ = strconv.ParseFloat(f[0], 64)
		l5, _ = strconv.ParseFloat(f[1], 64)
	}
	return
}

func parseInt(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

var vmPageSize = regexp.MustCompile(`page size of (\d+) bytes`)

// parseVMStat returns bytes available from vm_stat output: free, inactive,
// speculative and purgeable pages, which is what Activity Monitor counts
// as reclaimable.
func parseVMStat(s string) int64 {
	page := int64(4096)
	if m := vmPageSize.FindStringSubmatch(s); m != nil {
		page, _ = strconv.ParseInt(m[1], 10, 64)
	}
	var pages int64
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Pages free", "Pages inactive", "Pages speculative", "Pages purgeable":
			pages += parseInt(strings.TrimSuffix(strings.TrimSpace(v), "."))
		}
	}
	return pages * page
}
