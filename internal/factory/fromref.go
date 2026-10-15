package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/smeltdev/smelt/internal/gitexec"
	"github.com/smeltdev/smelt/internal/gitutil"
)

// fromRemote is the remote --from fetches a branch from.
const fromRemote = "origin"

// ErrUnknownRef is returned when --from names neither a branch nor a commit.
var ErrUnknownRef = errors.New("no branch or commit")

// FromRef is what a run started with --from was based on: the ref as given and
// the commit it resolved to.
type FromRef struct {
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
}

// FromResolution is the result of ResolveFrom.
type FromResolution struct {
	// Commit is the commit the ref resolved to.
	Commit string
	// Branch is the branch the ref names, without origin/; "" for a commit or
	// a tag.
	Branch string
	// Warning is a problem that did not stop the resolution, such as a failed
	// fetch, so what is local was used.
	Warning string
}

// branchOf is the branch a ref names, with a leading origin/ removed.
func branchOf(ref string) string {
	return strings.TrimPrefix(ref, fromRemote+"/")
}

// ResolveFrom resolves ref in the repository at root to a commit without
// touching its checkout. It first fetches the branch from origin, which only
// moves remote-tracking refs, and a failed fetch is a Warning. Then it uses
// the local ref, unless origin's copy is ahead of it: the local branch wins
// when it is level with or has diverged from origin's. A ref that resolves
// neither way is ErrUnknownRef.
func ResolveFrom(ctx context.Context, root, ref string) (FromResolution, error) {
	var res FromResolution
	name := branchOf(ref)
	git := gitexec.Runner{Dir: root}
	remoteRef := "refs/remotes/" + fromRemote + "/" + name

	// A commit that is already here needs no fetch; a repository with no
	// origin has nothing to fetch from.
	if _, err := gitutil.RevParseCtx(ctx, root, ref); err != nil || !isHex(ref) {
		if git.Run(ctx, "remote", "get-url", fromRemote) == nil {
			if err := gitutil.FetchRemoteBranch(ctx, root, fromRemote, name); err != nil {
				res.Warning = fmt.Sprintf("could not fetch %s from %s, using what is local: %v", name, fromRemote, err)
			}
		}
	}

	local, localErr := gitutil.RevParseCtx(ctx, root, ref)
	remote, remoteErr := gitutil.RevParseCtx(ctx, root, remoteRef)
	switch {
	case localErr != nil && remoteErr != nil:
		return FromResolution{}, fmt.Errorf("%w %s", ErrUnknownRef, ref)
	case localErr != nil:
		res.Commit = remote
	case remoteErr == nil && local != remote && git.Run(ctx, "merge-base", "--is-ancestor", local, remote) == nil:
		res.Commit = remote
	default:
		res.Commit = local
	}
	if git.Run(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+ref) == nil {
		res.Branch = ref
	} else if remoteErr == nil {
		res.Branch = name
	}
	return res, nil
}

// isHex reports whether s could be an abbreviated or full commit hash.
func isHex(s string) bool {
	if len(s) < 4 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
