package factory

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/smeltdev/smelt/internal/gitexec"
	"github.com/smeltdev/smelt/internal/quality"
	"github.com/smeltdev/smelt/internal/quality/lizard"
)

// DetectLanguages returns the sorted names of the languages the project's
// files are written in: the files git tracks or would add, less the paths the
// quality check leaves out by default (vendored, generated and test code), by
// the extension map the quality check uses. A file in a language smelt does
// not name is ignored.
func DetectLanguages(ctx context.Context, root string) ([]string, error) {
	out, err := gitexec.Runner{Dir: root}.Output(ctx, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, fmt.Errorf("list the project's files: %w", err)
	}
	var ex quality.Excluder
	found := map[string]bool{}
	for _, f := range strings.Split(string(out), "\x00") {
		if f == "" || ex.Path(f) {
			continue
		}
		if lang, ok := lizard.LanguageOf(f); ok {
			found[lang] = true
		}
	}
	langs := make([]string, 0, len(found))
	for l := range found {
		langs = append(langs, l)
	}
	slices.Sort(langs)
	return langs, nil
}
