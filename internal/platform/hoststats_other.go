//go:build !linux && !darwin && !windows

package platform

func hostStats() (load1, load5 float64, totalMB, availMB int64) { return }
