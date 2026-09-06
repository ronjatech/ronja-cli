package wfdir

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// unknownKeys is what makes ronja.json safe to EXTEND: the keys a manifest
// carried that this build of the CLI has no field for, kept beside the decoded
// struct so a rewrite puts them back exactly where it found them.
//
// Without it the manifest is decoded into a typed struct and re-marshalled
// whole, so any key the running CLI does not know is silently DELETED the first
// time it rewrites the file — and push, discard and clone all rewrite it. That
// already bit `reportingTimezone`: a colleague on an older CLI dropped a
// hand-added declaration out of a committed file with no message anywhere. It is
// a nuisance for one string; it is data loss the moment the manifest carries a
// resource's declared dependencies, because "the folder no longer says what it
// reads" is not something a diff review reliably catches.
//
// It records the ORDER of every key, not only of the unknown ones, because
// position is half of "preserved" for a file that lives in a pull request. Put
// a key back on the END and an older CLI and a newer one fight over the same
// file forever, each rewrite moving it and each move showing up as a diff
// nobody asked for. Keys the file already had keep their place; a key the CLI
// adds afterwards goes on the end, and the next read folds it into the order.
//
// It is deliberately NOT a general "keep the file byte-for-byte" layer. Values
// are re-emitted through the JSON encoder, so a manifest is still normalized to
// two-space indentation exactly as SaveManifest has always written it. The
// encoder's HTML escaping applies to a preserved value like any other, so the
// ampersand in a hand-written query string comes back spelled \u0026. What
// is preserved is the DATA and its order, not somebody's hand formatting.
// Every build escapes identically, so the old CLI and the new one cannot
// ping-pong over it — it is one diff line, once, the first time a value
// carrying & < or > is rewritten.
//
// ⚠️ Preservation is PER OBJECT, and the objects are every one this package
// writes to a committed file: the manifest root, each instances[] entry, each
// stacks entry, and — in ronja.lock.json — the root, each stack, and each table
// and each automation under it. Every nesting level of both files carries a
// sidecar, which is the property to keep when a level is added: one that does
// not is silent, and the key it drops is a key a newer CLI wrote. A key nested inside a
// KNOWN key's value is not preserved: `parameters[]` decodes into
// api.WorkflowParameter and `access` into api.DataAppAccess, both re-marshalled
// whole, so an unknown key inside either is still dropped by a rewrite. That is
// deliberate. Those types live in internal/api and are the REQUEST bodies too —
// giving them a MarshalJSON would change what the CLI sends, not just what it
// writes — and the keys the format is growing next sit at the root or on an
// entry. If a nested unknown key ever has to survive (the access block has
// already grown twice, with the codex and metric allowlists), the fix is a
// sidecar on those api types plus a decision about the wire, not a wider capture
// here; see the backlog item BL-8c4f.
type unknownKeys struct {
	// order is every key of the object as it was read, in document order —
	// known and unknown alike, since restoring position needs both.
	order []string
	// values holds only the keys with no struct field, since the known ones are
	// re-marshalled from the struct (where they may have CHANGED, or been
	// cleared by an omitempty field going empty).
	values map[string]json.RawMessage
}

// capture records the unknown keys and the key order of one decoded JSON
// object. known is the set of keys the receiving struct marshals to.
//
// A malformed object is not an error here: the typed decode that runs beside
// this one produces the error a human can act on, and duplicating it would only
// mean two spellings of the same complaint.
func (u *unknownKeys) capture(data []byte, known map[string]bool) {
	order, fields, err := objectFields(data)
	if err != nil {
		return
	}
	u.order = order
	u.values = nil
	for key, raw := range fields {
		if known[key] {
			continue
		}
		if u.values == nil {
			u.values = make(map[string]json.RawMessage, 1)
		}
		u.values[key] = raw
	}
}

