//go:build !linux && !darwin

package server

import "errors"

// hostLoad1 has no source on this platform.
func hostLoad1() (float64, error) {
	return 0, errors.New("load average unavailable on this host")
}
