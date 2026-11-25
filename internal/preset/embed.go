package preset

import (
	"embed"
	"fmt"
	"sort"
)

//go:embed presets/*.json
var presetFS embed.FS

// Names lists the embedded preset names (e.g. "software"), sorted.
func Names() []string {
	entries, err := presetFS.ReadDir("presets")
	if err != nil {
		panic(fmt.Sprintf("preset: embedded presets: %v", err))
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name()[:len(e.Name())-len(".json")])
	}
	sort.Strings(out)
	return out
}

// Get returns the raw JSON of an embedded preset.
func Get(name string) ([]byte, bool) {
	data, err := presetFS.ReadFile("presets/" + name + ".json")
	if err != nil {
		return nil, false
	}
	return data, true
}
