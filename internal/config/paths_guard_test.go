package config_test

import (
	"errors"
	"os"
	"os/exec"
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

const childRealEnv = "SMELT_TEST_GUARD_CHILD_REAL"

// TestRunGuardChild is the body of TestRunFailsATestThatUsesTheRealHome; on its
// own it does nothing.
func TestRunGuardChild(t *testing.T) {
	realDir := os.Getenv(childRealEnv)
	if realDir == "" {
		t.Skip("runs only as a child of TestRunFailsATestThatUsesTheRealHome")
	}
	t.Setenv(config.EnvXDGDataHome, realDir)
	_, err := config.AppData()
	assert.NilError(t, err)
}

// TestRunFailsATestThatUsesTheRealHome re-executes the test binary without the
// isolation marker, so configtest.Run itself must install the guard and turn a
// violation into a failing exit status.
func TestRunFailsATestThatUsesTheRealHome(t *testing.T) {
	realHome := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunGuardChild$")
	env := []string{
		childRealEnv + "=" + filepath.Join(realHome, ".local", "share"),
		"HOME=" + realHome,
		config.EnvXDGDataHome + "=" + filepath.Join(realHome, ".local", "share"),
		config.EnvXDGConfigHome + "=" + filepath.Join(realHome, ".config"),
		config.EnvXDGStateHome + "=" + filepath.Join(realHome, ".local", "state"),
	}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "SMELT_TEST_HOME_ISOLATED=") && !strings.HasPrefix(e, "HOME=") && !strings.HasPrefix(e, "XDG_") {
			env = append(env, e)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	assert.Assert(t, errors.As(err, &exit), "want a non-zero exit, got %v\n%s", err, out)
	assert.Assert(t, strings.Contains(string(out), "TestRunGuardChild"), string(out))
}
