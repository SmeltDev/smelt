package cmd

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/reviews/patterns"
)

// languageNameRe is the shape of a language name: lizard's names, such as
// "go" or "objective-c".
var languageNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func newReviewPatternsCmd() *cobra.Command {
	var languages []string
	var area string
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "patterns",
		Short: "Print the catalogue of review patterns",
		Long: fmt.Sprintf(`Print the review patterns smelt knows: named ways a change goes wrong, such as
silent-fallback or lonely-interface, each with a definition, its usual severity
and an is/isn't example per language.

--language limits the examples to those languages (repeat it or separate
names with commas), and leaves out patterns that do not exist in them. A
language with no examples gets the definitions alone. --area limits the
output to one of: %s ("adversarial" is another name for requirements).

With --json the output is a versioned document that other tools may read; see
docs/CLI.md.`, areaNames()),
		Example: `  smelt review patterns --language python --area correctness
  smelt review patterns --json`,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			streams := iostream.FromCmd(cmd)
			langs, err := checkLanguages(languages)
			if err != nil {
				return err
			}
			var want patterns.Area
			if area != "" {
				if want, err = patterns.ParseArea(area); err != nil {
					return badPatternsFlag(err)
				}
			}
			all, err := patterns.Load()
			if err != nil {
				return fmt.Errorf("load the pattern catalogue: %w", err)
			}
			selected := patterns.Select(all, want, langs)
			if jsonOut {
				return iostream.PrintJSON(streams.Out, patterns.Document{Version: patterns.Version, Patterns: selected})
			}
			printPatterns(streams, selected)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&languages, "language", nil, "limit examples to this language, such as python (repeatable)")
	cmd.Flags().StringVar(&area, "area", "", "limit to one area: "+areaNames())
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

// areaNames lists the areas for help text and errors.
func areaNames() string {
	names := make([]string, 0, len(patterns.Areas()))
	for _, a := range patterns.Areas() {
		names = append(names, string(a))
	}
	return strings.Join(names, ", ")
}

// checkLanguages normalizes the --language values and rejects text that cannot
// be a language name. A name the catalogue has no examples for is fine.
func checkLanguages(languages []string) ([]string, error) {
	langs := patterns.NormalizeLanguages(languages)
	for _, l := range langs {
		if !languageNameRe.MatchString(l) {
			return nil, badPatternsFlag(fmt.Errorf("invalid language %q", l))
		}
	}
	return langs, nil
}

func badPatternsFlag(err error) error {
	return newUserError(err.Error() + ".").
		withCode("command.invalid_args").
		withExitCode(ExitBadArgs).
		wrap(err).
		withoutDetail()
}

// printPatterns writes the patterns as Markdown, a heading per area.
func printPatterns(streams iostream.Streams, selected []patterns.Pattern) {
	for _, a := range patterns.Areas() {
		var inArea []patterns.Pattern
		for _, p := range selected {
			if p.Area == a {
				inArea = append(inArea, p)
			}
		}
		if len(inArea) == 0 {
			continue
		}
		streams.Println("## " + string(a))
		streams.Println()
		streams.Println(patterns.Markdown(inArea))
	}
}
