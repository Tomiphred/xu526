package platform

import (
	"context"
	"os/exec"
	"time"
)

// hostStats asks sysctl and vm_stat (both part of macOS) for load and memory.
func hostStats() (load1, load5 float64, totalMB, availMB int64) {
	out := func(name string, args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		b, _ := exec.CommandContext(ctx, name, args...).Output()
		return string(b)
	}
	load1, load5 = parseLoadavg(out("/usr/sbin/sysctl", "-n", "vm.loadavg"))
	totalMB = parseInt(out("/usr/sbin/sysctl", "-n", "hw.memsize")) >> 20
	availMB = parseVMStat(out("/usr/bin/vm_stat")) >> 20
	return
}
