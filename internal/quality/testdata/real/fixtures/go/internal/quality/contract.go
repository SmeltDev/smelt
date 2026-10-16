package quality

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Contract is what a provider prints: the functions and duplicated blocks of
// a tree, in any language. smelt never parses source itself; everything after
// the provider works from this.
type Contract struct {
	// Provider names the tool and version that measured, for display only.
	Provider string `json:"provider,omitempty"`
	// Languages are the languages the provider measured.
	Languages []string           `json:"languages,omitempty"`
	Functions []ContractFunction `json:"functions"`
	Clones    []ContractClone    `json:"clones,omitempty"`
	// NotMeasured are files the provider could not measure, or the reason clones
	// could not be.
	NotMeasured []string `json:"not_measured,omitempty"`
}

// ContractFunction is one function. Lines is the function's non-blank,
// non-comment lines; Complexity is the provider's cyclomatic complexity.
type ContractFunction struct {
	File       string `json:"file"`
	Name       string `json:"name"`
	Language   string `json:"language,omitempty"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
	Complexity int    `json:"complexity"`
	Lines      int    `json:"lines"`
	// Hash is ignored on input: smelt computes it from the source.
	Hash string `json:"hash,omitempty"`
}

// ContractClone is a block that occurs at several places, Lines long at each.
type ContractClone struct {
	Lines     int        `json:"lines"`
	Locations []Location `json:"locations"`
}

// Location is a line range of a file.
type Location struct {
	File  string `json:"file"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// ParseContract reads and validates a contract. Output that is not one is an
// error, never an empty contract: a provider that failed must not look like a
// tree with nothing in it.
func ParseContract(data []byte) (Contract, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Contract{}, errors.New("parse the metrics contract: the provider printed nothing")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var c Contract
	if err := dec.Decode(&c); err != nil {
		return Contract{}, fmt.Errorf("parse the metrics contract: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Contract{}, errors.New("parse the metrics contract: unexpected data after the JSON document")
	}
	if err := c.validate(); err != nil {
		return Contract{}, fmt.Errorf("parse the metrics contract: %w", err)
	}
	return c, nil
}

func (c Contract) validate() error {
	for i, f := range c.Functions {
		switch {
		case f.File == "" || f.Name == "":
			return fmt.Errorf("function %d needs a file and a name", i)
		case f.Start < 1 || f.End < f.Start:
			return fmt.Errorf("function %s in %s has the lines %d-%d", f.Name, f.File, f.Start, f.End)
		case f.Complexity < 0 || f.Lines < 0:
			return fmt.Errorf("function %s in %s has a negative measurement", f.Name, f.File)
		}
	}
	for i, cl := range c.Clones {
		if len(cl.Locations) < 2 || cl.Lines < 1 {
			return fmt.Errorf("clone %d needs a length and at least two locations", i)
		}
		for _, l := range cl.Locations {
			if l.File == "" || l.Start < 1 || l.End < l.Start {
				return fmt.Errorf("clone %d has a bad location %s:%d-%d", i, l.File, l.Start, l.End)
			}
		}
	}
	return nil
}

// Measured reports whether the provider measured anything: an empty contract
// is a tree nothing could measure, which is not an error.
func (c Contract) Measured() bool { return len(c.Functions) > 0 || len(c.Languages) > 0 }

// Files are the files the contract names: those of its functions and clones.
func (c Contract) Files() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, f := range c.Functions {
		add(f.File)
	}
	for _, cl := range c.Clones {
		for _, l := range cl.Locations {
			add(l.File)
		}
	}
	return out
}
