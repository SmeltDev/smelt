package commandutil

import (
	"fmt"
	"strings"
)

// pytestExitNoTests is pytest's exit status when it collected no tests.
const pytestExitNoTests = 5

// WrapPytestNoTests returns a shell command that runs command, treating pytest's
// "no tests collected" exit (5) as a pass when git tracks no test files. A repo
// with tests still fails on 5, since there it means collection broke. Commands
// that do not run pytest are returned untouched.
func WrapPytestNoTests(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 || !runsPytest(fields) {
		return command
	}
	return fmt.Sprintf(`{ %s
}; code=$?; if [ "$code" -eq %d ] && [ -z "$(git ls-files -- '*test_*.py' '*_test.py' 2>/dev/null)" ]; then exit 0; fi; exit $code`, command, pytestExitNoTests)
}

// runsPytest reports whether the command's first words invoke pytest.
func runsPytest(fields []string) bool {
	if fields[0] == "pytest" || fields[0] == "py.test" {
		return true
	}
	return len(fields) >= 3 && strings.HasPrefix(fields[0], "python") && fields[1] == "-m" && fields[2] == "pytest"
}
