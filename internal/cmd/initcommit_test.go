package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/factory"
	"github.com/smeltdev/smelt/internal/sandboximage"
	"github.com/smeltdev/smelt/internal/skills"
	"github.com/smeltdev/smelt/internal/testing/gitrepo"
)

func TestFormatCommitStep(t *testing.T) {
	assert.Equal(t, formatCommitStep(nil), "")

	assert.Equal(t, formatCommitStep([]string{".smelt/config.json", "Dockerfile.smelt"}),
		"Commit these so runs start from them:\n"+
			"  git add .smelt/config.json Dockerfile.smelt\n"+
			"  git commit -m \"Set up smelt\"\n")

	all := []string{
		".smelt/config.json", "Dockerfile.smelt", ".gitignore", ".claude/settings.json", ".codex/hooks.json",
		".claude/skills/smelt-validate-config", ".agents/skills/smelt-validate-config",
	}
	assert.Equal(t, formatCommitStep(all),
		"Commit these so runs start from them:\n"+
			"  git add .smelt/config.json Dockerfile.smelt .gitignore .claude/settings.json \\\n"+
			"          .codex/hooks.json .claude/skills/smelt-validate-config \\\n"+
			"          .agents/skills/smelt-validate-config\n"+
			"  git commit -m \"Set up smelt\"\n")
}

// runInitWrites does what init writes into the project, step by step.
func runInitWrites(t *testing.T, dir string, circleCI bool) (printed string) {
	t.Helper()
	streams, _, errOut := testStreams()
	candidates := initCandidates()
	before := snapshotInitFiles(dir, candidates)

	commands := []config.Command{{Name: "test", Run: "go test ./...", Timeout: 60}}
	assert.NilError(t, writeProjectConfig(dir, &config.ProjectConfig{Commands: commands}, streams))
	assert.NilError(t, os.WriteFile(filepath.Join(dir, sandboximage.DefaultDockerfile), []byte("FROM scratch\n"), 0o644))
	assert.NilError(t, ensureGitignoreEntries(dir, streams))
	assert.NilError(t, writeSettings(dir, commands, streams, fakeConfirmYes))
	assert.NilError(t, writeCodexHooks(dir, commands, streams, fakeConfirmYes))
	installSkillsStep(dir, streams, planInit(circleCI).Skills)

	errOut.Reset()
	printCommitStep(dir, before, candidates, streams)
	return errOut.String()
}

func TestInitClosingLinesListEveryFileItWrote(t *testing.T) {
	dir := gitrepo.SetupGitRepo(t, "o", "r")
	got := runInitWrites(t, dir, false)
	assert.Equal(t, got, "\nCommit these so runs start from them:\n"+
		"  git add .smelt/config.json Dockerfile.smelt .gitignore .claude/settings.json \\\n"+
		"          .codex/hooks.json .claude/skills/smelt-validate-config \\\n"+
		"          .agents/skills/smelt-validate-config\n"+
		"  git commit -m \"Set up smelt\"\n")
}

func TestInitClosingLinesListTheCircleCISkills(t *testing.T) {
	dir := gitrepo.SetupGitRepo(t, "o", "r")
	got := runInitWrites(t, dir, true)
	for _, name := range []string{"smelt-sidecar", "smelt-sidecar-setup", "smelt-validate-config"} {
		assert.Assert(t, strings.Contains(got, ".claude/skills/"+name), got)
		assert.Assert(t, strings.Contains(got, ".agents/skills/"+name), got)
	}
}

func TestInitClosingLinesSkipFilesItDidNotChange(t *testing.T) {
	dir := gitrepo.SetupGitRepo(t, "o", "r")
	runInitWrites(t, dir, false)
	// A second pass rewrites the same bytes, so there is nothing new to commit.
	assert.Equal(t, runInitWrites(t, dir, false), "")
}

func TestCommittingWhatInitPrintsLeavesNothingBehind(t *testing.T) {
	dir := gitrepo.SetupGitRepo(t, "o", "r")
	out := runInitWrites(t, dir, false)

	var add []string
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "git add "); ok {
			add = append(add, strings.Fields(strings.TrimSuffix(rest, "\\"))...)
		} else if strings.HasPrefix(line, "          ") {
			add = append(add, strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), "\\"))...)
		}
	}
	for _, args := range [][]string{append([]string{"add"}, add...), {"commit", "-m", "Set up smelt"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitrepo.GitEnv(dir)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
	}

	wt, err := factory.CreateWorktree(context.Background(), factory.WorktreeOptions{Root: dir, Path: filepath.Join(t.TempDir(), "wt"), RunID: "run-1", Prompt: "x"})
	assert.NilError(t, err)
	assert.Equal(t, wt.Baseline, wt.Head)
	assert.Equal(t, len(wt.Uncommitted), 0)
}

func TestInitCandidatesNameOnlySkillsThatExist(t *testing.T) {
	for _, p := range initCandidates() {
		assert.Assert(t, !strings.Contains(p, "example"), p)
	}
	assert.Assert(t, len(skills.All) > 0)
}
