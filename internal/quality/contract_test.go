package quality

import (
	"testing"

	"gotest.tools/v3/assert"
)

func TestParseContract(t *testing.T) {
	c, err := ParseContract([]byte(`{
  "provider": "lizard 1.17.10", "languages": ["go", "python"],
  "functions": [{"file": "a.go", "name": "f", "language": "go", "start": 2, "end": 9, "complexity": 4, "lines": 6, "hash": "ignored"}],
  "clones": [{"lines": 22, "locations": [{"file": "a.py", "start": 5, "end": 27}, {"file": "b.py", "start": 90, "end": 112}]}],
  "not_measured": ["lib/thing.ex"]
}`))
	assert.NilError(t, err)
	assert.Equal(t, c.Provider, "lizard 1.17.10")
	assert.Equal(t, c.Functions[0].Complexity, 4)
	assert.DeepEqual(t, c.Files(), []string{"a.go", "a.py", "b.py"})
	assert.Assert(t, c.Measured())
}

func TestParseContractRejectsWhatIsNotOne(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"empty", "", "printed nothing"},
		{"not json", "lizard: command not found", "parse the metrics contract"},
		{"two documents", `{"functions":[]} {"functions":[]}`, "after the JSON"},
		{"wrong type", `{"functions": 3}`, "parse the metrics contract"},
		{"no name", `{"functions":[{"file":"a","start":1,"end":2}]}`, "needs a file and a name"},
		{"bad range", `{"functions":[{"file":"a","name":"f","start":5,"end":2}]}`, "lines 5-2"},
		{"lone clone", `{"clones":[{"lines":9,"locations":[{"file":"a","start":1,"end":9}]}]}`, "two locations"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseContract([]byte(tt.in))
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

func TestAnEmptyContractIsNotMeasured(t *testing.T) {
	c, err := ParseContract([]byte(`{"functions": []}`))
	assert.NilError(t, err)
	assert.Assert(t, !c.Measured())
}
