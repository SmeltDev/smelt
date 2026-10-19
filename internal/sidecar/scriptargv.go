package sidecar

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/smeltdev/smelt/internal/brand"
	"github.com/smeltdev/smelt/internal/circleci"
)

// MaxArgBytes is the largest single argument smelt builds for an exec. Linux
// refuses one argument over 128 KiB (MAX_ARG_STRLEN) with a message that does
// not say which argument, so anything near that is a bug caught here instead.
const MaxArgBytes = 100 * 1024

// inlineScriptBytes is the largest script submitted as an argument to the
// CircleCI exec API. Bigger ones are staged in a file first: the API has no
// stdin, and staging every script would cost an SSH session per command.
const inlineScriptBytes = 16 * 1024

// CheckArgv reports an argument over MaxArgBytes.
func CheckArgv(args ...string) error {
	for _, a := range args {
		if len(a) > MaxArgBytes {
			return fmt.Errorf("internal: script passed as an argument (%d bytes)", len(a))
		}
	}
	return nil
}

// scriptArgv is the argv that runs script on a CircleCI sidecar. A script that
// fits goes inline; a larger one is handed to stage, which returns the path of
// a file holding it, and the command runs that file and removes it.
func scriptArgv(script string, stage func(script string) (path string, err error)) ([]string, error) {
	if len(script) <= inlineScriptBytes {
		return []string{"-c", script}, CheckArgv(script)
	}
	path, err := stage(script)
	if err != nil {
		return nil, fmt.Errorf("stage script: %w", err)
	}
	const runStaged = `sh "$0"; code=$?; rm -f "$0"; exit $code`
	return []string{"-c", runStaged, path}, nil
}

// SubmitScript runs script with `sh` on a CircleCI sidecar and returns the
// command ID. The script never travels in an argument beyond inlineScriptBytes.
func SubmitScript(ctx context.Context, client *circleci.Client, sidecarID, script string, env map[string]string) (string, error) {
	args, err := scriptArgv(script, func(script string) (string, error) {
		return stageScript(ctx, client, sidecarID, script)
	})
	if err != nil {
		return "", err
	}
	return client.SubmitExec(ctx, sidecarID, "sh", args, env)
}

// stageScript writes script to a new file on the sidecar through the SSH exec
// channel's stdin. The channel runs a bare binary without a shell, hence tee.
func stageScript(ctx context.Context, client *circleci.Client, sidecarID, script string) (string, error) {
	sess, err := OpenSession(ctx, client, sidecarID, false)
	if err != nil {
		return "", err
	}
	var r [8]byte
	_, _ = rand.Read(r[:])
	path := "/tmp/" + brand.Command + "-script-" + hex.EncodeToString(r[:])
	res, err := ExecOverSSH(ctx, sess, "tee "+path, strings.NewReader(script), nil)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("tee exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return path, nil
}
