// Package metricfile parses the committed file that defines one metric in a
// pipeline folder, and fingerprints what a push of it would send.
//
// STDLIB ONLY, exactly as internal/tabledocs is, and for the same reason: this
// is the one place that decides what a metric file MEANS, and a leaf with no
// Ronja imports can be read and tested without any of the loop around it.
//
// THE ONE RULE WORTH STATING UP FRONT: the recipe grammar belongs to the
// SERVER, and nothing here may become a second copy of it. `rdb.ValidateMetricRecipe`
// decides what a recipe may say — which aggregates exist, what a `value`
// expression may combine, what a native grain is — and a Go shape for it here
// would be a second thing to keep in step, whose drifted version would accept a
// recipe the compiler then refused (or, worse, silently DROP a key somebody
// wrote). So the recipe is carried as json.RawMessage from the file straight to
// the wire, and this package reaches into exactly one field of it: `source`,
// which has to be rewritten from the folder's alias to this organization's id
// before the request. Every other key — including one this build has never heard
// of — is passed through verbatim BY THIS PACKAGE.
//
// ⚠️ AND IS THEN DROPPED BY THE SERVER, TODAY, IN SILENCE. That last sentence is
// a statement about this file and nothing further: `rdb.DecodeMetricRecipe` is a
// plain json.Unmarshal with no DisallowUnknownFields and the canonical form
// stored in `metric_recipe` is a re-marshal of the TYPED struct, so a key the
// server's grammar does not know reaches it, is ignored, and is absent from what
// is persisted — with no warning on any write path. The file keeps the key, the
// row never had it, and nothing compares the two. Do not read "passed through
// verbatim" as a guarantee that it ARRIVES: closing that is BL-8c71
// (claude/backlog/), which owns the decision between refusing an unknown key and
// warning about one, and owns bringing this header into line with whichever it
// picks.
//
// The top level is deliberately the OPPOSITE rule, and it is one this package
// can actually keep. Unknown keys at the TOP level are an error, on the docs
// sidecar's rule (cli/README.md: "a dropped key is the failure this whole loop
// exists to remove") — a `verified` or an `owner` somebody adds expecting it to
// be honoured must be refused where it was written, not ignored on every push
// for ever. Unknown keys INSIDE the recipe are not refused here, because there
// the grammar is not this package's to know.
package metricfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// metricFile is the on-disk shape.
//
//	{
//	  "recipe": {
//	    "source": "orders",
//	    "time": {"column": "created_at", "native_grain": "day"},
//	    "dimensions": [{"name": "country"}],
//	    "base_measures": [
//	      {"name": "revenue", "agg": "sum",   "column": "amount"},
//	      {"name": "orders",  "agg": "count", "column": "*"}
//	    ],
//	    "value": "revenue / orders"
//	  },
//	  "description": "Average order value, per day.",
//	  "reportingTimezone": "Europe/Stockholm"
//	}
//
// Three keys and no more. The metric's NAME is the file's stem, not a key here,
// for the reason a .sql file's table name is its stem: one carrier, one name,
// and a `name` key would be a second place to say it that a rename would leave
// disagreeing with the filename.
//
// `description` and `reportingTimezone` are POINTERS because both are
// three-state, exactly as the docs sidecar's `description` is: an absent key is
// a field this folder does not manage and the row keeps whatever it has, a
// present one is a claim, and an empty claim clears what the row holds. There
// is no field for anything DERIVED from the recipe — additivity, the time and
// value columns, the dimension list, the compiled SQL, the source closure — and
// none for verification state. Both absences are the point rather than an
// oversight: a file that could set them could contradict its own recipe, or
// mint a metric asserting it is verified.
type metricFile struct {
	Recipe            json.RawMessage `json:"recipe"`
	Description       *string         `json:"description,omitempty"`
	ReportingTimezone *string         `json:"reportingTimezone,omitempty"`
}

// File is one parsed metric file.
type File struct {
	// Recipe is the definition EXACTLY as the file spells it — aliases and all,
	// unknown keys and all. Never sent as it stands: Resolve rewrites `source`
	// first.
	Recipe json.RawMessage
	// Description is the metric's prose, three-state (see metricFile).
	Description *string
	// ReportingTimezone is the calendar the metric is evaluated at, three-state.
	// An explicit "" is a RESET to the UTC literal, which the recipe route reads
	// as its own third state; an absent key leaves the declaration alone.
	ReportingTimezone *string
}

