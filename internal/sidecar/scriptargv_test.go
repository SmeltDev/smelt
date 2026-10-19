package sidecar

import (
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

func TestCheckArgv(t *testing.T) {
	assert.NilError(t, CheckArgv("a", strings.Repeat("x", MaxArgBytes)))
	err := CheckArgv("a", strings.Repeat("x", MaxArgBytes+1))
	assert.Error(t, err, "internal: script passed as an argument (102401 bytes)")
}

func TestScriptArgvKeepsLargeScriptsOutOfArgv(t *testing.T) {
	for _, size := range []int{10, inlineScriptBytes, inlineScriptBytes + 1, 1 << 20} {
		staged := ""
		args, err := scriptArgv(strings.Repeat("x", size), func(s string) (string, error) {
			staged = s
			return "/tmp/staged", nil
		})
		assert.NilError(t, err)
		total := 0
		for _, a := range args {
			total += len(a)
		}
		if size > inlineScriptBytes {
			assert.Equal(t, len(staged), size)
			assert.Assert(t, total < 4096, "size %d: argv is %d bytes", size, total)
			assert.Equal(t, args[len(args)-1], "/tmp/staged")
		} else {
			assert.Equal(t, staged, "")
			assert.Equal(t, args[1], strings.Repeat("x", size))
		}
	}
}
