package tabledocs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// sidecarFile is the on-disk shape of a docs sidecar — the committed file a
// pipeline folder keeps for a table it does NOT build.
//
//	{
//	  "description": "One row per Fortnox invoice, synced hourly.",
//	  "columns": {
//	    "invoice_no": "The supplier's own invoice number, not Ronja's id.",
//	    "amount": "Line total excluding VAT, in SEK."
//	  }
//	}
//
// Both keys are POINTERS/maps rather than plain values because the whole file is
// three-state (see Docs): an absent `description` is a description this folder
// does not manage, and an absent column key is prose it does not manage. A
// present one is a claim, and an empty claim clears what the row holds.
//
// Two keys and no more, deliberately. This file is documentation, not a table
// definition: it declares no type, no kind, no lineage and no name, and a folder
// that could declare a schema here would be a second, weaker way to create a
// table beside the one that actually builds it.
type sidecarFile struct {
	Description *string           `json:"description,omitempty"`
	Columns     map[string]string `json:"columns,omitempty"`
}

// ParseSidecar decodes one docs sidecar.
//
// Unknown keys are an ERROR rather than being dropped, exactly as in the
// `ronja automation` loop's curated JSON and for the same reason: a dropped key
// is the failure the whole sync idea exists to remove — the file says one thing,
// the row keeps another, and nothing anywhere reports a difference. A `type` or
// a `name` somebody adds expecting it to be honoured must be refused where it
// was written, not ignored on every push for ever.
//
// A JSON `null` column value decodes to "", which is the same claim an explicit
// "" makes: clear the prose on that column. Both are a claim; only an ABSENT key
// is silence.
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
	// Sorted before the loop so a file with two unusable keys names them in the
	// same order on every run, and the refusal is diffable.
	sort.Strings(names)
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return Docs{}, fmt.Errorf("\"columns\" has an entry with a blank name, which names no column — remove it, or give it the column's name")
		}
		if trimmed != name {
			// Refused rather than trimmed: a name is JOINED against the table's
			// measured columns by exact match, so " amount" would be reported as
			// unmatched for ever while looking correct in the file. Trimming it
			// silently would be the same trap the other way round, hiding a typo
			// the author can see.
			return Docs{}, fmt.Errorf("the column name %q has whitespace around it — a column is matched by its exact name, so this one would never attach; write %q", name, trimmed)
		}
		// No duplicate check: `names` is the key set of a Go map, so a name
		// appears once by construction. A file that really does spell one column
		// twice is collapsed by encoding/json before this ever sees it — last
		// value wins — which is JSON's own rule and not something this parser can
		// observe, let alone report.
		docs.Columns[name] = file.Columns[name]
	}
	return docs, nil
}
