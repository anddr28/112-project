//go:build unix

package metrics

import (
	"bytes"
	"os"
	"strconv"
	"syscall"
)

// cpuSeconds — user+system время процесса (getrusage работает и в Linux, и в macOS).
func cpuSeconds() (float64, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	tv := func(t syscall.Timeval) float64 { return float64(t.Sec) + float64(t.Usec)/1e6 }
	return tv(ru.Utime) + tv(ru.Stime), true
}

var pageSize = float64(os.Getpagesize())

// residentBytes — текущий RSS из /proc/self/statm (Linux; в macOS /proc нет — метрики не будет).
func residentBytes() (float64, bool) {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	f := bytes.Fields(b)
	if len(f) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(string(f[1]), 10, 64)
	if err != nil {
		return 0, false
	}
	return float64(pages) * pageSize, true
}
