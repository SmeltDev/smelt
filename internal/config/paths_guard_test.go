package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/config/configtest"
)

func TestMain(m *testing.M) { configtest.Main(m) }

func TestGuardFlagsADataDirInsideTheRealHomeAndNamesTheTest(t *testing.T) {
	realHome := t.TempDir()
	guard := configtest.NewGuard(filepath.Join(realHome, ".local", "share"))
	config.SetPathGuard(guard.Check)
	t.Cleanup(func() { config.SetPathGuard(nil) })

	t.Setenv(config.EnvXDGDataHome, filepath.Join(realHome, ".local", "share"))
	_, err := config.AppData()
	assert.NilError(t, err)
	v := guard.Violations()
	assert.Equal(t, len(v), 1)
	assert.Assert(t, strings.Contains(v[0], "TestGuardFlagsADataDirInsideTheRealHomeAndNamesTheTest"), v[0])

	t.Setenv(config.EnvXDGDataHome, t.TempDir())
	_, err = config.AppData()
	assert.NilError(t, err)
	assert.Equal(t, len(guard.Violations()), 1, "a temp data home is fine")
}

func TestGuardFollowsSymlinks(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	assert.NilError(t, os.MkdirAll(realDir, 0o755))
	link := filepath.Join(base, "link")
	assert.NilError(t, os.Symlink(realDir, link))
	guard := configtest.NewGuard(realDir)
	guard.Check(filepath.Join(link, "smelt"))
	assert.Equal(t, len(guard.Violations()), 1)
}
