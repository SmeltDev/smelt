package version

import (
	"runtime/debug"
	"testing"

	"gotest.tools/v3/assert"
)

func fakeBuild(settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) { return &debug.BuildInfo{Settings: settings}, true }
}

func TestDescribe(t *testing.T) {
	full := fakeBuild(
		debug.BuildSetting{Key: "vcs.revision", Value: "dabe7281234567890"},
		debug.BuildSetting{Key: "vcs.time", Value: "2026-10-18T09:30:00Z"},
		debug.BuildSetting{Key: "vcs.modified", Value: "false"},
	)
	dirty := fakeBuild(
		debug.BuildSetting{Key: "vcs.revision", Value: "dabe7281234567890"},
		debug.BuildSetting{Key: "vcs.time", Value: "2026-10-18T09:30:00Z"},
		debug.BuildSetting{Key: "vcs.modified", Value: "true"},
	)
	noDate := fakeBuild(debug.BuildSetting{Key: "vcs.revision", Value: "dabe728"})
	none := fakeBuild(debug.BuildSetting{Key: "GOOS", Value: "linux"})
	unavailable := func() (*debug.BuildInfo, bool) { return nil, false }

	tests := []struct {
		name string
		read func() (*debug.BuildInfo, bool)
		want string
	}{
		{"revision and date", full, "dev (dabe728, 2026-10-18)"},
		{"dirty", dirty, "dev (dabe728-dirty, 2026-10-18)"},
		{"no date", noDate, "dev (dabe728)"},
		{"no vcs info", none, "dev"},
		{"no build info", unavailable, "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, Describe("dev", ReadBuild(tt.read)), tt.want)
		})
	}
}

func TestReadBuildShortensRevision(t *testing.T) {
	b := ReadBuild(fakeBuild(debug.BuildSetting{Key: "vcs.revision", Value: "0123456789abcdef"}))
	assert.Equal(t, b.Revision, "0123456")
	assert.Equal(t, b.Modified, false)
}
