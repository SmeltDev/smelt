package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/smeltdev/smelt/internal/sandboximage"
)

// commitStepWidth is the column a wrapped `git add` line breaks at.
const commitStepWidth = 80

// initCandidates are the project files init can write, relative to workDir:
// the ones every project gets, the Codex hooks, the CircleCI scaffold, and one
// directory per agent for each skill init can install. A file init did not
// write is filtered out by comparing snapshots. Files init only offers as
// examples, and the git hook, are not here: they are not meant to be committed.
func initCandidates() []string {
	skillNames := append(append([]string(nil), circleCIInitSkills...), genericInitSkills...)
	paths := make([]string, 0, 6+2*len(skillNames))
	paths = append(paths,
		filepath.Join(".smelt", "config.json"),
		sandboximage.DefaultDockerfile,
		".gitignore",
		filepath.Join(".claude", "settings.json"),
		filepath.Join(".codex", "hooks.json"),
		filepath.Join(".circleci", "test-suites.yml"))
	for _, name := range skillNames {
		paths = append(paths,
			filepath.Join(".claude", "skills", name),
			filepath.Join(".agents", "skills", name))
	}
	return paths
}

// snapshotInitFiles fingerprints each path under workDir, a file or a whole
// directory. A missing path gets "". Comparing two snapshots tells which files
// a step wrote or changed, whichever of the steps did it.
func snapshotInitFiles(workDir string, paths []string) map[string]string {
	snap := make(map[string]string, len(paths))
	for _, p := range paths {
		snap[p] = fingerprint(os.DirFS(workDir), filepath.ToSlash(p))
	}
	return snap
}

// fingerprint hashes the names and contents under path; "" when nothing exists.
func fingerprint(fsys fs.FS, path string) string {
	h := sha256.New()
	found := false
	// WalkDir visits in lexical order, so the digest is stable.
	_ = fs.WalkDir(fsys, path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil
		}
		found = true
		h.Write([]byte(p + "\x00"))
		h.Write(data)
		return nil
	})
	if !found {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// changedInitFiles are the paths, in paths' order, that exist after and differ
// from before: written or changed by init, and so worth committing.
func changedInitFiles(before, after map[string]string, paths []string) []string {
	var changed []string
	for _, p := range paths {
		if after[p] != "" && after[p] != before[p] {
			changed = append(changed, p)
		}
	}
	return changed
}

// formatCommitStep renders the closing block that tells the developer what to
// commit so runs start from it. "" when init wrote nothing.
func formatCommitStep(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	const indent = "  "
	const cont = indent + "        " // lines up under the first path after "git add "
	var b strings.Builder
	b.WriteString("Commit these so runs start from them:\n")
	line := indent + "git add"
	for _, p := range paths {
		if len(line)+1+len(p) > commitStepWidth && line != indent+"git add" && line != cont {
			b.WriteString(line + " \\\n")
			line = cont
		}
		if line == cont {
			line += p
		} else {
			line += " " + p
		}
	}
	b.WriteString(line + "\n")
	b.WriteString(indent + "git commit -m \"Set up smelt\"\n")
	return b.String()
}
