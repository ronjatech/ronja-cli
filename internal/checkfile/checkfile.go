// Package checkfile parses the committed file that declares one table's health
// checks in a pipeline folder (`checks/<stem>.json`), and fingerprints what a
// live check looks like through the fields that file declares.
//
// STDLIB ONLY, as internal/metricfile and internal/tabledocs are, and for the
// same reason: this is the one place that decides what a checks file MEANS.
//
// THE SHAPE IS THIS PACKAGE'S, THE GRAMMAR IS THE SERVER'S. The parser refuses
// what makes a file ambiguous — an unknown key, a check with two bodies or none,
// two names that fold together, two freshness watchdogs — and nothing more. What
// an expression may say, how long a name may be and how many checks a table may
// hold are the server's rules (rtablecheck.Validate*), and a copy of them here
// would be a second thing to keep in step.
//
// THREE-STATE, on the `ronja automation` precedent: `severity`, `description`
// and `enabled` may each be absent, and an absent one is a field this file does
// not manage. On a create it takes the server's default (warn, "", enabled); on
// an update it is never sent. So a check silenced in the app is not re-enabled
// by a push of a file that does not say `enabled`, and the fingerprint a push
// records (Projection) covers only the fields the file declares — a UI silence
// of such a check is not drift.
package checkfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// Check kinds, mirrored from rtablecheck.KindExpression / KindFreshness.
const (
	KindExpression = "expression"
	KindFreshness  = "freshness"
)

// Severities, mirrored from rtablecheck.
const (
	SeverityWarn = "warn"
	SeverityFail = "fail"
)

// Field names, as the file and the HTTP body both spell them. A differing-field
// list is reported and sent in these words.
const (
	FieldName        = "name"
	FieldExpression  = "expression"
	FieldInterval    = "expectedIntervalHours"
	FieldSeverity    = "severity"
	FieldDescription = "description"
	FieldEnabled     = "enabled"
)

// Entry is one declared check.
type Entry struct {
	// Name is the check's name, trimmed. Matching against live checks is by
	// Fold(Name); the exact spelling is still a field the file manages.
	Name string
	// Expression is the expression check's body, trimmed; empty for a watchdog.
	Expression string
	// ExpectedIntervalHours is a freshness watchdog's interval; 0 for an
	// expression check.
	ExpectedIntervalHours int
	// Severity, Description and Enabled are three-state: nil means this file does
	// not manage the field.
	Severity    *string
	Description *string
	Enabled     *bool
}

// Kind is the check kind the entry's body makes it.
func (e Entry) Kind() string {
	if e.ExpectedIntervalHours > 0 {
		return KindFreshness
	}
	return KindExpression
}

// File is one parsed checks file.
type File struct {
	Checks []Entry
}

// fileJSON / entryJSON are the on-disk shape. Pointers throughout, so a present
// zero value is told apart from an absent key.
type fileJSON struct {
	Checks *[]entryJSON `json:"checks"`
}

type entryJSON struct {
	Name                  *string `json:"name,omitempty"`
	Expression            *string `json:"expression,omitempty"`
	ExpectedIntervalHours *int    `json:"expectedIntervalHours,omitempty"`
	Severity              *string `json:"severity,omitempty"`
	Description           *string `json:"description,omitempty"`
	Enabled               *bool   `json:"enabled,omitempty"`
}

