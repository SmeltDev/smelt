package sandboximage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

// Exercise the packaging script without Docker or musl. The fake loader runs
// shell tools, while a native helper and Git's resource directory cover both
// branches of the helper packaging that a version-only check would miss.
func TestKitRuntimePackagesGitResources(t *testing.T) {
	root := t.TempDir()
	write := func(name, contents string) {
		t.Helper()
		path := filepath.Join(root, name)
		assert.NilError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		assert.NilError(t, os.WriteFile(path, []byte(contents), 0o755))
	}
	write("lib/ld-musl-test.so.1", "#!/bin/sh\nshift 2\nexec \"$@\"\n")
	write("usr/lib/libtest.so", "library")
	write("etc/ssl/certs/ca-certificates.crt", "certificate")
	write("usr/share/git-core/templates/description", "template")
	write("helpers/mergetools/meld", "#!/bin/sh\necho merge\n")
	write("helpers/git-remote-https", "\x7fELFhelper")
	write("helpers/git-script", "#!/bin/sh\necho helper\n")
	write("tools/git", "#!/bin/sh\nif [ \"$1\" = --exec-path ]; then echo '"+filepath.Join(root, "helpers")+"'; fi\n")
	for _, tool := range []string{"bash", "pkill", "sha256sum"} {
		write("tools/"+tool, "#!/bin/sh\nexit 0\n")
	}
	write("opt/smelt/lib/ld-musl-test.so.1", "#!/bin/sh\nshift 2\nexec \"$@\"\n")
	assert.NilError(t, os.MkdirAll(filepath.Join(root, "opt/smelt/bin"), 0o755))
	raw, err := os.ReadFile(filepath.Join("..", "..", "kit", "runtime.sh"))
	assert.NilError(t, err)
	// Redirect build-time paths into the fixture; the shipped script still runs
	// unchanged apart from its filesystem root.
	script := strings.NewReplacer(
		"/opt/smelt", root+"/opt/smelt",
		"/lib/*.so*", root+"/lib/*.so*",
		"/lib/ld-musl-", root+"/lib/ld-musl-",
		"/usr/lib/", root+"/usr/lib/",
		"/etc/ssl/", root+"/etc/ssl/",
		"/usr/share/", root+"/usr/share/",
	).Replace(string(raw))
	write("runtime.sh", script)
	cmd := exec.CommandContext(t.Context(), "sh", filepath.Join(root, "runtime.sh"))
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "tools")+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	assert.NilError(t, err, "%s", out)
	runtime := filepath.Join(root, "opt/smelt/libexec/runtime")
	for path, want := range map[string]string{
		"git-core/mergetools/meld":       "#!/bin/sh\necho merge\n",
		"git-core/git-script":            "#!/bin/sh\necho helper\n",
		"git-core-real/git-remote-https": "\x7fELFhelper",
		"share/templates/description":    "template",
	} {
		got, err := os.ReadFile(filepath.Join(runtime, path))
		assert.NilError(t, err)
		assert.Equal(t, string(got), want)
	}
	wrapper, err := os.ReadFile(filepath.Join(runtime, "git-core/git-remote-https"))
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(string(wrapper), "--library-path"))
}
