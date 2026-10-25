package cmd

import (
	"os"
	"time"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/sandboximage"
	"github.com/smeltdev/smelt/internal/sidecar/docker"
	"github.com/smeltdev/smelt/internal/smeltd"
)

// defaultFactoryAttempts is how many rounds a factory run checks when neither
// --max-attempts nor factory.maxAttempts says.
const defaultFactoryAttempts = 10

// factorySettings are the `smelt factory build` settings that can come from
// either a flag or the project config.
type factorySettings struct {
	Backend  string
	Image    string
	Attempts int
	Log      bool
	// SandboxDockerfile is --sandbox-dockerfile: an absolute path to a toolchain
	// Dockerfile anywhere on the machine. It is an input only.
	SandboxDockerfile string
	// RepoDockerfile is set by the caller when the repository has its own
	// Dockerfile.smelt, which the docker backend then builds when nothing else
	// names an image. It is an input only.
	RepoDockerfile bool
	// Dockerfile is the Dockerfile to build Image from, set only when
	// ImageSource is imageFromDockerfile: absolute for ToolchainCaller,
	// repo-relative otherwise.
	Dockerfile  string
	Toolchain   sandboximage.ToolchainKind
	ImageSource imageSource
	// Sandbox is where the built image's toolchain came from, set once the
	// image is built.
	Sandbox *smeltd.FactorySandbox
	// KitWarning is the outdated-agent-kit warning for Dockerfile, "" when its
	// kit is current. The run records it.
	KitWarning string
	// ReadyTimeout bounds the wait for Docker to start; zero is the default.
	// Only tests set it.
	ReadyTimeout time.Duration
}

// dockerImage is the image the run's containers use, or "" for any other
// backend.
func (s factorySettings) dockerImage() string {
	if s.Backend != config.BackendDocker {
		return ""
	}
	return s.Image
}

// imageSource says where the resolved image came from, which decides whether
// smelt may build it.
type imageSource int

const (
	imageUnset          imageSource = iota
	imageFromFlag                   // --image
	imageFromConfig                 // factory.image: user-managed, never auto-built
	imageFromDockerfile             // a toolchain Dockerfile (flag, factory.dockerfile or the repo's): managed, built when missing
	imageFromEnv                    // SMELT_DOCKER_IMAGE, read by the provider
)

// factoryExplicit says which factorySettings were set by a flag, as opposed to
// holding the flag's default.
type factoryExplicit struct {
	Backend, Image, Attempts, Log bool
}

// resolveFactorySettings applies the precedence: an explicit flag, then
// factory.* in the project config, then the built-in default. The Docker
// backend never takes validation.sidecarImage, which is a CircleCI snapshot ID.
func resolveFactorySettings(flags factorySettings, set factoryExplicit, cfg *config.ProjectConfig) (factorySettings, error) {
	var fc config.FactoryConfig
	if cfg != nil && cfg.Factory != nil {
		fc = *cfg.Factory
	}
	out := flags

	if !set.Backend {
		out.Backend = fc.Backend
	}
	out.Backend = config.BackendOrDefault(out.Backend)

	if !set.Attempts {
		out.Attempts = defaultFactoryAttempts
		if fc.MaxAttempts > 0 {
			out.Attempts = fc.MaxAttempts
		}
	}

	if !set.Log {
		out.Log = fc.Log
	}

	if err := checkSandboxFlag(flags, set, out.Backend); err != nil {
		return out, err
	}
	selectFactoryImage(&out, flags, set, fc, cfg)

	if out.Backend == config.BackendDocker && out.ImageSource == imageUnset {
		return out, newUserError("No Docker image is set for factory runs.").
			withCode("command.invalid_args").
			withSuggestion("Pass --image, or run smelt init to write a Dockerfile.smelt (or set factory.image in .smelt/config.json).").
			withExitCode(ExitBadArgs).
			withoutDetail()
	}
	return out, nil
}

// selectFactoryImage sets out's image, toolchain Dockerfile and where they came
// from, for out's backend.
func selectFactoryImage(out *factorySettings, flags factorySettings, set factoryExplicit, fc config.FactoryConfig, cfg *config.ProjectConfig) {
	out.Image, out.Dockerfile, out.Toolchain, out.ImageSource = "", "", "", imageUnset
	switch {
	case set.Image && flags.Image != "":
		out.Image, out.ImageSource = flags.Image, imageFromFlag
	case out.Backend == config.BackendDocker:
		out.Image, out.Dockerfile, out.Toolchain, out.ImageSource = dockerImageFromConfig(fc, sandboximage.ToolchainInputs{
			Caller:         flags.SandboxDockerfile,
			Config:         fc.Dockerfile,
			RepoHasDefault: flags.RepoDockerfile && os.Getenv(docker.EnvImage) == "",
		})
	case cfg != nil && cfg.Validation != nil && cfg.Validation.SidecarImage != "":
		out.Image, out.ImageSource = cfg.Validation.SidecarImage, imageFromConfig
	}
	if out.Backend == config.BackendDocker && out.ImageSource == imageUnset && os.Getenv(docker.EnvImage) != "" {
		out.ImageSource = imageFromEnv
	}
}

// checkSandboxFlag refuses --sandbox-dockerfile where it cannot apply: with
// --image, which also names the image, and on a backend that builds no Docker
// image.
func checkSandboxFlag(flags factorySettings, set factoryExplicit, backend string) error {
	if flags.SandboxDockerfile == "" {
		return nil
	}
	if set.Image && flags.Image != "" {
		return newUserError("--sandbox-dockerfile and --image cannot be used together.").
			withCode("command.invalid_args").
			withSuggestion("Both name the image: drop --image to build the toolchain from the Dockerfile.").
			withExitCode(ExitBadArgs).
			withoutDetail()
	}
	if backend != config.BackendDocker {
		return newUserError("--sandbox-dockerfile needs the docker backend.").
			withCode("command.invalid_args").
			withSuggestion("Use --backend docker, or drop --sandbox-dockerfile.").
			withExitCode(ExitBadArgs).
			withoutDetail()
	}
	return nil
}

// dockerImageFromConfig picks the docker image: a toolchain Dockerfile the
// caller named, else an explicit factory.image, else factory.dockerfile, else
// the repository's own Dockerfile.smelt.
func dockerImageFromConfig(fc config.FactoryConfig, in sandboximage.ToolchainInputs) (img, dockerfile string, kind sandboximage.ToolchainKind, src imageSource) {
	if fc.Image != "" && in.Caller == "" {
		return fc.Image, "", "", imageFromConfig
	}
	if t, ok := sandboximage.ResolveToolchain(in); ok {
		return "", t.Path, t.Kind, imageFromDockerfile
	}
	return "", "", "", imageUnset
}
