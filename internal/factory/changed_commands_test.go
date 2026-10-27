package factory

import (
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
)

func TestExpandChanged(t *testing.T) {
	for _, tc := range []struct {
		name, template string
		files          []string
		want, label    string
	}{
		{"empty", "check {files} {dirs}", nil, "check  ", "changed: 0 dirs"},
		{"quoted", "check {files}", []string{"z;echo", "a b", "it's"}, "check 'a b' 'it'\\''s' 'z;echo'", "changed: 3 files"},
		{"directories", "check {dirs}", []string{"pkg/a", "root", "pkg/b", "a b/x", "it's/x"}, "check './' './a b' './it'\\''s' './pkg'", "changed: 4 dirs"},
		{"both", "{files} {dirs} {files}", []string{"p/a", "p/a"}, "'p/a' './p' 'p/a'", "changed: 1 dirs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, label := expandChanged(tc.template, tc.files)
			assert.Equal(t, got, tc.want)
			assert.Equal(t, label, tc.label)
		})
	}
}
func TestCommandVariantsPreserveSettings(t *testing.T) {
	commands := []config.Command{{Name: "legacy", Run: "all"}, {Name: "test", Run: "full", Changed: "check {files}", Timeout: 42, Format: config.FormatSARIF, SarifPath: "report"}}
	assert.Equal(t, len(changedVariants(commands, nil)), 1)
	variants := changedVariants(commands, []string{"a"})
	assert.Equal(t, variants[0].command, commands[0])
	want := commands[1]
	want.Run = "check 'a'"
	assert.Equal(t, variants[1].command, want)
	assert.Equal(t, fullVariants(commands)[0].command, commands[1])
}
