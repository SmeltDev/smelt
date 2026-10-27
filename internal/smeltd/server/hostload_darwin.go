package server

import (
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix"
)

// hostLoad1 reads the 1-minute load average from the vm.loadavg sysctl, a
// struct loadavg: three uint32 fixed-point values, padding, then an int64 scale.
func hostLoad1() (float64, error) {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil {
		return 0, err
	}
	if len(raw) < 24 {
		return 0, fmt.Errorf("vm.loadavg is %d bytes", len(raw))
	}
	scale := binary.NativeEndian.Uint64(raw[16:24])
	if scale == 0 {
		return 0, fmt.Errorf("vm.loadavg has a zero scale")
	}
	return float64(binary.NativeEndian.Uint32(raw[0:4])) / float64(scale), nil
}
