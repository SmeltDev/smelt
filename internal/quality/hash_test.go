package quality

import (
	"testing"

	"gotest.tools/v3/assert"
)

func TestFunctionHash(t *testing.T) {
	tests := []struct {
		name     string
		fn       string
		src      string
		start    int
		end      int
		variant  string
		varName  string
		wantSame bool
	}{
		{"go: whitespace", "handle", "func handle(x int) int {\n\treturn x + 1\n}\n", 1, 3,
			"func handle(x int) int {\n        return   x + 1\n\n}\n", "handle", true},
		{"go: rename", "handle", "func handle(x int) int {\n\treturn x + 1\n}\n", 1, 3,
			"func process(x int) int {\n\treturn x + 1\n}\n", "process", true},
		{"go: method rename with receiver name", "Run", "func (s *S) Run() {\n\ts.step()\n}\n", 1, 3,
			"func (s *S) Exec() {\n\ts.step()\n}\n", "Exec", true},
		{"go: body edit", "handle", "func handle(x int) int {\n\treturn x + 1\n}\n", 1, 3,
			"func handle(x int) int {\n\treturn x + 2\n}\n", "handle", false},
		{"python: whitespace", "calc", "def calc(a):\n    return a * 2\n", 1, 2,
			"def calc(a):\n\treturn a  *  2\n", "calc", true},
		{"python: rename", "calc", "def calc(a):\n    return a * 2\n", 1, 2,
			"def compute(a):\n    return a * 2\n", "compute", true},
		{"python: body edit", "calc", "def calc(a):\n    return a * 2\n", 1, 2,
			"def calc(a):\n    return a * 3\n", "calc", false},
		{"typescript: whitespace", "load", "function load(id: string) {\n  return fetch(id);\n}\n", 1, 3,
			"function load(id: string) {\n\n    return fetch(id);\n}\n", "load", true},
		{"typescript: rename", "load", "function load(id: string) {\n  return fetch(id);\n}\n", 1, 3,
			"function get(id: string) {\n  return fetch(id);\n}\n", "get", true},
		{"typescript: body edit", "load", "function load(id: string) {\n  return fetch(id);\n}\n", 1, 3,
			"function load(id: string) {\n  return fetch(id, {});\n}\n", "load", false},
		{"qualified rename", "Cache::get", "int Cache::get(int k) {\n  return m[k];\n}\n", 1, 3,
			"int Cache::fetch(int k) {\n  return m[k];\n}\n", "Cache::fetch", true},
		{"recursive rename", "fact", "def fact(n):\n    return n * fact(n - 1)\n", 1, 2,
			"def f2(n):\n    return n * f2(n - 1)\n", "f2", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := FunctionHash([]byte(tt.src), tt.start, tt.end, tt.fn)
			b := FunctionHash([]byte(tt.variant), tt.start, tt.end+1, tt.varName)
			assert.Equal(t, a == b, tt.wantSame, "%s vs %s", a, b)
		})
	}
}

func TestFunctionHashOnlyLooksAtItsLines(t *testing.T) {
	src := []byte("a\nfunc f() {}\nb\n")
	assert.Equal(t, FunctionHash(src, 2, 2, "f"), FunctionHash([]byte("zzz\nfunc f() {}\nyyy\n"), 2, 2, "f"))
	assert.Equal(t, FunctionHash(src, 9, 12, "f"), FunctionHash(nil, 1, 1, "f"), "out of range is empty")
}