// merge splices the unknown keys back into body — the object the typed struct
// marshalled to — restoring the key order the file was read in.
//
// The struct's value WINS for every key it produced: a captured value is only
// ever consulted for a key the struct has no field for, so nothing this CLI
// understands can be resurrected from a stale copy.
func (u unknownKeys) merge(body []byte) ([]byte, error) {
	if len(u.order) == 0 && len(u.values) == 0 {
		// Nothing was read (an object built in code by init or clone), so the
		// struct's own field order is the answer.
		return body, nil
	}
	marshalledOrder, marshalled, err := objectFields(body)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	written := make(map[string]bool, len(marshalled)+len(u.values))
	write := func(key string, raw json.RawMessage) error {
		if written[key] {
			return nil
		}
		written[key] = true
		if buf.Len() > 1 {
			buf.WriteByte(',')
		}
		encoded, err := json.Marshal(key)
		if err != nil {
			return err
		}
		buf.Write(encoded)
		buf.WriteByte(':')
		buf.Write(raw)
		return nil
	}

	for _, key := range u.order {
		switch raw, ok := marshalled[key]; {
		case ok:
			// A key the struct still produces: its CURRENT value, in the place
			// the file had it.
			if err := write(key, raw); err != nil {
				return nil, err
			}
		default:
			// Not in the struct's output. Either it is unknown — put it back —
			// or it is a known omitempty field that has since gone empty, in
			// which case dropping it is the whole point of omitempty and
			// restoring it would undo a deliberate clear.
			if raw, ok := u.values[key]; ok {
				if err := write(key, raw); err != nil {
					return nil, err
				}
			}
		}
	}
	// Anything the struct produced that the file did not have — a field this
	// command just set — goes on the end, in struct order.
	for _, key := range marshalledOrder {
		if err := write(key, marshalled[key]); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// ensureLeading returns a copy whose key order starts with key, when the object
// this sidecar was read from did not carry it at all.
//
// It exists for exactly one key — formatVersion — and for a reason particular to
// it. merge puts a key the struct produced but the FILE did not on the END, which
// is right for an ordinary new field: it lands where a reader expects a fresh
// line. formatVersion is not ordinary. It is the first thing anyone opening the
// file needs to know, it is what an older CLI's gate reads, and appearing last in
// a migrated manifest reads as an afterthought rather than as the file's own
// declaration of what it is.
//
// A no-op when the file already had the key (its recorded position wins) and
// when nothing was captured at all (an object built in code marshals in struct
// order, where formatVersion is already first).
func (u unknownKeys) ensureLeading(key string) unknownKeys {
	if len(u.order) == 0 {
		return u
	}
	for _, existing := range u.order {
		if existing == key {
			return u
		}
	}
	order := make([]string, 0, len(u.order)+1)
	order = append(order, key)
	u.order = append(order, u.order...)
	return u
}

// dropObjectKey removes one key from a marshalled object, preserving the order
// of the rest.
//
// It exists so a struct field can be emitted CONDITIONALLY without giving up
// what its json tag guarantees. `instances` has no omitempty on purpose — that
// is what keeps a legacy manifest writing the key it has always written, empty
// or not — and a stack folder still must not carry it. Encoding and then
// removing is the honest version of that: one rule, applied where the condition
// is known, instead of a second nearly-identical struct.
func dropObjectKey(body []byte, drop string) ([]byte, error) {
	order, fields, err := objectFields(body)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for _, key := range order {
		if key == drop {
			continue
		}
		if buf.Len() > 1 {
			buf.WriteByte(',')
		}
		encoded, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		buf.Write(encoded)
		buf.WriteByte(':')
		buf.Write(fields[key])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// objectFields decodes a JSON object one key at a time, reporting the keys in
// DOCUMENT order alongside their raw values. encoding/json's map decode loses
// the order, and the order is the thing this file exists to keep.
//
// Values come back as json.RawMessage, so an unknown key round-trips whatever
// it held — a nested object, a number spelled 1.0, a string — without this
// package having to model it.
func objectFields(data []byte) ([]string, map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, nil, fmt.Errorf("expected a JSON object, got %v", tok)
	}
	var order []string
	fields := map[string]json.RawMessage{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, nil, fmt.Errorf("expected an object key, got %v", keyTok)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, err
		}
		// JSON permits a repeated key and encoding/json takes the last one; the
		// order list must not then name it twice, or the rewrite would emit a
		// duplicate of its own making.
		if _, seen := fields[key]; !seen {
			order = append(order, key)
		}
		fields[key] = raw
	}
	return order, fields, nil
}

// plainManifest and plainInstance are the method-LESS twins the four
// Marshal/UnmarshalJSON methods decode and encode through. A defined type does
// not carry its source type's methods, so json.Unmarshal on a twin does the
// ordinary struct decode those methods wrap, instead of calling them again
// forever.
//
// They are declared here, package-level and once, rather than as a local `type
// plain X` inside each method, because encoding/json puts the name in its error
// text and a person hand-editing ronja.json then reads it. Two types both named
// `plain` cannot be told apart in a message; these two can — see
// rewriteInternalTypeNames, which is the thing that renames them back.
type (
	plainManifest       Manifest
	plainInstance       Instance
	plainStack          Stack
	plainDependency     Dependency
	plainLock           Lock
	plainLockStack      LockStack
	plainLockTable      LockTable
	plainLockTableDocs  LockTableDocs
	plainLockAutomation LockAutomation
)

// publicTwin maps each twin back to the type a person actually knows about.
//
// One map so the rename cannot cover one twin and forget the other, which is
// the failure a pair of string constants invites.
var publicTwin = map[reflect.Type]reflect.Type{
	reflect.TypeOf(plainManifest{}):       reflect.TypeOf(Manifest{}),
	reflect.TypeOf(plainInstance{}):       reflect.TypeOf(Instance{}),
	reflect.TypeOf(plainStack{}):          reflect.TypeOf(Stack{}),
	reflect.TypeOf(plainDependency{}):     reflect.TypeOf(Dependency{}),
	reflect.TypeOf(plainLock{}):           reflect.TypeOf(Lock{}),
	reflect.TypeOf(plainLockStack{}):      reflect.TypeOf(LockStack{}),
	reflect.TypeOf(plainLockTable{}):      reflect.TypeOf(LockTable{}),
	reflect.TypeOf(plainLockTableDocs{}):  reflect.TypeOf(LockTableDocs{}),
	reflect.TypeOf(plainLockAutomation{}): reflect.TypeOf(LockAutomation{}),
}

// rewriteInternalTypeNames renames the decode twins out of a json type error,
// in place, before the error is shown to anyone.
//
// encoding/json builds its message from the Go type it was decoding, and every
// decode here runs through a twin — so a mistyped field in a committed manifest
// used to report `cannot unmarshal object into Go struct field
// plainManifest.instances.url of type string`. `plainManifest` is a name from
// inside this file: it tells the reader nothing about their file and reads like
// a bug in the CLI. Both spellings are covered, because the type appears as the
// STRUCT the field belongs to in one message and as the whole decoded TYPE in
// the other (`into Go value of type wfdir.plainManifest`, for a manifest that
// is not an object at all).
//
// It mutates rather than rebuilding the message, so the caller goes on wrapping
// the same typed error and anything downstream matching on it still matches.
func rewriteInternalTypeNames(err error) {
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) {
		return
	}
	if public, ok := publicTwin[typeErr.Type]; ok {
		typeErr.Type = public
	}
	for twin, public := range publicTwin {
		if typeErr.Struct == twin.Name() {
			typeErr.Struct = public.Name()
			break
		}
	}
	typeErr.Field = flattenEmbeddedPath(typeErr.Field)
}

// flattenEmbeddedPath drops embedded struct names out of a json field path, so
// the path names where the value actually sits in the FILE.
//
// Binding is embedded in Instance, so its fields flatten into the entry's own
// JSON object — there is no "binding" key in a manifest, and never was. But
// encoding/json builds its field path from the Go structure, so a mistyped
// workflowID reported as `Manifest.instances.Binding.workflowID` sends someone
// looking for a key their file does not contain. Dropping the segment gives
// `Manifest.instances.workflowID`, which they can find.
//
// The name comes from reflection rather than a literal, so renaming the type
// carries the fix with it instead of quietly stranding this string.
func flattenEmbeddedPath(field string) string {
	embedded := reflect.TypeOf(Binding{}).Name() + "."
	return strings.ReplaceAll(field, embedded, "")
}

// jsonFieldNames is the set of object keys a struct type marshals to.
//
// Derived by reflection rather than written out by hand so that adding a field
// to Manifest or Instance automatically makes its key KNOWN. A hand-maintained
// list would be one someone forgets, and forgetting it here is silent and
// nasty: the new field's key would be captured as "unknown" and then re-emitted
// from the stale captured copy, so every write of that field would be reverted
// by the very layer meant to protect it.
func jsonFieldNames(t reflect.Type) map[string]bool {
	names := map[string]bool{}
	collectJSONFieldNames(t, names)
	return names
}

func collectJSONFieldNames(t reflect.Type, into map[string]bool) {
	for i := range t.NumField() {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" && tag == "-" {
			continue
		}
		// An embedded struct with no name of its own FLATTENS into the same
		// object — Instance embeds Binding, and workflowID sits beside url in
		// one entry. Its keys therefore belong to the outer type's set.
		embedded := field.Type
		if embedded.Kind() == reflect.Pointer {
			embedded = embedded.Elem()
		}
		if field.Anonymous && name == "" && embedded.Kind() == reflect.Struct {
			collectJSONFieldNames(embedded, into)
			continue
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		into[name] = true
	}
}
