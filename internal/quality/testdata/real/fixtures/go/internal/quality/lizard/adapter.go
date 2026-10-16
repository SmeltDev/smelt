// Package lizard is the default metrics provider: it runs the lizard
// code-complexity tool in the sandbox and maps its output onto the metrics
// contract of internal/quality. It is the only place that knows lizard's
// output formats.
package lizard

import (
	"encoding/csv"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/smeltdev/smelt/internal/quality"
)

// Markers separate the script's sections on stdout.
const (
	markVersion = "@@smelt:version@@"
	markCSV     = "@@smelt:csv@@"
	markDup     = "@@smelt:duplicates@@"
)

// NotInstalledHint is what the script says when lizard is missing.
const NotInstalledHint = "lizard is not installed in this image: run smelt factory image --update-kit"

// Provider is lizard run over the files git does not ignore, less those ex
// leaves out by default.
func Provider(ex quality.Excluder) quality.Provider {
	return quality.Provider{Script: Script(ex), Parse: Parse}
}

// Script lists the project's files with git, skips the paths ex's defaults
// leave out, and runs lizard three times: its version, its CSV report (one row
// per function) and its duplicate-code extension.
func Script(ex quality.Excluder) string {
	var b strings.Builder
	b.WriteString("command -v lizard >/dev/null 2>&1 || { echo '" + NotInstalledHint + "' >&2; exit 127; }\n")
	b.WriteString("files=$(mktemp) && trap 'rm -f \"$files\"' EXIT || exit 1\n")
	b.WriteString("git -c core.quotepath=off ls-files -co --exclude-standard | while IFS= read -r f; do\n")
	// Lizard also reads some text formats as code; only the languages smelt
	// names are measured.
	b.WriteString("  case \"$f\" in " + strings.Join(extensionPatterns(), "|") + ") ;; *) continue;; esac\n")
	if patterns := ex.ShellPatterns(); len(patterns) > 0 {
		b.WriteString("  case \"$f\" in " + strings.Join(patterns, "|") + ") continue;; esac\n")
	}
	b.WriteString("  [ -f \"$f\" ] && printf '%s\\n' \"$f\"\n")
	b.WriteString("done > \"$files\" || exit 1\n")
	b.WriteString("echo " + markVersion + "; lizard --version\n")
	b.WriteString("echo " + markCSV + "; tr '\\n' '\\0' < \"$files\" | xargs -0 -r lizard --csv || exit 1\n")
	// Clones are best effort: a failing duplicate scan leaves the section empty
	// and the report says clones were not measured.
	b.WriteString("echo " + markDup + "; tr '\\n' '\\0' < \"$files\" | xargs -0 -r lizard -Eduplicate || true\n")
	return b.String()
}

// Parse maps the script's output onto a contract.
func Parse(stdout string) (quality.Contract, error) {
	sections := splitSections(stdout)
	csvOut, ok := sections[markCSV]
	if !ok {
		return quality.Contract{}, errors.New("lizard printed no function report")
	}
	fns, err := parseCSV(csvOut)
	if err != nil {
		return quality.Contract{}, err
	}
	c := quality.Contract{Provider: providerName(sections[markVersion]), Functions: fns}
	c.Languages = languagesOf(fns)
	clones, err := parseDuplicates(sections[markDup])
	if err != nil {
		c.NotMeasured = append(c.NotMeasured, "clones: "+err.Error())
	}
	c.Clones = clones
	return c, nil
}

func splitSections(out string) map[string]string {
	sections := map[string]string{}
	var cur *strings.Builder
	var name string
	flush := func() {
		if cur != nil {
			sections[name] = cur.String()
		}
	}
	for line := range strings.SplitSeq(out, "\n") {
		switch strings.TrimSpace(line) {
		case markVersion, markCSV, markDup:
			flush()
			name, cur = strings.TrimSpace(line), &strings.Builder{}
			continue
		}
		if cur != nil {
			cur.WriteString(line + "\n")
		}
	}
	flush()
	return sections
}

func providerName(version string) string {
	if v := strings.TrimSpace(version); v != "" {
		return "lizard " + strings.Fields(v)[0]
	}
	return "lizard"
}

// anonymous names the functions lizard has no name for. Several in a file are
// told apart by the report, which numbers repeated names.
const anonymous = "(anonymous)"

// Columns of lizard's CSV report: NLOC, CCN, token, PARAM, length, location,
// file, function, long_name, start, end.
const (
	colNLOC     = 0
	colCCN      = 1
	colFile     = 6
	colFunction = 7
	colStart    = 9
	colEnd      = 10
	csvColumns  = 11
)

