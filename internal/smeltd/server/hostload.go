package server

import "runtime"

// hostLoadPerCPU is the host's 1-minute load average divided by its CPU count.
func hostLoadPerCPU() (float64, error) {
	load, err := hostLoad1()
	if err != nil {
		return 0, err
	}
	return load / float64(max(runtime.NumCPU(), 1)), nil
}
