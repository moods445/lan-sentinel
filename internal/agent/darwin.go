//go:build darwin

package agent

import "golang.org/x/sys/unix"

func NewCollector(mounts []string) Collector { return newSystemCollector(mounts) }

func selfCPU() (float64, error) {
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		return 0, err
	}
	return float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6, nil
}