// Fold is the name a check is matched by: lower case, trimmed — the server's
// unique index is on lower(name). Go and Postgres disagree about a handful of
// runes (İ, ß). A mismatch there does NOT end as a match: the server refuses the
// create as a taken name, the push's re-read (which folds in Go) does not find
// the row, and the entry is `refused` with the server's name_taken message.
func Fold(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// Parse decodes one checks file. Every refusal names the entry's index, so the
// author can find it.
func Parse(body []byte) (File, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var raw fileJSON
	if err := dec.Decode(&raw); err != nil {
		return File{}, fmt.Errorf("this is not a valid checks file: %w", err)
	}
	if dec.More() {
		return File{}, fmt.Errorf("this file holds more than one JSON document; a checks file is one object, {\"checks\": [...]}")
	}
	if raw.Checks == nil {
		return File{}, fmt.Errorf("this file declares no \"checks\" list — a checks file is {\"checks\": [{\"name\": ..., \"expression\": ...}]}")
	}
	out := File{Checks: make([]Entry, 0, len(*raw.Checks))}
	seen := map[string]int{}
	watchdog := -1
	for i, in := range *raw.Checks {
		entry, err := parseEntry(in)
		if err != nil {
			return File{}, fmt.Errorf("checks[%d]: %w", i, err)
		}
		fold := Fold(entry.Name)
		if prior, dup := seen[fold]; dup {
			return File{}, fmt.Errorf("checks[%d]: %q has the same name as checks[%d] — names are matched case-insensitively, and a table cannot hold two checks with one name", i, entry.Name, prior)
		}
		seen[fold] = i
		if entry.Kind() == KindFreshness {
			if watchdog >= 0 {
				return File{}, fmt.Errorf("checks[%d]: a second freshness check (checks[%d] is one) — a table has one enabled freshness check, since two can only disagree; edit that one's expectedIntervalHours instead", i, watchdog)
			}
			watchdog = i
		}
		out.Checks = append(out.Checks, entry)
	}
	return out, nil
}

func parseEntry(in entryJSON) (Entry, error) {
	var e Entry
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		return e, fmt.Errorf("\"name\" is required — a short human name, e.g. \"Every order has an ID\"")
	}
	e.Name = strings.TrimSpace(*in.Name)
	switch {
	case in.Expression != nil && in.ExpectedIntervalHours != nil:
		return e, fmt.Errorf("%q has both \"expression\" and \"expectedIntervalHours\" — a check is an expression check or a freshness check, not both", e.Name)
	case in.Expression != nil:
		e.Expression = strings.TrimSpace(*in.Expression)
		if e.Expression == "" {
			return e, fmt.Errorf("%q has an empty \"expression\"", e.Name)
		}
	case in.ExpectedIntervalHours != nil:
		if *in.ExpectedIntervalHours <= 0 {
			return e, fmt.Errorf("%q has \"expectedIntervalHours\": %d — it is a positive whole number of hours (1 hourly, 24 daily, 168 weekly)", e.Name, *in.ExpectedIntervalHours)
		}
		e.ExpectedIntervalHours = *in.ExpectedIntervalHours
	default:
		return e, fmt.Errorf("%q needs a body: \"expression\" (a boolean SQL scalar over the table) or \"expectedIntervalHours\" (a freshness check)", e.Name)
	}
	if in.Severity != nil {
		sev := strings.ToLower(strings.TrimSpace(*in.Severity))
		if sev != SeverityWarn && sev != SeverityFail {
			return e, fmt.Errorf("%q has \"severity\": %q — it is %q or %q; omit it to leave the check's severity alone", e.Name, *in.Severity, SeverityWarn, SeverityFail)
		}
		e.Severity = &sev
	}
	if in.Description != nil {
		desc := strings.TrimSpace(*in.Description)
		e.Description = &desc
	}
	if in.Enabled != nil {
		enabled := *in.Enabled
		e.Enabled = &enabled
	}
	return e, nil
}

