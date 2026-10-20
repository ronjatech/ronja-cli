package tabledocs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// sidecarFile is the on-disk docs sidecar a pipeline folder keeps for a table it
// does not build; the pointer and map keep it three-state (see Docs). Two keys
// and no more: a sidecar that could declare a type or name would be a second,
// weaker way to create a table.
type sidecarFile struct {
	Description *string           `json:"description,omitempty"`
	Columns     map[string]string `json:"columns,omitempty"`
}

// ParseSidecar decodes one docs sidecar. Unknown keys are refused, not dropped:
// a `type` someone adds expecting it to be honoured would otherwise be ignored
// on every push. A JSON null column value decodes to an empty string, the same
// clearing claim; only an absent key is silence.
func ParseSidecar(body []byte) (Docs, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var file sidecarFile
	if err := dec.Decode(&file); err != nil {
		return Docs{}, fmt.Errorf("this is not a valid table-documentation file: %w", err)
	}
	if dec.More() {
		return Docs{}, fmt.Errorf("this file holds more than one JSON document; a docs sidecar is one object describing one table")
	}
	docs := Docs{Description: file.Description}
	if len(file.Columns) == 0 {
		return docs, nil
	}
	docs.Columns = make(map[string]string, len(file.Columns))
	names := make([]string, 0, len(file.Columns))
	for name := range file.Columns {
		names = append(names, name)
	}
	// Sorted so a file with two unusable keys is refused on the same one every run.
	sort.Strings(names)
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return Docs{}, fmt.Errorf("\"columns\" has an entry with a blank name, which names no column — remove it, or give it the column's name")
		}
		if trimmed != name {
			// Refused, not trimmed: names join by exact match, so " amount"
			// would stay unmatched for ever while looking correct in the file.
			return Docs{}, fmt.Errorf("the column name %q has whitespace around it — a column is matched by its exact name, so this one would never attach; write %q", name, trimmed)
		}
		// A column spelled twice in the file is collapsed by encoding/json (last
		// wins) before this sees it, so it cannot be reported.
		docs.Columns[name] = file.Columns[name]
	}
	return docs, nil
}
