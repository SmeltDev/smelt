// Package configtest keeps a package's tests away from the user's real smelt
// data. A package whose tests reach config.AppData, Dir or AppState calls Main
// (or Run) from its TestMain.
package configtest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/smeltdev/smelt/internal/config"
)

// isolatedEnv marks a process whose environment Run has already isolated; the
// processes it starts inherit it.
const isolatedEnv = "SMELT_TEST_HOME_ISOLATED"

// Guard records the smelt directories a test run resolves inside a real home.
type Guard struct {
	real []string

	mu         sync.Mutex
	violations []string
}

// NewGuard returns a Guard that flags directories at or under any of realDirs.
func NewGuard(realDirs ...string) *Guard {
	g := &Guard{}
	for _, d := range realDirs {
		if d != "" {
			g.real = append(g.real, resolve(d))
		}
	}
	return g
}

// Check records a violation naming the calling test when dir is inside a real
// directory.
func (g *Guard) Check(dir string) {
	if !g.inside(dir) {
		return
	}
	g.record(fmt.Sprintf("%s resolved %s, inside the real home; set XDG_DATA_HOME, XDG_CONFIG_HOME and XDG_STATE_HOME to t.TempDir()", callingTest(), dir))
}

func (g *Guard) inside(dir string) bool {
	dir = resolve(dir)
	for _, r := range g.real {
		if dir == r || strings.HasPrefix(dir, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (g *Guard) record(v string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.violations = append(g.violations, v)
}

// Violations returns what Check has recorded.
func (g *Guard) Violations() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.violations...)
}

// resolve follows symlinks as far as the path exists, so /var and /private/var
// compare equal.
func resolve(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; cur = filepath.Dir(cur) {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		if filepath.Dir(cur) == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
	}
}

var testFunc = regexp.MustCompile(`\.(Test[A-Z_0-9]\w*)`)

// callingTest names the test on the call stack, or says there is none.
func callingTest() string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if m := testFunc.FindStringSubmatch(f.Function); m != nil {
			return m[1]
		}
		if !more {
			return "a test (name unknown)"
		}
	}
}

// realDirs lists the data, config and state homes of the current environment.
func realDirs() []string {
	var dirs []string
	for _, env := range []string{config.EnvXDGDataHome, config.EnvXDGConfigHome, config.EnvXDGStateHome} {
		if v := os.Getenv(env); v != "" {
			dirs = append(dirs, v)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs,
			filepath.Join(home, ".local", "share"),
			filepath.Join(home, ".config"),
			filepath.Join(home, ".local", "state"))
	}
	return dirs
}

// keepGoCaches pins the Go build and module caches to where they are now, so
// a test that runs "go build" does not start cold under the swapped HOME.
func keepGoCaches() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	if os.Getenv("GOPATH") == "" {
		_ = os.Setenv("GOPATH", filepath.Join(home, "go"))
	}
	if cache, err := os.UserCacheDir(); err == nil && os.Getenv("GOCACHE") == "" {
		_ = os.Setenv("GOCACHE", filepath.Join(cache, "go-build"))
	}
}

// Main runs a package's tests with HOME and the XDG homes in a temp dir and
// exits with their status.
func Main(m *testing.M) { os.Exit(Run(m)) }

// Run is Main for a TestMain that has its own setup or exit handling. It
// returns the exit code: non-zero when a test resolved a real smelt directory.
func Run(m *testing.M) int {
	// A test binary re-executed by a test inherits the environment that test
	// set up; swapping it again would cut the child off from the parent's dirs.
	if os.Getenv(isolatedEnv) != "" {
		return m.Run()
	}
	_ = os.Setenv(isolatedEnv, "1")
	guard := NewGuard(realDirs()...)
	keepGoCaches()
	tmp, err := os.MkdirTemp("", "smelt-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "configtest:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	for env, dir := range map[string]string{
		"HOME":                  tmp,
		config.EnvXDGDataHome:   filepath.Join(tmp, "data"),
		config.EnvXDGConfigHome: filepath.Join(tmp, "config"),
		config.EnvXDGStateHome:  filepath.Join(tmp, "state"),
	} {
		if err := os.Setenv(env, dir); err != nil {
			fmt.Fprintln(os.Stderr, "configtest:", err)
			return 1
		}
	}
	config.SetPathGuard(guard.Check)
	code := m.Run()
	config.SetPathGuard(nil)
	if v := guard.Violations(); len(v) > 0 {
		fmt.Fprintln(os.Stderr, "FAIL: tests used the real smelt data:\n  "+strings.Join(v, "\n  "))
		return 1
	}
	return code
}
