package sandboximage

import "strings"

// ToolchainKind says where a run's toolchain Dockerfile came from.
type ToolchainKind string

// The places a toolchain Dockerfile can come from, in precedence order.
const (
	// ToolchainCaller is a Dockerfile the caller named, anywhere on the machine.
	ToolchainCaller ToolchainKind = "caller"
	// ToolchainConfig is factory.dockerfile in the project config.
	ToolchainConfig ToolchainKind = "config"
	// ToolchainRepo is the repository's own DefaultDockerfile.
	ToolchainRepo ToolchainKind = "repo"
)

// ToolchainSource is the Dockerfile a toolchain is built from. Path is absolute
// for ToolchainCaller and repo-relative for the others.
type ToolchainSource struct {
	Path string
	Kind ToolchainKind
}

// ToolchainInputs are what ResolveToolchain chooses between.
type ToolchainInputs struct {
	// Caller is the absolute path a flag or overlay gave, or "".
	Caller string
	// Config is factory.dockerfile, or "".
	Config string
	// RepoHasDefault is set when the repository has a DefaultDockerfile.
	RepoHasDefault bool
}

// ResolveToolchain picks the caller's Dockerfile, then factory.dockerfile, then
// the repository's DefaultDockerfile; ok is false when there is none. It is pure.
func ResolveToolchain(in ToolchainInputs) (src ToolchainSource, ok bool) {
	switch {
	case in.Caller != "":
		return ToolchainSource{Path: in.Caller, Kind: ToolchainCaller}, true
	case in.Config != "":
		return ToolchainSource{Path: in.Config, Kind: ToolchainConfig}, true
	case in.RepoHasDefault:
		return ToolchainSource{Path: DefaultDockerfile, Kind: ToolchainRepo}, true
	}
	return ToolchainSource{}, false
}

// KitLabel is "<version>@<digest>" for a kit image named by digest. A kit built
// from source has an image ID instead of a digest, which the label marks.
func KitLabel(version, ref string) string {
	if _, digest, ok := strings.Cut(ref, "@"); ok {
		return version + "@" + digest
	}
	return version + "@source:" + ref
}

// KitVersionLabel is this smelt's kit version, for KitLabel.
func KitVersionLabel() string { return kitVersionString() }
