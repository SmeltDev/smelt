// Package sarif reads SARIF 2.1.0, the format static analysis tools report
// findings in, and tells which findings of one run are new compared with
// another. It is pure: it never touches files, so callers supply the source
// lines the matching needs.
package sarif

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
)

// Version is the only SARIF version Parse accepts.
const Version = "2.1.0"

// Finding is one result of a tool run, reduced to what matching and feedback
// need.
type Finding struct {
	RuleID  string
	Message string
	// File is the slash-separated path relative to the repo, "" for a finding
	// that belongs to no file.
	File string
	// Line is the 1-based start line, 0 when the tool gave none.
	Line int
	// Snippet is the text of the flagged region when the tool reported it.
	Snippet string
	// Text is the trimmed text of the flagged line. Annotate fills it; matching
	// uses it so that a finding that only moved is still the same finding.
	Text string
	// Fingerprints are the tool's own identities for the finding, as
	// "name=value", from partialFingerprints and fingerprints.
	Fingerprints []string
}

type log struct {
	Version string `json:"version"`
	Runs    *[]run `json:"runs"`
}

type run struct {
	Results []result `json:"results"`
}

type result struct {
	RuleID              string              `json:"ruleId"`
	Rule                struct{ ID string } `json:"rule"`
	Message             message             `json:"message"`
	Locations           []location          `json:"locations"`
	PartialFingerprints map[string]string   `json:"partialFingerprints"`
	Fingerprints        map[string]string   `json:"fingerprints"`
	Suppressions        []json.RawMessage   `json:"suppressions"`
}

type message struct {
	Text     string `json:"text"`
	Markdown string `json:"markdown"`
}

type location struct {
	PhysicalLocation struct {
		ArtifactLocation struct {
			URI string `json:"uri"`
		} `json:"artifactLocation"`
		Region struct {
			StartLine int `json:"startLine"`
			Snippet   struct {
				Text string `json:"text"`
			} `json:"snippet"`
		} `json:"region"`
	} `json:"physicalLocation"`
}

// Parse reads a SARIF log and returns the findings of all its runs. Results a
// tool marks as suppressed, such as by a nolint comment, are left out. root,
// when not empty, is the directory absolute paths are made relative to.
func Parse(data []byte, root string) ([]Finding, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, errors.New("the command printed no SARIF (empty output)")
	}
	var l log
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("the output is not valid JSON: %w", err)
	}
	if l.Runs == nil {
		return nil, errors.New("the output is not SARIF: it has no \"runs\"")
	}
	if l.Version != Version {
		return nil, fmt.Errorf("unsupported SARIF version %q, want %q", l.Version, Version)
	}
	var out []Finding
	for _, r := range *l.Runs {
		for _, res := range r.Results {
			if len(res.Suppressions) > 0 {
				continue
			}
			out = append(out, finding(res, root))
		}
	}
	return out, nil
}

func finding(res result, root string) Finding {
	f := Finding{RuleID: res.RuleID, Message: res.Message.Text}
	if f.RuleID == "" {
		f.RuleID = res.Rule.ID
	}
	if f.Message == "" {
		f.Message = res.Message.Markdown
	}
	f.Message = strings.Join(strings.Fields(f.Message), " ")
	if len(res.Locations) > 0 {
		p := res.Locations[0].PhysicalLocation
		f.File = relPath(p.ArtifactLocation.URI, root)
		f.Line = max(p.Region.StartLine, 0)
		f.Snippet = p.Region.Snippet.Text
	}
	f.Fingerprints = fingerprints(res)
	return f
}

// fingerprints lists both fingerprint maps as sorted-by-name pairs; the maps
// are small and the order only has to be deterministic.
func fingerprints(res result) []string {
	var out []string
	for _, m := range []map[string]string{res.PartialFingerprints, res.Fingerprints} {
		for k, v := range m {
			out = append(out, k+"="+v)
		}
	}
	slices.Sort(out)
	return out
}

// relPath turns a SARIF artifact URI into a repo-relative slash path.
func relPath(uri, root string) string {
	if uri == "" {
		return ""
	}
	p := strings.TrimPrefix(uri, "file://")
	if u, err := url.PathUnescape(p); err == nil {
		p = u
	}
	p = path.Clean(strings.ReplaceAll(p, `\`, "/"))
	if root != "" {
		root = strings.TrimRight(root, "/")
		if rest, ok := strings.CutPrefix(p, root+"/"); ok {
			p = rest
		}
	}
	return strings.TrimPrefix(p, "./")
}
