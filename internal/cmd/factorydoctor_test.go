package cmd

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/smeltdev/smelt/internal/sidecar/docker"
	"gotest.tools/v3/assert"
)

func TestPrintStorageReport(t *testing.T) {
	var out bytes.Buffer
	printStorageReport(&out, docker.StorageReport{
		Host:   docker.HostStorage{Endpoint: "unix:///socket", DaemonID: "id", DockerRoot: "/var/lib/docker", Settings: "/settings-store.json", BackingImage: "/external/Docker.raw", FilesystemPath: "/external"},
		Caches: []docker.CacheUsage{{Name: "smelt-cache-project-1001", Bytes: 2e9, Cap: 10e9, MeasuredAt: "2026-10-25T00:00:00Z"}, {Name: "smelt-gomod-project", Cap: 10e9, Unavailable: "not measured"}},
	}, nil)
	for _, want := range []string{"Backing image: /external/Docker.raw", "Backing filesystem: /external", "2.00 GB / 10.00 GB cap (measured", "unavailable / 10.00 GB cap (not measured)"} {
		assert.Assert(t, strings.Contains(out.String(), want), out.String())
	}
	out.Reset()
	printStorageReport(&out, docker.StorageReport{Host: docker.HostStorage{Endpoint: "ssh://remote"}}, fmt.Errorf("backing-storage discovery failed"))
	assert.Assert(t, strings.Contains(out.String(), "Storage unavailable: backing-storage discovery failed"))
	assert.Equal(t, newFactoryDoctorCmd().Use, "doctor")
}
