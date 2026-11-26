//go:build windows

package agent

import "golang.org/x/sys/windows"

func NewCollector(mounts []string) Collector { return newSystemCollector(mounts) }

func selfCPU() (float64, error) {
	h, err := windows.GetCurrentProcess()
	if err != nil {
		return 0, err
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, err
	}
	return float64(kernel.Nanoseconds()+user.Nanoseconds()) / 1e9, nil
}
