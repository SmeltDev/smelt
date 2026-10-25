package sandboximage

import (
	"testing"

	"gotest.tools/v3/assert"
)

func TestResolveToolchain(t *testing.T) {
	tests := []struct {
		name   string
		in     ToolchainInputs
		want   ToolchainSource
		wantOK bool
	}{
		{"caller beats all", ToolchainInputs{Caller: "/x/D", Config: "D.cfg", RepoHasDefault: true}, ToolchainSource{"/x/D", ToolchainCaller}, true},
		{"config beats repo", ToolchainInputs{Config: "D.cfg", RepoHasDefault: true}, ToolchainSource{"D.cfg", ToolchainConfig}, true},
		{"repo", ToolchainInputs{RepoHasDefault: true}, ToolchainSource{DefaultDockerfile, ToolchainRepo}, true},
		{"none", ToolchainInputs{}, ToolchainSource{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ResolveToolchain(tt.in)
			assert.Equal(t, ok, tt.wantOK)
			assert.Equal(t, got, tt.want)
		})
	}
}

func TestKitLabel(t *testing.T) {
	assert.Equal(t, KitLabel("5", KitRegistryRepo+"@sha256:abc"), "5@sha256:abc")
	assert.Equal(t, KitLabel("5", "sha256:def"), "5@source:sha256:def")
}
