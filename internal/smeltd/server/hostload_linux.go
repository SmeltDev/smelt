package server

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// hostLoad1 reads the 1-minute load average from /proc/loadavg.
func hostLoad1() (float64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty /proc/loadavg")
	}
	return strconv.ParseFloat(fields[0], 64)
}
