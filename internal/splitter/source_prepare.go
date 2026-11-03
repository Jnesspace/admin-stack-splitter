package splitter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func prepareFromStack(config, statePath string, snapshot []byte, choicesPath, out, tool string, validate bool) (*prepareResult, error) {
	spec, err := layoutFromStackSettings(snapshot)
	if err != nil {
		return nil, err
	}
	if choicesPath != "" {
		raw, err := os.ReadFile(choicesPath)
		if err != nil {
			return nil, err
		}
		var choices map[string]json.RawMessage
		if err := json.Unmarshal(raw, &choices); err != nil || choices == nil {
			return nil, fmt.Errorf("invalid split choices JSON")
		}
		for key, value := range choices {
			switch key {
			case "groups":
				err = json.Unmarshal(value, &spec.Groups)
			case "variables":
				err = json.Unmarshal(value, &spec.Variables)
			default:
				return nil, fmt.Errorf("with source stack settings, --layout accepts only groups and variables; %s must come from the source stack", key)
			}
			if err != nil {
				return nil, fmt.Errorf("invalid %s in split choices", key)
			}
		}
	}
	dir, err := os.MkdirTemp("", "admin-splitter-layout-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "layout.json")
	if err := writeJSON(path, spec); err != nil {
		return nil, err
	}
	return prepare(config, statePath, path, out, tool, validate)
}
