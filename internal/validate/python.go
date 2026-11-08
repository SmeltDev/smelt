package validate

import (
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/gitexec"
)

// NoTestGateNote is what init says when a Python repo gets no test gate.
const NoTestGateNote = "no test gate: no tests found (add tests, then run smelt init again)"

// pytestName matches a requirement line or dependency name that is pytest.
var pytestName = regexp.MustCompile(`(?i)^\s*pytest($|[^A-Za-z0-9_.-])`)

// detectPython returns the pytest gate for a Python repo, or nothing with the
// no-test-gate note when the gate could not pass: no tracked tests, or pytest
// not declared anywhere it could be installed from.
func detectPython(workDir string) ([]config.Command, string) {
	if !hasPythonTests(workDir) || !pytestDeclared(workDir) {
		return nil, NoTestGateNote
	}
	return []config.Command{
		{Name: roleTest, Run: "pytest", Role: config.RoleGate, Timeout: config.DefaultTestTimeout},
	}, ""
}

// isPythonTestFile reports whether a repo-relative path names a pytest test file.
func isPythonTestFile(p string) bool {
	base := path.Base(p)
	return strings.HasSuffix(base, ".py") && (strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py"))
}

// hasPythonTests reports whether the repo has test files. Tracked files count;
// outside a git repo the tree is walked instead.
func hasPythonTests(workDir string) bool {
	out, err := (gitexec.Runner{Dir: workDir}).Output(context.Background(), "ls-files", "-z")
	if err != nil {
		return walkHasPythonTests(workDir)
	}
	for _, f := range strings.Split(string(out), "\x00") {
		if isPythonTestFile(f) {
			return true
		}
	}
	return false
}

// walkHasPythonTests is the fallback outside git, skipping hidden directories,
// virtualenvs and node_modules.
func walkHasPythonTests(workDir string) bool {
	found := false
	_ = filepath.WalkDir(workDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipDir
		}
		name := d.Name()
		if d.IsDir() {
			if p != workDir && (strings.HasPrefix(name, ".") || name == "venv" || name == "node_modules" || name == "__pycache__") {
				return filepath.SkipDir
			}
			return nil
		}
		found = isPythonTestFile(name)
		return nil
	})
	return found
}

// pytestDeclared reports whether pytest is something the project installs:
// a dependency, an optional or dev group, or a requirements/Pipfile/setup entry.
func pytestDeclared(workDir string) bool {
	if pyprojectDeclaresPytest(filepath.Join(workDir, "pyproject.toml")) {
		return true
	}
	for _, name := range []string{"requirements.txt", "setup.py", "setup.cfg", "Pipfile"} {
		if fileMentionsPytest(filepath.Join(workDir, name)) {
			return true
		}
	}
	matches, _ := filepath.Glob(filepath.Join(workDir, "requirements*.txt"))
	for _, m := range matches {
		if fileMentionsPytest(m) {
			return true
		}
	}
	return false
}

// fileMentionsPytest reports whether any line of the file starts with pytest,
// ignoring the quotes and commas of a setup.py list.
func fileMentionsPytest(p string) bool {
	data, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if pytestName.MatchString(strings.Trim(strings.TrimSpace(line), `"',`)) {
			return true
		}
	}
	return false
}

// pyprojectDeclaresPytest reads the dependency tables of a pyproject.toml.
func pyprojectDeclaresPytest(p string) bool {
	var doc map[string]any
	if _, err := toml.DecodeFile(p, &doc); err != nil {
		return false
	}
	return listHasPytest(lookup(doc, "project", "dependencies")) ||
		groupsHavePytest(lookup(doc, "project", "optional-dependencies")) ||
		groupsHavePytest(lookup(doc, "dependency-groups")) ||
		keysHavePytest(lookup(doc, "tool", "poetry", "dependencies")) ||
		keysHavePytest(lookup(doc, "tool", "poetry", "dev-dependencies")) ||
		poetryGroupsHavePytest(lookup(doc, "tool", "poetry", "group"))
}

func lookup(doc map[string]any, keys ...string) any {
	var cur any = doc
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func listHasPytest(v any) bool {
	list, _ := v.([]any)
	for _, item := range list {
		if s, ok := item.(string); ok && pytestName.MatchString(s) {
			return true
		}
	}
	return false
}

// groupsHavePytest checks a table of named requirement lists.
func groupsHavePytest(v any) bool {
	groups, _ := v.(map[string]any)
	for _, g := range groups {
		if listHasPytest(g) {
			return true
		}
	}
	return false
}

func keysHavePytest(v any) bool {
	m, _ := v.(map[string]any)
	for k := range m {
		if pytestName.MatchString(k) {
			return true
		}
	}
	return false
}

func poetryGroupsHavePytest(v any) bool {
	groups, _ := v.(map[string]any)
	for _, g := range groups {
		if m, ok := g.(map[string]any); ok && keysHavePytest(m["dependencies"]) {
			return true
		}
	}
	return false
}
