// Package patterns is smelt's catalogue of review patterns: named ways a
// change goes wrong, described as behaviour and illustrated per language. It
// serves 'smelt review patterns' and the prompts 'smelt factory bootstrap'
// writes. It imports nothing of smelt's own, so any package may use it.
package patterns

import (
	"embed"
	"fmt"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Version is the version of the JSON document 'smelt review patterns --json'
// prints.
const Version = 1

// AppliesToAll is the AppliesTo value of a pattern that is not tied to a
// language feature.
const AppliesToAll = "all"

// Area is the kind of review a pattern belongs to.
type Area string

// The review areas, in the order the catalogue lists them.
const (
	Requirements    Area = "requirements"
	Correctness     Area = "correctness"
	Maintainability Area = "maintainability"
	Testing         Area = "testing"
)

// areaAliases are other names for an area. The adversarial review checks the
// change against the request, which is what the requirements area is.
var areaAliases = map[string]Area{"adversarial": Requirements}

// Areas returns every area, in catalogue order.
func Areas() []Area {
	return []Area{Requirements, Correctness, Maintainability, Testing}
}

// ParseArea returns the area named s, or an error listing the valid names.
func ParseArea(s string) (Area, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if a, ok := areaAliases[s]; ok {
		return a, nil
	}
	for _, a := range Areas() {
		if string(a) == s {
			return a, nil
		}
	}
	names := make([]string, 0, len(Areas()))
	for _, a := range Areas() {
		names = append(names, string(a))
	}
	return "", fmt.Errorf("unknown area %q: choose one of %s", s, strings.Join(names, ", "))
}

// Example shows a pattern in one language.
type Example struct {
	// Is is code or behaviour that is the pattern.
	Is string `yaml:"is" json:"is"`
	// Isnt is similar code that is not, so a reviewer does not over-report.
	Isnt string `yaml:"isnt" json:"isnt"`
}

// Pattern is one named way a change goes wrong.
type Pattern struct {
	Name       string `yaml:"name" json:"name"`
	Area       Area   `yaml:"-" json:"area"`
	Definition string `yaml:"definition" json:"definition"`
	// Severity is the usual severity of a finding of this pattern: high,
	// medium, low or info.
	Severity string `yaml:"severity" json:"severity"`
	// AppliesTo lists the languages the pattern exists in, or AppliesToAll.
	AppliesTo []string `yaml:"applies_to" json:"applies_to"`
	// Examples are keyed by language name. A language with no entry has the
	// definition alone.
	Examples map[string]Example `yaml:"examples" json:"examples"`
}

// Document is what 'smelt review patterns --json' prints.
type Document struct {
	Version  int       `json:"version"`
	Patterns []Pattern `json:"patterns"`
}

// areaFile is the shape of one data file.
type areaFile struct {
	Area     Area      `yaml:"area"`
	Patterns []Pattern `yaml:"patterns"`
}

//go:embed data/*.yaml
var dataFS embed.FS

// Load returns the whole catalogue: areas in catalogue order, patterns in file
// order. Every pattern has a non-nil AppliesTo and Examples.
func Load() ([]Pattern, error) {
	var out []Pattern
	for _, a := range Areas() {
		path := "data/" + string(a) + ".yaml"
		raw, err := dataFS.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var f areaFile
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&f); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if f.Area != a {
			return nil, fmt.Errorf("%s holds area %q", path, f.Area)
		}
		for _, p := range f.Patterns {
			p.Area = a
			if len(p.AppliesTo) == 0 {
				p.AppliesTo = []string{AppliesToAll}
			}
			if p.Examples == nil {
				p.Examples = map[string]Example{}
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// NormalizeLanguages lower-cases, de-duplicates and sorts language names.
func NormalizeLanguages(languages []string) []string {
	var out []string
	for _, l := range languages {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// Select returns the patterns of area (every area when it is empty) that exist
// in at least one of languages, each with only the examples for those
// languages. A pattern with no example for them keeps its definition alone;
// another language's example is never substituted. With no languages nothing
// is filtered. Select does not modify all.
func Select(all []Pattern, area Area, languages []string) []Pattern {
	languages = NormalizeLanguages(languages)
	out := []Pattern{}
	for _, p := range all {
		if area != "" && p.Area != area {
			continue
		}
		if len(languages) > 0 && !appliesTo(p, languages) {
			continue
		}
		p.Examples = examplesFor(p.Examples, languages)
		out = append(out, p)
	}
	return out
}

func appliesTo(p Pattern, languages []string) bool {
	return slices.Contains(p.AppliesTo, AppliesToAll) ||
		slices.ContainsFunc(languages, func(l string) bool { return slices.Contains(p.AppliesTo, l) })
}

func examplesFor(all map[string]Example, languages []string) map[string]Example {
	out := map[string]Example{}
	for lang, ex := range all {
		if len(languages) == 0 || slices.Contains(languages, lang) {
			out[lang] = ex
		}
	}
	return out
}

// Markdown renders ps as one level-three section each: the name, the
// definition, an is/isn't pair per language in name order, and the usual
// severity. Callers write the heading above.
func Markdown(ps []Pattern) string {
	var b strings.Builder
	for i, p := range ps {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(Section(p))
	}
	return b.String()
}

// Section renders one pattern as Markdown.
func Section(p Pattern) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### `%s`\n%s\n", p.Name, p.Definition)
	langs := make([]string, 0, len(p.Examples))
	for l := range p.Examples {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	for _, l := range langs {
		fmt.Fprintf(&b, "- Is (%s): %s\n- Isn't (%s): %s\n", l, p.Examples[l].Is, l, p.Examples[l].Isnt)
	}
	fmt.Fprintf(&b, "- Usual severity: %s\n", p.Severity)
	return b.String()
}