// Parse decodes one metric file.
//
// Unknown TOP-LEVEL keys are an ERROR rather than being dropped, exactly as in
// the docs sidecar and the `ronja automation` loop's curated JSON: a dropped key
// is the failure the whole sync idea exists to remove — the file says one thing,
// the row keeps another, and nothing anywhere reports a difference.
func Parse(body []byte) (File, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var file metricFile
	if err := dec.Decode(&file); err != nil {
		return File{}, fmt.Errorf("this is not a valid metric file: %w", err)
	}
	if dec.More() {
		return File{}, fmt.Errorf("this file holds more than one JSON document; a metric file is one object defining one metric")
	}
	if len(file.Recipe) == 0 || string(file.Recipe) == "null" {
		return File{}, fmt.Errorf("this file declares no \"recipe\", which is the whole of a metric's definition — a metric row cannot exist without one")
	}
	// COMPACTED, not re-marshalled through a Go shape: whitespace is
	// insignificant to the server and to this file's meaning, so re-indenting a
	// committed file must not read as a change worth a rebuild. Everything else
	// — key order, unknown keys, nested structure — survives byte for byte.
	compact, err := compactJSON(file.Recipe)
	if err != nil {
		return File{}, fmt.Errorf("this file's \"recipe\" is not valid JSON: %w", err)
	}
	if !isJSONObject(compact) {
		return File{}, fmt.Errorf("this file's \"recipe\" is not a JSON object — a recipe is {source, time, dimensions, base_measures, value}")
	}
	// Both three-state claims are trimmed below rather than carried straight
	// through: they are compared and fingerprinted, so a trailing newline in a
	// committed file must not read as an edit. Neither is set here — the literal
	// used to carry ReportingTimezone and then have it overwritten two lines
	// later, which read as if the two fields were handled differently.
	out := File{Recipe: compact}
	if file.Description != nil {
		trimmed := strings.TrimSpace(*file.Description)
		out.Description = &trimmed
	}
	if file.ReportingTimezone != nil {
		trimmed := strings.TrimSpace(*file.ReportingTimezone)
		out.ReportingTimezone = &trimmed
	}
	return out, nil
}

// Source reads the name the recipe's `source` names — an alias this folder
// declares, or a literal table id.
//
// It is the ONE field this package looks inside the recipe for, because it is
// the one that cannot survive the trip: a folder deployed to two organizations
// spells its source as a name, and each organization's server only knows an id.
// A missing or non-string `source` is refused HERE rather than left to the
// server, because the refusal a caller can act on names the FILE.
func (f File) Source() (string, error) {
	fields, err := f.fields()
	if err != nil {
		return "", err
	}
	raw, ok := fields["source"]
	if !ok {
		return "", fmt.Errorf("this file's recipe names no \"source\" — a metric reads exactly one table, and that is where its name goes")
	}
	var source string
	if err := json.Unmarshal(raw, &source); err != nil {
		return "", fmt.Errorf("this file's recipe has a \"source\" that is not a string (%s) — it is the name of the table the metric reads", string(raw))
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return "", fmt.Errorf("this file's recipe has an empty \"source\" — it is the name of the table the metric reads")
	}
	return source, nil
}

// Resolve returns the recipe with `source` replaced by the id given, which is
// the form that goes over the wire.
//
// EVERY OTHER KEY IS CARRIED VERBATIM, including one this build does not know
// about: the top-level object is decoded into raw fields and re-assembled, so
// nested values are the file's own bytes and only `source` is rewritten. Key
// ORDER is not preserved (Go marshals a map in sorted order) and does not need
// to be — this is the form that is SENT, and the form that is HASHED for
// comparison against a previous send, never the form the server's own
// compare-and-swap digest is taken over. That digest is always taken from bytes
// the server itself produced; see RecipeSHA256.
func (f File) Resolve(sourceID string) (json.RawMessage, error) {
	fields, err := f.fields()
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(sourceID)
	if err != nil {
		return nil, err
	}
	// A copy, so a caller that resolves the same file twice — once for the push
	// and once for a status — cannot see the first call's substitution.
	out := make(map[string]json.RawMessage, len(fields))
	for key, value := range fields {
		out[key] = value
	}
	out["source"] = encoded
	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return compactJSON(body)
}

