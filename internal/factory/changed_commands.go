package factory

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// expandChanged expands repository paths as individual shell arguments.
func expandChanged(template string, files []string) (string, string) {
	files = slices.Clone(files)
	slices.Sort(files)
	files = slices.Compact(files)
	dirs := make([]string, 0, len(files))
	for _, file := range files {
		dir := path.Dir(file)
		if dir == "." {
			dirs = append(dirs, "./")
		} else {
			dirs = append(dirs, "./"+dir)
		}
	}
	slices.Sort(dirs)
	dirs = slices.Compact(dirs)
	label := fmt.Sprintf("changed: %d files", len(files))
	if strings.Contains(template, "{dirs}") {
		label = fmt.Sprintf("changed: %d dirs", len(dirs))
	}
	return strings.NewReplacer("{files}", sidecar.ShellJoin(files), "{dirs}", sidecar.ShellJoin(dirs)).Replace(template), label
}

type commandVariant struct {
	command  config.Command
	baseline config.Command
	label    string
}

func changedVariants(commands []config.Command, files []string) []commandVariant {
	var variants []commandVariant
	for _, c := range commands {
		baseline := c
		label := ""
		if c.Changed != "" {
			if len(files) == 0 {
				continue
			}
			c.Run, label = expandChanged(c.Changed, files)
		}
		variants = append(variants, commandVariant{command: c, baseline: baseline, label: label})
	}
	return variants
}

func fullVariants(commands []config.Command) []commandVariant {
	var variants []commandVariant
	for _, c := range commands {
		if c.Changed != "" {
			variants = append(variants, commandVariant{command: c, baseline: c, label: "full"})
		}
	}
	return variants
}

// runVariants keeps original command names for SARIF baseline lookup and labels
// only completed results, before callbacks and status recording.
func (s *Sidecars) runVariants(ctx context.Context, variants []commandVariant) []Check {
	return runEach(variants, s.OnCheck, func(v commandVariant) Check {
		c := v.command
		var check Check
		if c.Format == config.FormatSARIF {
			check = s.sarifRatchet().check(ctx, c, v.baseline)
		} else {
			check = runCommand(ctx, s.Exec, s.Implementer.Entry, c)
		}
		if v.label != "" {
			check.Name += " (" + v.label + ")"
		}
		return check
	})
}
