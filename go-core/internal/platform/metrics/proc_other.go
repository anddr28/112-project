//go:build !unix

package metrics

func cpuSeconds() (float64, bool)    { return 0, false }
func residentBytes() (float64, bool) { return 0, false }