// Fingerprint is this file's answer to "has the local file changed since the
// last push from this folder" — taken over the WHOLE of what a push would send,
// which is the resolved recipe plus both three-state fields.
//
// A SECOND fingerprint beside the lock's liveSHA256, and the two are not
// interchangeable, by the invariant wfdir.LockTable states: this one is taken
// from the FILE and compared against the file, liveSHA256 is taken from the ROW
// and compared against the row. Comparing the file against the row cannot work
// here at all — the row holds the recipe the server CANONICALIZED (its version
// defaulted, its source a real id), and the file holds aliases and may omit a
// defaultable key, so a byte comparison would report every metric as pending for
// ever and re-push it on every run.
//
// The resolved recipe is the argument rather than being computed here because
// resolution needs the folder's alias codec, which a stdlib-only leaf has no
// business holding. The three parts are separated by a NUL, so a description
// ending in the timezone's first characters cannot alias with the timezone
// itself, and each three-state field distinguishes ABSENT from an empty claim —
// they are different instructions, and a fingerprint that folded them would miss
// the edit that clears a description.
func Fingerprint(resolvedRecipe json.RawMessage, description, reportingTimezone *string) string {
	var buf bytes.Buffer
	buf.Write(resolvedRecipe)
	buf.WriteByte(0)
	writeThreeState(&buf, description)
	buf.WriteByte(0)
	writeThreeState(&buf, reportingTimezone)
	return sha256Hex(buf.Bytes())
}

// writeThreeState renders one optional claim so that ABSENT, an empty claim and
// a value are three distinguishable strings.
func writeThreeState(buf *bytes.Buffer, value *string) {
	if value == nil {
		buf.WriteString("absent")
		return
	}
	buf.WriteString("set:")
	buf.WriteString(*value)
}

// RecipeSHA256 fingerprints a recipe the way the SERVER's precondition is
// expressed: sha256 over the raw bytes, lowercase hex.
//
// ⚠️ THE BYTES MUST BE THE ONES THE SERVER SENT, HASHED EXACTLY AS RECEIVED.
// This is the digest `baseRecipeSha256` compares against, and the server takes
// it over the recipe as this API SENDS it — the `recipe` in the recipe route's
// own response, or `metricRecipe` from GET /feature/model/<draft>, which are the
// same document. So a client that decoded the recipe into a map and
// re-marshalled it would hash a different key order and the compare-and-swap
// would fail against an edit nobody made. That is why the recipe is carried as
// json.RawMessage from the response all the way here, and why Resolve's
// re-assembled form is explicitly NOT what this is called on.
//
// NOT the bytes STORED in `metric_recipe`, which is what this said before and
// what the server briefly did. `metric_recipe` is JSONB, so Postgres
// re-serializes what was written (keys sorted length-then-bytes, a space after
// `:` and `,`) — and Go's encoder COMPACTS an embedded json.RawMessage on the
// way out, stripping exactly those spaces from every surface that publishes it.
// No caller could ever hold the stored form, so a precondition taken over it was
// satisfiable by NOBODY: every conditional push 409'd against an edit nobody
// made. It was invisible from this side, because the CLI's fake instance
// computes both ends of the comparison the same way and so cannot disagree with
// itself. The server now hashes the wire form (feature.recipeSHA256 →
// canonicalRecipeBytes) and returns the same bytes from both surfaces, which
// makes "hash what you were handed" — always the right advice — true as well.
//
// An absent recipe (a row that is not a metric, or a JSON `null`) hashes to
// nothing rather than to the digest of the four bytes "null": empty is what
// every guard in this loop reads as "no answer", and a real digest there would
// arm a comparison against a recipe that does not exist.
func RecipeSHA256(recipe json.RawMessage) string {
	if len(recipe) == 0 || string(recipe) == "null" {
		return ""
	}
	return sha256Hex(recipe)
}

// sha256Hex is lowercase-hex sha256, the one hasher this package's two digests
// are taken with.
//
// UNEXPORTED, deliberately. It was exported "so a caller has one hasher and not
// three" and no such caller ever appeared: Fingerprint and RecipeSHA256 are what
// the loop calls, and each already answers a whole question rather than handing
// back a primitive to be assembled. An exported hasher would only invite a
// fourth digest taken over bytes nobody chose here — which is the exact mistake
// RecipeSHA256's warning is about.
//
// It is still byte-identical to wfdir.HashString and to the server's own digest,
// which is the property the preconditions depend on.
func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// fields decodes the recipe's top level into raw values, which is as far into it
// as this package ever goes.
func (f File) fields() (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(f.Recipe, &fields); err != nil {
		return nil, fmt.Errorf("this file's \"recipe\" is not a JSON object: %w", err)
	}
	return fields, nil
}

func compactJSON(body []byte) (json.RawMessage, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, body); err != nil {
		return nil, err
	}
	return json.RawMessage(buf.Bytes()), nil
}

func isJSONObject(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	return len(trimmed) > 0 && trimmed[0] == '{'
}