func parseCSV(out string) ([]quality.ContractFunction, error) {
	r := csv.NewReader(strings.NewReader(out))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read lizard's CSV: %w", err)
	}
	var fns []quality.ContractFunction
	for i, row := range rows {
		if i == 0 && len(row) > 0 && !isNumber(row[0]) {
			continue // a header, if a lizard version prints one
		}
		fn, err := csvFunction(row)
		if err != nil {
			return nil, fmt.Errorf("lizard's CSV row %d: %w", i+1, err)
		}
		fns = append(fns, fn)
	}
	return fns, nil
}

func csvFunction(row []string) (quality.ContractFunction, error) {
	if len(row) < csvColumns {
		return quality.ContractFunction{}, fmt.Errorf("has %d columns, want %d", len(row), csvColumns)
	}
	nums := map[int]int{}
	for _, col := range []int{colNLOC, colCCN, colStart, colEnd} {
		n, err := strconv.Atoi(strings.TrimSpace(row[col]))
		if err != nil {
			return quality.ContractFunction{}, fmt.Errorf("column %d is not a number: %q", col+1, row[col])
		}
		nums[col] = n
	}
	file := path.Clean(row[colFile])
	name := row[colFunction]
	if name == "" {
		// Lizard reports closures and lambdas on their own, without a name.
		name = anonymous
	}
	return quality.ContractFunction{
		File: file, Name: name, Language: languageOf(file),
		Start: nums[colStart], End: nums[colEnd], Complexity: nums[colCCN], Lines: nums[colNLOC],
	}, nil
}

func isNumber(s string) bool {
	_, err := strconv.Atoi(strings.TrimSpace(s))
	return err == nil
}

func languagesOf(fns []quality.ContractFunction) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range fns {
		if !seen[f.Language] {
			seen[f.Language] = true
			out = append(out, f.Language)
		}
	}
	return out
}

// extensionPatterns are `case` patterns for the files of the known languages.
func extensionPatterns() []string {
	exts := slices.Sorted(maps.Keys(languages()))
	out := make([]string, len(exts))
	for i, e := range exts {
		out[i] = "*" + e
	}
	return out
}

func languageOf(file string) string {
	ext := strings.ToLower(path.Ext(file))
	if lang, ok := languages()[ext]; ok {
		return lang
	}
	return strings.TrimPrefix(ext, ".")
}

// languages maps the extensions lizard reads to the language names the
// contract carries.
func languages() map[string]string {
	return map[string]string{
		".go": "go", ".py": "python", ".ts": "typescript", ".tsx": "typescript",
		".js": "javascript", ".jsx": "javascript", ".mjs": "javascript", ".cjs": "javascript",
		".java": "java", ".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".hh": "cpp",
		".cs": "csharp", ".rb": "ruby", ".php": "php", ".swift": "swift", ".kt": "kotlin", ".kts": "kotlin",
		".scala": "scala", ".rs": "rust", ".lua": "lua", ".erl": "erlang", ".m": "objective-c", ".mm": "objective-c",
		".zig": "zig", ".sol": "solidity", ".gd": "gdscript", ".pl": "perl", ".vue": "vue", ".ttcn": "ttcn", ".ttcn3": "ttcn",
		".f90": "fortran", ".f95": "fortran", ".f03": "fortran", ".f08": "fortran", ".hxx": "cpp", ".ino": "cpp", ".phtml": "php",
	}
}

// dupLocation is one line of a duplicate block: "path:first ~ last".
var dupLocation = regexp.MustCompile(`^(.+):(\d+) ~ (\d+)\s*$`)

// parseDuplicates reads the text report of lizard's duplicate extension: a
// block is a run of location lines between "Duplicate block:" and a line of
// carets. A report with neither blocks nor lizard's own summary is not one.
func parseDuplicates(out string) ([]quality.ContractClone, error) {
	var clones []quality.ContractClone
	var cur []quality.Location
	recognised := false
	flush := func() {
		if len(cur) >= 2 {
			clones = append(clones, cloneOf(cur))
		}
		cur = nil
	}
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Duplicate block"):
			flush()
			recognised = true
		case strings.HasPrefix(line, "^^^"):
			flush()
		case strings.HasPrefix(line, "Total duplicate rate"), strings.Contains(line, "Total nloc"):
			recognised = true
		default:
			if m := dupLocation.FindStringSubmatch(line); m != nil {
				start, _ := strconv.Atoi(m[2])
				end, _ := strconv.Atoi(m[3])
				if start >= 1 && end >= start {
					cur = append(cur, quality.Location{File: path.Clean(m[1]), Start: start, End: end})
				}
			}
		}
	}
	flush()
	if !recognised {
		return nil, errors.New("the duplicate report is in a format smelt does not know")
	}
	return clones, nil
}

// cloneOf is the block at locs, as long as its shortest copy.
func cloneOf(locs []quality.Location) quality.ContractClone {
	n := locs[0].End - locs[0].Start + 1
	for _, l := range locs {
		n = min(n, l.End-l.Start+1)
	}
	return quality.ContractClone{Lines: n, Locations: locs}
}
