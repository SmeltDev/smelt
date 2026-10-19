package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/sandboximage"
)

func cfgWith(t *testing.T, body string) *config.ProjectConfig {
	t.Helper()
	var cfg config.ProjectConfig
	assert.NilError(t, json.Unmarshal([]byte(body), &cfg))
	return &cfg
}

func TestResolveBuildSettingsBuildsOnlyDockerfileImages(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	root := t.TempDir()
	streams := discardStreams()

	// A user-managed image passes through untouched, with no Docker involved.
	cfg := cfgWith(t, `{"factory":{"backend":"docker","image":"smelt-sandbox:local","dockerfile":"Dockerfile.smelt"}}`)
	got, err := resolveBuildSettings(context.Background(), streams, root, factorySettings{Attempts: 3}, factoryExplicit{}, cfg)
	assert.NilError(t, err)
	assert.Equal(t, got.Image, "smelt-sandbox:local")
	assert.Equal(t, got.ImageSource, imageFromConfig)

	// A managed one is built, which needs its Dockerfile.
	cfg = cfgWith(t, `{"factory":{"backend":"docker","dockerfile":"Dockerfile.smelt"}}`)
	_, err = resolveBuildSettings(context.Background(), streams, root, factorySettings{Attempts: 3}, factoryExplicit{}, cfg)
	assert.Equal(t, userMsg(err), "Could not read Dockerfile.smelt.")

	// With the Dockerfile present it gets as far as Docker, which is unreachable here.
	assert.NilError(t, os.WriteFile(filepath.Join(root, "Dockerfile.smelt"), []byte("FROM x\n"), 0o644))
	// The wait for Docker is cut to a second so the test does not sit out the
	// default three minutes.
	begin := time.Now()
	_, err = resolveBuildSettings(context.Background(), streams, root, factorySettings{Attempts: 3, ReadyTimeout: time.Second}, factoryExplicit{}, cfg)
	assert.ErrorContains(t, err, "did not finish starting")
	assert.Assert(t, time.Since(begin) < 10*time.Second, "took %s", time.Since(begin))
}

func TestPrepareDockerfileKeepsAnExistingFile(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	root := t.TempDir()
	path := filepath.Join(root, "Dockerfile.smelt")
	assert.NilError(t, os.WriteFile(path, []byte("FROM mine\n"), 0o644))
	assert.NilError(t, prepareDockerfile(context.Background(), discardStreams(), root, "Dockerfile.smelt", cfgWith(t, `{}`), false))
	got, err := os.ReadFile(path)
	assert.NilError(t, err)
	assert.Equal(t, string(got), "FROM mine\n")
}

func TestPrepareDockerfileRegenerateNeedsDocker(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	root := t.TempDir()
	path := filepath.Join(root, "Dockerfile.smelt")
	assert.NilError(t, os.WriteFile(path, []byte("FROM mine\n"), 0o644))
	err := prepareDockerfile(context.Background(), discardStreams(), root, "Dockerfile.smelt", cfgWith(t, `{}`), true)
	assert.Assert(t, err != nil)
	got, readErr := os.ReadFile(path)
	assert.NilError(t, readErr)
	assert.Equal(t, string(got), "FROM mine\n") // never overwritten on failure
}

func TestResolveBuildSettingsWarnsAboutAStaleUserImage(t *testing.T) {
	for name, tt := range map[string]struct {
		info sandboximage.ImageInfo
		err  error
		cfg  string
		want string
	}{
		"label missing": {sandboximage.ImageInfo{Status: sandboximage.ImageUnlabeled, Kit: 1},
			nil, `{"factory":{"backend":"docker","image":"smelt-sandbox:local"}}`,
			"smelt-sandbox:local has agent kit none; this smelt expects 5: rebuild it (docker build -t smelt-sandbox:local -f Dockerfile.smelt .)"},
		"lower": {sandboximage.ImageInfo{Status: sandboximage.ImageLabeled, Kit: 3, KitLabeled: true},
			nil, `{"factory":{"backend":"docker","image":"smelt-sandbox:local"}}`,
			"smelt-sandbox:local has agent kit 3; this smelt expects 5: rebuild it (docker build -t smelt-sandbox:local -f Dockerfile.smelt .)"},
		"equal":   {sandboximage.ImageInfo{Status: sandboximage.ImageLabeled, Kit: sandboximage.KitVersion, KitLabeled: true}, nil, `{"factory":{"backend":"docker","image":"smelt-sandbox:local"}}`, ""},
		"missing": {sandboximage.ImageInfo{Status: sandboximage.ImageMissing}, nil, `{"factory":{"backend":"docker","image":"smelt-sandbox:local"}}`, ""},
		"error":   {sandboximage.ImageInfo{}, errors.New("no daemon"), `{"factory":{"backend":"docker","image":"smelt-sandbox:local"}}`, ""},
		"custom dockerfile hint": {sandboximage.ImageInfo{Status: sandboximage.ImageUnlabeled, Kit: 1},
			nil, `{"factory":{"backend":"docker","image":"x:1","dockerfile":"ci/Dockerfile.dev"}}`,
			"x:1 has agent kit none; this smelt expects 5: rebuild it (docker build -t x:1 -f ci/Dockerfile.dev .)"},
		"not docker": {sandboximage.ImageInfo{Status: sandboximage.ImageUnlabeled, Kit: 1}, nil, `{"factory":{"backend":"circleci","image":"x:1"}}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			old := inspectUserImage
			t.Cleanup(func() { inspectUserImage = old })
			inspectUserImage = func(context.Context, string) (sandboximage.ImageInfo, error) { return tt.info, tt.err }
			got, err := resolveBuildSettings(context.Background(), discardStreams(), t.TempDir(), factorySettings{Attempts: 3}, factoryExplicit{}, cfgWith(t, tt.cfg))
			assert.NilError(t, err)
			assert.Equal(t, got.KitWarning, tt.want)
		})
	}
}
