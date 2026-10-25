package factory

import (
	"encoding/json"
	"fmt"
	"os"
)

// RunOverlay contains the supported caller settings for a factory build.
// Unrelated JSON keys are ignored; this is not a project configuration merge.
type RunOverlay struct {
	Sandbox struct {
		Dockerfile string `json:"dockerfile"`
	} `json:"sandbox"`
}

// ReadRunOverlay reads a JSON object containing optional sandbox.dockerfile.
// Paths are left relative to the invocation directory, just like caller flags.
func ReadRunOverlay(path string) (RunOverlay, error) {
	var overlay RunOverlay
	data, err := os.ReadFile(path)
	if err != nil {
		return overlay, fmt.Errorf("read run overlay: %w", err)
	}
	if err := json.Unmarshal(data, &overlay); err != nil {
		return RunOverlay{}, fmt.Errorf("parse run overlay: %w", err)
	}
	// JSON null is accepted by encoding/json for structs and strings, but is
	// not a compatible value for any of the supported overlay fields.
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return RunOverlay{}, fmt.Errorf("run overlay must be a JSON object")
	}
	if sandbox, ok := object["sandbox"]; ok {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(sandbox, &fields); err != nil || fields == nil {
			return RunOverlay{}, fmt.Errorf("run overlay sandbox must be an object")
		}
		if value, ok := fields["dockerfile"]; ok && string(value) == "null" {
			return RunOverlay{}, fmt.Errorf("run overlay sandbox.dockerfile must be a string")
		}
	}
	return overlay, nil
}