// ResolveStem finds the `.sql` file a checks file's stem names among the
// folder's .sql paths. A checks file lives beside the table the folder builds,
// so a stem that names none — or, in a nested folder, more than one — is refused.
func ResolveStem(stem string, sqlPaths []string) (string, error) {
	var matches []string
	for _, p := range sqlPaths {
		base := path.Base(p)
		if strings.TrimSuffix(base, path.Ext(base)) == stem {
			matches = append(matches, p)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("there is no %s.sql in this folder — checks live beside the table the folder builds; a table built elsewhere keeps its checks in the app", stem)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%s.sql is ambiguous here (%s) — rename one so the checks file names a single table", stem, strings.Join(matches, ", "))
	}
}

// Live is the part of a live check this package compares.
type Live struct {
	Name                    string
	Kind                    string
	Expression              string
	ExpectedIntervalSeconds int
	Severity                string
	Description             string
	Enabled                 bool
}

// projection is the canonical form fingerprinted. Pointers with omitempty so an
// undeclared three-state field is absent from it, and a declared empty one is
// not.
type projection struct {
	Name            string  `json:"name"`
	Kind            string  `json:"kind"`
	Expression      string  `json:"expression,omitempty"`
	IntervalSeconds int     `json:"intervalSeconds,omitempty"`
	Severity        *string `json:"severity,omitempty"`
	Description     *string `json:"description,omitempty"`
	Enabled         *bool   `json:"enabled,omitempty"`
}

// declared is which three-state fields an entry manages.
type declared struct{ severity, description, enabled bool }

func (e Entry) declared() declared {
	return declared{e.Severity != nil, e.Description != nil, e.Enabled != nil}
}

func project(l Live, d declared) projection {
	p := projection{Name: strings.TrimSpace(l.Name), Kind: l.Kind}
	if l.Kind == KindFreshness {
		p.IntervalSeconds = l.ExpectedIntervalSeconds
	} else {
		p.Expression = strings.TrimSpace(l.Expression)
	}
	if d.severity {
		sev := l.Severity
		p.Severity = &sev
	}
	if d.description {
		desc := strings.TrimSpace(l.Description)
		p.Description = &desc
	}
	if d.enabled {
		enabled := l.Enabled
		p.Enabled = &enabled
	}
	return p
}

// asLive is the entry as the check it declares would stand, for projection. The
// undeclared fields are zero and are dropped by the projection anyway.
func (e Entry) asLive() Live {
	l := Live{Name: e.Name, Kind: e.Kind(), Expression: e.Expression, ExpectedIntervalSeconds: e.ExpectedIntervalHours * 3600}
	if e.Severity != nil {
		l.Severity = *e.Severity
	}
	if e.Description != nil {
		l.Description = *e.Description
	}
	if e.Enabled != nil {
		l.Enabled = *e.Enabled
	}
	return l
}

// LiveSHA256 fingerprints a live check through the fields this entry declares —
// the value a push records per check once it has verified that the live check
// matches the entry.
func (e Entry) LiveSHA256(l Live) string {
	return hashProjection(project(l, e.declared()))
}

// Matches reports whether a live check already says everything this entry
// declares.
func (e Entry) Matches(l Live) bool {
	return e.LiveSHA256(l) == hashProjection(project(e.asLive(), e.declared()))
}

// Differs names the declared fields whose value the live check does not hold,
// in the wire's words — exactly the fields an update sends. A kind mismatch is
// the caller's to refuse before asking.
func (e Entry) Differs(l Live) []string {
	var out []string
	if strings.TrimSpace(l.Name) != e.Name {
		out = append(out, FieldName)
	}
	if e.Kind() == KindFreshness {
		if l.ExpectedIntervalSeconds != e.ExpectedIntervalHours*3600 {
			out = append(out, FieldInterval)
		}
	} else if strings.TrimSpace(l.Expression) != e.Expression {
		out = append(out, FieldExpression)
	}
	if e.Severity != nil && l.Severity != *e.Severity {
		out = append(out, FieldSeverity)
	}
	if e.Description != nil && strings.TrimSpace(l.Description) != *e.Description {
		out = append(out, FieldDescription)
	}
	if e.Enabled != nil && l.Enabled != *e.Enabled {
		out = append(out, FieldEnabled)
	}
	return out
}

// Unmoved reports whether a live check still hashes to a recorded LiveSHA256 —
// the drift question: "has anyone changed this check since the folder last
// agreed with it?".
//
// The recorded hash was taken through the fields the entry declared THEN, which
// need not be the fields it declares now: an author who adds `"severity"` to an
// entry has changed the projection's shape, not the live check. So the check is
// re-projected through every declared set it could have been taken over. The
// projection names its keys, so two different sets cannot collide.
func Unmoved(l Live, recorded string) bool {
	for _, d := range []declared{
		{}, {severity: true}, {description: true}, {enabled: true},
		{severity: true, description: true}, {severity: true, enabled: true},
		{description: true, enabled: true}, {severity: true, description: true, enabled: true},
	} {
		if hashProjection(project(l, d)) == recorded {
			return true
		}
	}
	return false
}

func hashProjection(p projection) string {
	body, err := marshal(p)
	if err != nil {
		// A struct of strings, ints and bools cannot fail to marshal.
		panic(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Encode renders a file the way clone writes one: two-space indented, keys in a
// reading order (name, body, severity, description, enabled), and `<`, `>` and
// `&` left as they are — an expression is SQL, and `<` in a committed file
// is unreadable.
func Encode(f File) ([]byte, error) {
	entries := make([]entryJSON, 0, len(f.Checks))
	for _, e := range f.Checks {
		name := e.Name
		out := entryJSON{Name: &name, Severity: e.Severity, Description: e.Description, Enabled: e.Enabled}
		if e.Kind() == KindFreshness {
			hours := e.ExpectedIntervalHours
			out.ExpectedIntervalHours = &hours
		} else {
			expr := e.Expression
			out.Expression = &expr
		}
		entries = append(entries, out)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(fileJSON{Checks: &entries}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
