package docker

import (
	"context"
	"testing"

	"gotest.tools/v3/assert"
)

func ptr(v float64) *float64 { return &v }

func TestResolveCPUShare(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured *float64
		engine     int
		runs       int
		want       float64
	}{
		{"split among runs", nil, 8, 4, 2},
		{"one run gets the engine", nil, 8, 1, 8},
		{"zero runs count as one", nil, 8, 0, 8},
		{"floor", nil, 8, 8, 2},
		{"floor capped at the engine", nil, 1, 4, 1},
		{"no engine count", nil, 0, 4, 0},
		{"explicit", ptr(1.5), 8, 4, 1.5},
		{"explicit ignores the floor", ptr(1), 8, 8, 1},
		{"zero opts out", ptr(0), 8, 4, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, resolveCPUShare(tc.configured, tc.engine, tc.runs).CPUs, tc.want)
		})
	}
}

func TestCPUShareDescribes(t *testing.T) {
	assert.Equal(t, resolveCPUShare(nil, 8, 4).String(), "cpus 2.0 (8 engine CPUs / 4 runs)")
	assert.Equal(t, resolveCPUShare(ptr(1.5), 8, 4).String(), "cpus 1.5 (factory.cpus)")
	assert.Equal(t, resolveCPUShare(ptr(0), 8, 4).String(), "")
}

func createWithCPUs(t *testing.T, ncpu, runs int, cpus *float64) *fakeDocker {
	t.Helper()
	f := newFakeDocker()
	f.ncpu = ncpu
	p := f.provider()
	p.workRoot = t.TempDir()
	p.CPUs, p.Runs = cpus, runs
	_, err := p.Create(context.Background(), "m", "", "/repo")
	assert.NilError(t, err)
	return f
}

func TestCreateLimitsCPUs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ncpu   int
		runs   int
		cpus   *float64
		nano   int64
		envVal string
	}{
		{"default split", 8, 4, nil, 2e9, "2"},
		{"floor", 8, 8, nil, 2e9, "2"},
		{"configured fraction", 8, 4, ptr(1.5), 1.5e9, "1.5"},
		{"opt out", 8, 4, ptr(0), 0, ""},
		{"engine reports nothing", 0, 4, nil, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := createWithCPUs(t, tc.ncpu, tc.runs, tc.cpus)
			assert.Equal(t, f.hostConfigs[0].NanoCPUs, tc.nano)
			if tc.envVal == "" {
				assert.Equal(t, len(f.envs[0]), 0)
				return
			}
			assert.DeepEqual(t, f.envs[0], []string{envCPUs() + "=" + tc.envVal})
		})
	}
}
