package commands

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The alias layer's TRANSPORT EDGE, and the one invariant everything in this
// package depends on:
//
//	INSIDE internal/commands, FILE CONTENT IS ALWAYS IN DISK FORM — the aliases
//	the folder committed, byte for byte. Translation happens only where bytes
//	cross the wire: alias → id on the way OUT, id → alias on the way IN.
//
// That direction is FORCED, not chosen. wfdir.Enumerate hashes the bytes on
// disk, and those hashes are compared against the baseline's in places that have
// no stack in scope to resolve against (appStatus, warnIfAppDirty,
// pipelineRemoteStatus). Making the baseline a hash of RESOLVED bytes would mean
// every one of those needs a codec, and any site that missed one would read an
// aliased file as permanently drifted — with nothing the author could edit to
// fix it. So local hashing is untouched, and every remote read is canonicalized
// and THEN de-aliased before it is compared, hashed or written to disk.
type aliasCodec struct {
	// toID and toAlias are the two directions, keyed by (kind, name).
	//
	// The KIND is part of the key because a name means nothing on its own: a
	// dependency declared as a `table` says nothing about what
	// `{{ secret('orders') }}` should resolve to, and substituting a table id
	// there would send the server an id of the wrong primitive rather than a
	// name it refuses.
	toID    map[aliasKey]string
	toAlias map[aliasKey]string
}

// aliasKey is one (dependency kind, name) pair — see aliasCodec's fields.
type aliasKey struct{ kind, name string }

// newAliasCodec builds the folder's codec from what it DECLARES and what the
// selected stack BINDS. An alias that is declared and not bound, or bound and
// not declared, resolves nothing: half a mapping is not a mapping, and the
// refusals in checkAliases are what tell the author which half is missing.
//
// A folder with no dependencies produces an empty codec, and an empty codec is
// the identity — see rewrite. That is what makes this change inert for every
// folder in the field.
func newAliasCodec(deps map[string]wfdir.Dependency, bind map[string]string) aliasCodec {
	c := aliasCodec{}
	if len(deps) == 0 || len(bind) == 0 {
		return c
	}
	c.toID = make(map[aliasKey]string, len(bind))
	c.toAlias = make(map[aliasKey]string, len(bind))
	// Sorted, so a folder that has somehow bound two aliases to one id produces
	// the SAME reverse map on every run rather than whichever name the last map
	// iteration happened to write.
	//
	// Manifest.CheckBind refuses that folder outright, and toDisk is why it does:
	// the reverse direction has to be a function, or reading a live row back into
	// a folder has no answer to "which declaration does this row answer to". The
	// tie-break here is not a substitute for that refusal — it exists because a
	// codec is built on every folder open, including `status`, which deliberately
	// does not hard-refuse and must still be deterministic.
	names := make([]string, 0, len(bind))
	for alias := range bind {
		names = append(names, alias)
	}
	sort.Strings(names)
	for _, alias := range names {
		dep, declared := deps[alias]
		if !declared {
			continue
		}
		key := aliasKey{kind: dep.Kind, name: alias}
		c.toID[key] = bind[alias]
		reverse := aliasKey{kind: dep.Kind, name: bind[alias]}
		if _, taken := c.toAlias[reverse]; !taken {
			c.toAlias[reverse] = alias
		}
	}
	return c
}

// resolve answers one (kind, name) pair directly, for a caller resolving a
// structured FIELD rather than a marker in source.
//
// The automation loop is that caller: its references are JSON fields, not
// markers, so it cannot go through rewrite — and one of the kinds it resolves
// (`note`) has no marker family at all. It reads the same map toWire does, so
// the two directions cannot come to disagree about what an alias means.
func (c aliasCodec) resolve(kind, name string) (string, bool) {
	id, ok := c.toID[aliasKey{kind: kind, name: name}]
	return id, ok
}

// toWire resolves this folder's aliases to ids in content about to be SENT.
//
// A name that is neither bound nor id-shaped is left EXACTLY as found. The
// server refuses it, loudly, on every marker family, and that refusal is older
// and better-aimed than anything invented here — it knows whether the row
// exists, whether the caller can reach it, and what the id was meant to be.
func (c aliasCodec) toWire(content string) string { return c.rewrite(content, c.toID) }

// toDisk replaces ids with the aliases this folder knows them by, in content
// that came FROM the server.
//
// The inverse of toWire, and the reason Manifest.CheckBind refuses two aliases
// bound to one id: without that refusal this direction has no single answer, and
// a remote read would land in the folder under whichever of the two names won a
// map iteration.
func (c aliasCodec) toDisk(content string) string { return c.rewrite(content, c.toAlias) }

// rewrite is the shared half of both directions.
//
// The empty-table short circuit is the load-bearing line: a folder that declares
// no dependencies never scans its own source at all, so its push is byte-identical
// — request for request — to the one it produced before this feature existed.
func (c aliasCodec) rewrite(content string, table map[aliasKey]string) string {
	if len(table) == 0 {
		return content
	}
	return rewriteMarkers(content, func(o markers.Occurrence) string {
		if sub, ok := table[aliasKey{kind: o.Family.Kind, name: o.Arg}]; ok {
			return sub
		}
		return o.Arg
	})
}

// rewriteMarkers applies sub to every alias-eligible marker argument, leaving a
// POSITIONAL ref exactly as it found it.
//
// A positional `{{ ref('0') }}` is an INDEX into the row's input_models, not a
// name, so nothing here may touch it: substituting one would replace an index
// with an id chosen by a completely different rule. tablerefs.Canonicalize is
// the only thing that resolves those, and it runs first — see canonicalCode.
//
// The error markers.Rewrite can return is unreachable through here. It refuses a
// substitution carrying a quote or a brace, and both directions substitute values
// that have already been refused for exactly that: an id, whose shape CheckBind
// asserts, or an alias, whose character set ValidAliasName pins. Returning the
// input unchanged rather than panicking leaves the name in the source, where the
// server refuses it by name.
func rewriteMarkers(content string, sub func(markers.Occurrence) string) string {
	out, err := markers.Rewrite(content, func(o markers.Occurrence) (string, error) {
		if o.Positional {
			return o.Arg, nil
		}
		return sub(o), nil
	})
	if err != nil {
		return content
	}
	return out
}

// accessDependency is one of a data app's id allowlists, seen as a place an
// alias may stand in for an id: the manifest key it is spelled with, the
// dependency kind an entry in it may be an alias OF, and the list itself.
//
// A POINTER to the list, so the one caller that rewrites and the two that only
// read walk the same slot list — the reason api.DataAppAccess.accessSlots is
// shaped this way, and the same hazard: a slot missing from here is a slot that
// silently stops resolving.
type accessDependency struct {
	Key  string
	Kind string
	IDs  *[]string
}

// accessDependencies is every id allowlist of a data app's access block, paired
// with the dependency kind its entries may be aliases of.
//
// Capabilities is absent and never appears here. It is a closed vocabulary of
// switches (checkDeclaredCapabilities owns it), not a list of rows, so there is
// nothing in it an alias could stand for.
//
// ⚠️ METRICS DECLARE `table`, and there is deliberately no `metric` dependency
// kind — there will not be one, and the reasons are worth reading before adding
// it:
//
//   - A dependency kind exists to say WHICH MARKER FAMILY may use an alias (see
//     the markers package doc), and no marker resolves a metric — a metric is
//     queried through queryMetric and is not a `{{ ref }}` target at all. A
//     `metric` kind would be a kind nothing could ever consume, and
//     markers.Kinds() — which checkDependencies validates against and every
//     refusal lists — is derived from the family table, so it could not even be
//     spelled.
//   - A metric IS a `kind='metric'` row in models_v2 and carries a `table-`
//     prefixed id, so it is already inside the id namespace markers.IsResourceID
//     and Manifest.CheckBind test against. Declaring `table` makes those checks
//     right rather than approximately right.
//   - `ronja bind` answers a declaration through GET /api/v2/search, which
//     reports a metric as kind "table" (the frontend has no metric prefix — see
//     backend/lib/search/hit.go). A `metric` kind would match no hit and report
//     "nothing is called that" for a row sitting right there.
//
// What that shares is the alias NAMESPACE, not the grant: an alias declared as
// `table` may be listed under either allowedTableIDs or allowedMetricIDs, and
// governance.ValidateDataAppScope refuses each slot against the row's actual
// models_v2 kind — symmetrically, a metric in the table slot and a plain table
// in the metric slot — so a name in the wrong list is refused there exactly as
// its id would have been.
//
// ⚠️ That server refusal is the ONLY thing separating the two. GET /api/v2/search
// carries no field saying "this is a metric" — search.Hit.IsMetric is
// `json:"-"` on purpose, since Spotlight emits Kind=="table" for metrics — so
// `ronja bind` cannot tell a metric from a plain table of the same name and will
// bind an exact-name match of either. The mistake surfaces on the push, by name,
// which is the right place for it; there is nothing this side of the wire that
// could catch it earlier.
func accessDependencies(a *api.DataAppAccess) []accessDependency {
	return []accessDependency{
		{"allowedTableIDs", markers.KindTable, &a.AllowedTableIDs},
		{"allowedSecretIDs", markers.KindSecret, &a.AllowedSecretIDs},
		{"allowedAgentIDs", markers.KindAgent, &a.AllowedAgentIDs},
		{"allowedWorkflowIDs", markers.KindWorkflow, &a.AllowedWorkflowIDs},
		{"allowedCodexIDs", markers.KindCodex, &a.AllowedCodexIDs},
		{"allowedMetricIDs", markers.KindTable, &a.AllowedMetricIDs},
	}
}

// accessToWire resolves this folder's aliases to ids in a data app's ACCESS
// block — the dependencies that are not in the folder's content.
//
// toWire's counterpart, and it carries the same contract entry for entry: an
// entry that is neither a declared alias of that slot's kind nor id-shaped is
// left EXACTLY as found, because the server refuses it and knows things this
// program does not. The kind is part of the lookup for the reason aliasCodec's
// fields record — a `secret` alias listed under allowedTableIDs resolves
// nothing, rather than granting a secret's id as a table.
//
// Normalized on the way out, so the sort that decides every downstream
// comparison happens AFTER the substitution. DeclaredAccess sorts by whatever
// the manifest spells — alias names — and comparing that order against the
// server's id order is the spurious diff this whole function exists to remove.
//
// There is NO accessToDisk, and the asymmetry with content is deliberate rather
// than unfinished. Content needs the inbound direction because remote content is
// HASHED against the bytes on disk (see this file's opening comment); an access
// set is never compared against the manifest's spelling in the other direction:
// its only inbound sinks are the drift baseline in .ronja/, which records what
// the SERVER held and is compared only against the server's own row
// (appBaselineDescribes), and `app clone`, which refuses a non-empty destination
// and so writes into a folder that declares nothing. Should anything ever
// compare a remote access set against the manifest's, that is the half to write
// first.
func (c aliasCodec) accessToWire(a api.DataAppAccess) api.DataAppAccess {
	// The same short circuit rewrite makes, for the same reason: a folder that
	// declares no dependencies produces the request it produced before this
	// feature existed, byte for byte, by construction rather than by the
	// substitution happening to be faithful.
	if len(c.toID) == 0 {
		return a
	}
	out := a
	for _, dep := range accessDependencies(&out) {
		*dep.IDs = c.resolveAccessIDs(dep.Kind, *dep.IDs)
	}
	return out.Normalized()
}

// resolveAccessIDs substitutes one allowlist's declared aliases for their ids.
//
// Always a FRESH slice when there is anything to walk: `out` above is a shallow
// copy of the caller's access set, so writing through its slice headers would
// rewrite the manifest's own lists in place — and the manifest is what `status`
// and every later reader still needs in disk form.
func (c aliasCodec) resolveAccessIDs(kind string, ids []string) []string {
	if len(ids) == 0 {
		return ids
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id
		if sub, bound := c.toID[aliasKey{kind: kind, name: id}]; bound {
			out[i] = sub
		}
	}
	return out
}

// bindTargets is every id this folder binds an alias to, id → the alias.
//
// Used by the literal-id refusal below, which is about the id whichever kind
// declared it — a source that writes `table-abc` literally is drifted whether
// `abc` was declared as a table or as anything else.
func (c aliasCodec) bindTargets() map[string]string {
	if len(c.toAlias) == 0 {
		return nil
	}
	out := make(map[string]string, len(c.toAlias))
	for key, alias := range c.toAlias {
		out[key.name] = alias
	}
	return out
}

// pipelineCodec is aliasCodec plus the names a PIPELINE folder's own files
// claim, and it is the one place the alias layer is more than an edge
// translation.
//
// A `{{ ref('orders') }}` in a pipeline folder may name a SIBLING orders.sql
// rather than a declared dependency, and the SIBLING WINS — the file you can see
// beats a declaration you have to scroll for. wfdir.CheckAliasCollisions refuses
// a dependency declared under a stem's name, so the two can never both answer
// for one word.
//
// The sibling half cannot be resolved up front. On a FIRST push the sibling's
// table does not exist yet: its id arrives only when its own create returns. So
// toWire reads the binding LIVE, inside the topo loop, and topoOrder — which
// derives its edges from folderUpstreams, stem refs included — is what
// guarantees the file a ref names has already been pushed.
type pipelineCodec struct {
	f *folder
	// pathByStem maps a stem to the folder file that claims it, first in path
	// order. Fixed for the run, since it comes from the files on disk.
	//
	// ⚠️ Only the KEYS of the map handed to newPipelineCodec are read, which is
	// what lets `status` build one from a wfdir.Enumeration (whose values are
	// hashes, not content) without reading every file a second time.
	pathByStem map[string]string
	// ambiguous is the stems TWO files claim — `staging/orders.sql` and
	// `raw/orders.sql` — stem → both paths, so the refusal can name them. A ref
	// naming one is refused rather than resolved to whichever file sorted first:
	// a folder can legally hold two files of one stem (they create two tables of
	// one name, which the schema permits), and picking silently would build the
	// wrong table with no diff to look at.
	ambiguous map[string][]string
}

// newPipelineCodec reads the local names a pipeline folder's files claim. Only
// the KEYS of `local` are used — see pathByStem.
func newPipelineCodec(f *folder, local map[string]string) pipelineCodec {
	c := pipelineCodec{f: f, pathByStem: make(map[string]string, len(local))}
	for _, path := range sortedPaths(local) {
		stem := tableNameFor(path)
		first, taken := c.pathByStem[stem]
		if !taken {
			c.pathByStem[stem] = path
			continue
		}
		if c.ambiguous == nil {
			c.ambiguous = map[string][]string{}
		}
		if len(c.ambiguous[stem]) == 0 {
			c.ambiguous[stem] = []string{first}
		}
		c.ambiguous[stem] = append(c.ambiguous[stem], path)
	}
	return c
}

// toWire resolves one pipeline file's names to ids: a sibling's stem first, then
// a declared alias. `path` is the file being resolved, which is what lets a
// self-reference be named as one.
//
// The error is per-file and the caller refuses that file with it. Three of the
// four are about what the author wrote; the last is about this program:
//
//   - an AMBIGUOUS stem, which no answer here can disambiguate;
//   - a SELF-reference, which is a cycle of one and which the create could not
//     resolve in any case, since the table does not exist while it is being made;
//   - ONE SIBLING SPELLED TWO WAYS in one file — see spelledTwoWays;
//   - a sibling whose table id is not recorded yet, which means the push reached
//     a downstream file before its upstream. That is topoOrder failing to do the
//     one thing it exists for, so it is reported as a bug rather than dressed up
//     as something the author can fix.
func (c pipelineCodec) toWire(path, content string) (string, error) {
	var failure error
	fail := func(format string, args ...any) {
		if failure == nil {
			failure = fmt.Errorf(format, args...)
		}
	}
	// Which sibling each spelling in THIS file names, and how it was spelled the
	// first time — the state spelledTwoWays needs. Built here rather than in a
	// second scan so the refusal sees exactly the occurrences the substitution
	// does, in the same order.
	byID := tablePaths(c.f.Binding)
	spelledAs := map[string]string{}
	out := rewriteMarkers(content, func(o markers.Occurrence) string {
		if o.Family.Kind == markers.KindTable {
			if sibling, claimed := c.pathByStem[o.Arg]; claimed {
				switch {
				case len(c.ambiguous[o.Arg]) > 0:
					fail("%s reads %q, and two files in this folder are called %q (%s) — rename one of them, or write the table's id",
						path, o.Marker, o.Arg, strings.Join(c.ambiguous[o.Arg], " and "))
				case sibling == path:
					fail("%s reads %q, which names this same file — a derived table cannot read from itself", path, o.Marker)
				default:
					if first, twice := spelledTwoWays(spelledAs, sibling, o.Arg); twice {
						fail("%s", spelledTwoWaysMessage(path, sibling, first, o.Arg))
						return o.Arg
					}
					if id := c.f.Binding.Tables[sibling]; id != "" {
						return id
					}
					fail("%s reads %q and no table id is recorded for %s yet, so this push reached it before its upstream. That is a bug in ronja (topoOrder should have ordered %s first), not something wrong with your folder",
						path, o.Marker, sibling, sibling)
				}
				return o.Arg
			}
			// Not a stem, but possibly this same folder's table written as its
			// literal id — the other half of the pair spelledTwoWays refuses.
			if sibling, inFolder := byID[o.Arg]; inFolder && sibling != path {
				if first, twice := spelledTwoWays(spelledAs, sibling, o.Arg); twice {
					fail("%s", spelledTwoWaysMessage(path, sibling, first, o.Arg))
				}
				return o.Arg
			}
		}
		if id, bound := c.f.Codec.toID[aliasKey{kind: o.Family.Kind, name: o.Arg}]; bound {
			return id
		}
		return o.Arg
	})
	if failure != nil {
		return "", failure
	}
	return out, nil
}

// spelledTwoWays records how this file spells one sibling and reports the second,
// different spelling of the same one.
//
// WITHOUT THIS REFUSAL the folder can never be clean again, and there is nothing
// the author can edit that fixes it — the same permanent-drift failure
// literalBindTargets exists for, arrived at from the other direction. A pipeline
// sibling's table id has two legitimate disk spellings (its stem and its literal
// id, see pipelineCodec.disk), and toWire collapses both to the id, so the
// server's copy holds ONE spelling for the pair. Coming back, spellings maps that
// id to whichever spelling this file used — one value per id, because the reverse
// of a many-to-one is not a function — and disk rewrites BOTH occurrences to it.
// The literal one then reads back as a name the file does not contain: drift on
// every `pipeline status`, reproduced by every push.
//
// literalBindTargets does not cover it and cannot: it is about a declared alias's
// BIND TARGET, and a sibling's stem is not one — nothing in `bind` names it.
func spelledTwoWays(spelledAs map[string]string, sibling, spelling string) (first string, twice bool) {
	first, seen := spelledAs[sibling]
	if !seen {
		spelledAs[sibling] = spelling
		return "", false
	}
	return first, first != spelling
}

// spelledTwoWaysMessage is the refusal spelledTwoWays earns, worded once because
// the two call sites reach it from either spelling.
func spelledTwoWaysMessage(path, sibling, first, second string) string {
	return fmt.Sprintf("%s names %s both as %q and as %q, and the server stores one id for the pair — so its copy reads back as a single spelling and this file reads as drifted for ever, with nothing to edit that fixes it. Spell it the same way in both places",
		path, sibling, first, second)
}

// disk replaces ids with the names ONE LOCAL FILE spells them by, in the remote
// code that file is about to be compared against.
//
// Per-file, and that is the whole subtlety of the pipeline half. A sibling's
// table id has TWO legitimate disk spellings — the id, which every pipeline
// folder written before this slice uses and which the command help still
// documents, and the stem — so "what does table-a read back as" is not a
// property of the folder at all. It is a property of the file: whichever
// spelling that file uses is the one its own hash was taken from, and the one
// the de-alias has to reproduce.
//
// A folder-wide answer was tried first and is wrong in the expensive direction:
// mapping every sibling id to its stem turned every existing pipeline folder's
// id-form refs into names, and each one read as drift on a table nobody had
// touched.
//
// The ALIAS half is folder-wide and needs no such care, because
// literalBindTargets refuses a source that writes a bind target's id literally —
// so for a declared alias there is only ever one disk spelling.
func (c pipelineCodec) disk(local, remote string) string {
	stems := c.spellings(local)
	if len(stems) == 0 && len(c.f.Codec.toAlias) == 0 {
		return remote
	}
	return rewriteMarkers(remote, func(o markers.Occurrence) string {
		if o.Family.Kind == markers.KindTable {
			if stem, ok := stems[o.Arg]; ok {
				return stem
			}
		}
		if alias, ok := c.f.Codec.toAlias[aliasKey{kind: o.Family.Kind, name: o.Arg}]; ok {
			return alias
		}
		return o.Arg
	})
}

// spellings reads one local file for the SIBLING STEMS it uses, mapped to the
// ids those stems resolve to — the exact inverse of what toWire did to this same
// file, and nothing wider.
//
// Read LIVE off the binding, so a table this push has just created is spelled
// correctly for every file after it in the same run. A stem with no id yet
// contributes nothing: there is no remote code to de-alias for a table that does
// not exist.
//
// ⚠️ ONE ENTRY PER ID, necessarily — `out` is keyed by id — and that is why
// pipelineCodec.toWire refuses a file that spells one sibling two ways. Two
// spellings for one id make this map a choice rather than an inverse, and
// whichever one it kept would rewrite BOTH occurrences in the remote copy: the
// other would read back as a word the file does not contain, and the file would
// be drifted for ever with nothing to edit that fixes it. The refusal is what
// keeps this a function; do not "fix" a collision here.
func (c pipelineCodec) spellings(local string) map[string]string {
	out := map[string]string{}
	for _, o := range markers.Scan(local) {
		if o.Positional || o.Family.Kind != markers.KindTable {
			continue
		}
		sibling, claimed := c.pathByStem[o.Arg]
		if !claimed || len(c.ambiguous[o.Arg]) > 0 {
			continue
		}
		if id := c.f.Binding.Tables[sibling]; id != "" {
			out[id] = o.Arg
		}
	}
	return out
}

// canonicalDisk is Table.CanonicalCode plus the de-alias step every remote read
// owes this folder, and the ORDER is the whole of it.
//
// Canonicalize runs FIRST, and runs on ID form, because that is the only form it
// can read: a positional `{{ ref('2') }}` resolves to input_models[2], and
// tablerefs.IsTableID then gates whether the substitution happens at all.
// De-aliasing first would leave the positional ref pointing into an unchanged
// input_models while the ids around it had become names — a file in two
// vocabularies at once, hashed against neither.
//
// `local` is the folder file this row is being compared against, for the reason
// disk records. An empty one de-aliases declared aliases and no stems, which is
// the honest answer when the file cannot be read.
func (c pipelineCodec) canonicalDisk(local string, t *api.Table) (string, bool) {
	code, unresolved := t.CanonicalCode()
	return c.disk(local, code), unresolved
}

// folderUpstreams names the files in THIS folder that `code` reads from, by
// either spelling: an id ref bound to a sibling, or a sibling's stem.
//
// Both spellings in one function, because topoOrder depends on it: a first push
// that builds a downstream before its upstream exists produces a confidence
// report describing a state that never existed — the failure topoOrder's own doc
// names — and a stem ref is exactly the edge a folder written against names is
// made of.
func (c pipelineCodec) folderUpstreams(code string) []string {
	byID := tablePaths(c.f.Binding)
	seen := map[string]bool{}
	var out []string
	for _, o := range markers.Scan(code) {
		if o.Family.Kind != markers.KindTable || o.Positional {
			continue
		}
		path := ""
		if sibling, claimed := c.pathByStem[o.Arg]; claimed && len(c.ambiguous[o.Arg]) == 0 {
			path = sibling
		} else if inFolder, ok := byID[o.Arg]; ok {
			path = inFolder
		}
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// aliasReport is what the alias pre-flight found, split by what a command should
// do with it.
//
// Two lists rather than one error, because the two audiences differ. A PUSH must
// stop on a Refusal — it is about to send a source the server cannot resolve, or
// to record a baseline that will read as drifted for ever. A `status` or a
// `validate` must REPORT the same thing and carry on: a folder mid-promotion,
// with its declarations merged and its binds not yet filled in, has to stay
// inspectable. That is the same "keep the folder openable" discipline
// wfdir.checkStacks documents, applied one layer up.
type aliasReport struct {
	// Refusals are the states a push must not proceed from.
	Refusals []string
	// Warnings are dead config: true, worth saying, and no reason to stop.
	Warnings []string
}

// err folds the refusals into the one error a push returns, or nil.
func (r aliasReport) err() error {
	switch len(r.Refusals) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("%s", r.Refusals[0])
	default:
		return fmt.Errorf("%s", strings.Join(r.Refusals, "\n  "))
	}
}

// fieldRef is one alias-eligible value written in a STRUCTURED FIELD rather
// than in a source marker — an automation's `references[].resourceID`,
// `action.config.workflowID` and the rest.
//
// It exists because the two legs of the pre-flight that read a folder's SOURCE
// are blind to such a folder: markers.Scan finds nothing in an automation's
// JSON, so `unusedAliases` would report every dependency the folder actually
// uses as dead config, and `literalBindTargets` would miss an id a bind is
// already pointed at. Both are answered from this list instead, which the
// automation loop derives from the same field enumeration its resolver walks.
type fieldRef struct {
	// Where names the file and field, for the message: "nightly.json →
	// references[2].resourceID".
	Where string
	// Kind is the DEPENDENCY kind the value names, empty for a reference kind
	// the alias layer cannot express (resolveAutomationRefs refuses those, and
	// they cannot be a use of any declared alias).
	Kind  string
	Value string
}

// checkAliases is the whole alias pre-flight, and it runs BEFORE any network:
// every question it asks is answered from the manifest, the selected stack and
// the files on disk.
//
// `files` is the folder's source in DISK form. `stems` are the local names the
// folder's own files claim — the .sql stems for a pipeline, the .json stems for
// an automation folder, and nothing for the two kinds whose files claim none.
// `fields` are the alias-eligible values written in a folder's structured
// FIELDS rather than in its source, which is the only thing an automation
// folder has: see fieldRef above and folderFieldRefs, which derives them.
func checkAliases(m *wfdir.Manifest, sel wfdir.Selection, codec aliasCodec, files map[string]string, stems []string, fields []fieldRef) aliasReport {
	var out aliasReport
	where, fix := describeBindSite(sel)

	// The DECLARATION rules this build can never have written: `dependencies`
	// declared below the format version that means them, and a kind this build
	// does not resolve. First, because both say the manifest cannot be read as
	// the folder intends, which makes every message after them a guess. See
	// Manifest.CheckDeclarations for why neither is at load.
	if err := m.CheckDeclarations(); err != nil {
		out.Refusals = append(out.Refusals, err.Error())
	}

	// The cross-object bind rules, at the ACCEPTANCE point they were written for
	// — a bind with no declaration, a bind value that is not an id, and two
	// aliases on one id. See Manifest.CheckBind: refusing these at LOAD would make
	// a folder a git merge produced unopenable rather than un-pushable.
	if err := m.CheckBind(sel); err != nil {
		out.Refusals = append(out.Refusals, err.Error())
	}

	// Every DECLARED alias must be bound here. Answered from the manifest alone,
	// so it says nothing about where the alias is used and has no opinion about
	// commented-out code — which is exactly the property markers.Scan's doc asks
	// a caller to have before it turns an unresolvable name into an error.
	for _, alias := range m.AliasNames() {
		// Key PRESENT is enough, even with an empty value: a bind of "" is
		// CheckBind's refusal to make ("not a table id"), and two messages about
		// one broken line is how a reader learns to skim them.
		if _, bound := sel.Bind[alias]; bound {
			continue
		}
		out.Refusals = append(out.Refusals, fmt.Sprintf(
			"%s declares the dependency %q and %s binds nothing to it — an alias only resolves to the id THIS organization uses for it, and there is none. %s, or drop it from \"dependencies\"",
			wfdir.ManifestName, alias, where, fix))
	}

	// A declared alias that collides with a sibling's stem. The file wins, so the
	// declaration is dead text that reads as if it were in force — refused where
	// the name was invented rather than shadowed where it is used.
	if err := wfdir.CheckAliasCollisions(m.Dependencies, stems); err != nil {
		out.Refusals = append(out.Refusals, err.Error())
	}

	// The manifest's `access` block is scanned alongside the folder's source,
	// because a data app's dependencies are not all in its content: an entry
	// there is an id belonging to one organization exactly as a marker argument
	// is. Zero-valued and inert for the two kinds that have no access block.
	access := m.DeclaredAccess()

	out.Refusals = append(out.Refusals, literalBindTargets(codec, files, access, fields)...)
	out.Refusals = append(out.Refusals, misusedAliases(m, files)...)
	out.Warnings = append(out.Warnings, unusedAliases(m, files, access, fields)...)
	return out
}

// misusedAliases refuses a marker argument this folder's own declarations say is
// wrong, in the two cases where nothing else would ever notice.
//
// The codec keys on (kind, name), which is right — a name means nothing on its
// own — but a MISS is silent by construction: the argument is left exactly as
// found, and the folder pushes. These two legs are what turns the two misses
// that are unambiguously mistakes into refusals.
//
//   - WRONG FAMILY, ANY KIND. `orders` is declared as a `table`, the code writes
//     `{{ secret('orders', 'token') }}`, and every existing check passes:
//     CheckBind sees a declared alias bound to a table id, literalBindTargets
//     sees no literal id, and unusedAliases sees the table-kind key being used
//     by the ref that also names it. The secret marker ships the literal string
//     `orders`. Two declared kinds for one word is not a judgement call — one of
//     them is a typo — so the message names both and stops.
//   - AN UNRESOLVABLE ARGUMENT, `secret` ONLY. Deliberately asymmetric, and the
//     asymmetry is the whole reason this leg exists: the secret families are the
//     ONLY ones where the server SOFT-FAILS an unresolvable id into a warning
//     (rworkflow's secretIDs / querySecretIDs legs are SeverityWarning; ref,
//     write, agent, codex, mailbox and workflow are all SeverityError). Every
//     other family hard-rejects the save, and that refusal is older and
//     better-aimed than anything invented here — it knows whether the row
//     exists and whether the caller can reach it — so leaving those alone is the
//     existing, deliberate choice this file's toWire records. A secret's warning
//     is not a refusal, so a folder deploys green with nothing bound and fails
//     at run time; that is the gap, and this closes exactly it.
//
// GATED on the folder declaring dependencies at all, which keeps the alias layer
// inert for every folder in the field: a folder that declares nothing has no
// alias for a marker to be confused with, and refusing its `{{ secret('name') }}`
// would be a new opinion about source the server has always accepted with a
// warning — a change to folders this feature was never about.
//
// ⚠️ markers.Scan reads source as TEXT and cannot see that the server ignores a
// marker inside a `#` comment or a triple-quoted block, so a commented-out
// marker trips these refusals. Accepted rather than mirrored: the alternative is
// a second copy of stripLineComments and stripTripleQuotedBlocks whose drift
// from the server's would be silent in both directions, and deleting a dead line
// is a real fix. Both messages say so.
func misusedAliases(m *wfdir.Manifest, files map[string]string) []string {
	if len(m.Dependencies) == 0 {
		return nil
	}
	const deadCodeNote = " (markers are read as text here, so a commented-out one still counts — delete the dead line if that is what this is)"
	var out []string
	for _, path := range sortedPaths(files) {
		seen := map[aliasKey]bool{}
		for _, o := range markers.Scan(files[path]) {
			key := aliasKey{kind: o.Family.Kind, name: o.Arg}
			if o.Positional || seen[key] || markers.IsResourceID(o.Family.Kind, o.Arg) {
				continue
			}
			seen[key] = true
			if dep, declared := m.Dependencies[o.Arg]; declared && dep.Kind != o.Family.Kind {
				out = append(out, fmt.Sprintf(
					"%s writes %q, and %s declares %q as a %s, not a %s — so this marker resolves nothing and the literal name %q is what the server receives. Declare a %s dependency for it under another name, or fix the marker%s",
					path, o.Marker, wfdir.ManifestName, o.Arg, dep.Kind, o.Family.Kind, o.Arg, o.Family.Kind, deadCodeNote))
				continue
			}
			if o.Family.Kind != markers.KindSecret {
				continue
			}
			out = append(out, fmt.Sprintf(
				"%s writes %q, and %q is neither a secret id nor a secret %s declares — and a secret is the one marker the server SOFT-FAILS into a warning instead of refusing the save, so this would push green and run with nothing bound. Declare it (\"%s\": {\"kind\": \"secret\"}) and bind it, or write the secret's id%s",
				path, o.Marker, o.Arg, wfdir.ManifestName, o.Arg, deadCodeNote))
		}
	}
	return out
}

// literalBindTargets refuses a source that writes, LITERALLY, an id some alias
// is bound to.
//
// It has to be a refusal rather than a warning because the folder it describes
// can never be clean. The remote read de-aliases that id back to the alias, the
// local file keeps the id, and the two hashes differ for ever — so the file
// reads as drifted on every status and is re-pushed by every push, with nothing
// the author can edit that fixes it except the change this message asks for.
//
// Scanned over the WHOLE folder, not over the subset a narrowed push is sending,
// which is the opposite of refusePositionalRefs and deliberately so: that one is
// about what a REQUEST would carry, and a file nobody is sending carries nothing.
// This is about the folder's CONFIG contradicting its source, which is true of
// the folder whichever file is named on the command line, and the fix is one
// word.
//
// `access` is a data app's allowlist block, scanned on the same rule and against
// the same targets map — one definition of "a bind target", so the two legs
// cannot disagree about what one is. The DAMAGE differs, though, and the message
// says so: a source that writes a bind target's id reads as drifted for ever,
// while an allowlist that writes one is silently NON-PORTABLE — nothing drifts,
// because an id compares equal to itself, and pushing the folder to a second
// stack simply grants that stack the first organization's id. There is no
// symptom at all until somebody opens the app there, which is the more expensive
// of the two failures and the reason this is a refusal rather than a note.
func literalBindTargets(codec aliasCodec, files map[string]string, access api.DataAppAccess, fields []fieldRef) []string {
	targets := codec.bindTargets()
	if len(targets) == 0 {
		return nil
	}
	var out []string
	for _, dep := range accessDependencies(&access) {
		for _, id := range *dep.IDs {
			alias, bound := targets[id]
			if !bound {
				continue
			}
			out = append(out, fmt.Sprintf(
				"%s lists %q under \"access\".%q, and binds %q to that id — so this folder grants one organization's row by id, and pushed to another stack it would grant an id nobody there has. Use the alias: %q",
				wfdir.ManifestName, id, dep.Key, alias, alias))
		}
	}
	// A structured FIELD carrying a bind target's id — an automation's
	// references, its watched tables, the workflow its action runs. Same damage
	// as the access block's, and stated the same way: nothing drifts, because an
	// id compares equal to itself, so the folder is silently non-portable and
	// there is no symptom at all until somebody pushes it to the second stack.
	for _, field := range fields {
		alias, bound := targets[field.Value]
		if !bound {
			continue
		}
		out = append(out, fmt.Sprintf(
			"%s writes %q, and %s binds %q to that id — so this folder names one organization's row by id, and pushed to another stack it would name an id nobody there has. Use the alias: %q",
			field.Where, field.Value, wfdir.ManifestName, alias, alias))
	}
	for _, path := range sortedPaths(files) {
		seen := map[string]bool{}
		for _, o := range markers.Scan(files[path]) {
			if o.Positional || seen[o.Arg] {
				continue
			}
			alias, bound := targets[o.Arg]
			if !bound {
				continue
			}
			seen[o.Arg] = true
			out = append(out, fmt.Sprintf(
				"%s writes %q, and %s binds %q to that id — so the server's copy reads back as %q while your file keeps the id, and this folder reads as drifted for ever with nothing to edit that fixes it. Use the alias: %q",
				path, o.Marker, wfdir.ManifestName, alias, alias,
				strings.Replace(o.Marker, o.Arg, alias, 1)))
		}
	}
	return out
}

// unusedAliases names a declared dependency no source in the folder mentions.
//
// A WARNING and not a refusal: it is dead config, not a broken deploy. Nothing
// resolves through it, nothing is sent wrong because of it, and a folder that
// declares an alias one commit before the code that uses it is an ordinary state
// to be in for an afternoon.
//
// ⚠️ "Mentions" is markers.Scan's answer, which reads the source as TEXT and does
// not model dead code — so an alias used only inside a commented-out line counts
// as used. That is the right direction for a warning to be wrong in: staying
// quiet about a name that IS written down costs nothing, and accusing one that is
// there in front of the reader is what trains people past the whole line.
//
// A data app's `access` block counts as a use, and has to: an alias declared
// purely so the allowlist can name it is used by the folder in every sense that
// matters, and reporting it as dead config on every push would be the warning
// crying wolf at the one kind that needs it most.
func unusedAliases(m *wfdir.Manifest, files map[string]string, access api.DataAppAccess, fields []fieldRef) []string {
	if len(m.Dependencies) == 0 {
		return nil
	}
	used := map[aliasKey]bool{}
	// An automation folder's uses are all here and NONE of them are in the
	// source: markers.Scan reads a .json file and finds nothing, so without this
	// leg every dependency such a folder declares would be reported as dead
	// config on every push and status — the warning crying wolf at the one kind
	// that has no other way to say what it uses.
	for _, field := range fields {
		used[aliasKey{kind: field.Kind, name: field.Value}] = true
	}
	for _, path := range sortedPaths(files) {
		for _, o := range markers.Scan(files[path]) {
			if !o.Positional {
				used[aliasKey{kind: o.Family.Kind, name: o.Arg}] = true
			}
		}
	}
	for _, dep := range accessDependencies(&access) {
		for _, id := range *dep.IDs {
			used[aliasKey{kind: dep.Kind, name: id}] = true
		}
	}
	var out []string
	for _, alias := range m.AliasNames() {
		if used[aliasKey{kind: m.Dependencies[alias].Kind, name: alias}] {
			continue
		}
		out = append(out, fmt.Sprintf("%s declares the dependency %q (%s) and no file in this folder uses it",
			wfdir.ManifestName, alias, m.Dependencies[alias].Kind))
	}
	return out
}

// noteAliases runs the alias pre-flight for a READ-ONLY command and reports what
// it found.
//
// It costs a second read of the folder's source, so it is skipped outright for a
// folder that declares no dependencies — which is every folder that existed
// before this feature, and the reason a `status` on one is unchanged down to the
// syscall. A read that fails is SILENT: `status` is the command that has to keep
// answering, and a folder whose files cannot be walked has a problem the rest of
// the report is about to name properly.
func (f *folder) noteAliases() {
	report, _ := f.aliasReport()
	noteAliasReport(report)
}

// aliasReport runs the alias pre-flight over the folder's own files, for a
// command that needs the findings rather than just the printing.
//
// An empty report for a folder that declares no dependencies — see noteAliases.
//
// THE ERROR IS THE VERDICT for a folder whose files cannot be read, and it is
// returned rather than swallowed because a caller that gates on this report has
// to fail CLOSED. An empty report is indistinguishable from a clean one, and
// `ronja app validate` — the command a CI job runs INSTEAD of the push — turns a
// clean one into `ok: true`. `warnIfAppDirty` happens to re-walk the same tree
// two lines later and catches most causes today, but "some other call errors
// first" is an accident of ordering, not a property, and it is not the kind of
// thing to leave a green CI verdict resting on. A `status` still ignores it (see
// noteAliases): it refuses nothing, and the rest of its report is about to name
// the unreadable folder properly.
func (f *folder) aliasReport() (aliasReport, error) {
	if len(f.Manifest.Dependencies) == 0 {
		return aliasReport{}, nil
	}
	files, _, err := readLocalFiles(f.Root, f.Kind)
	if err != nil {
		return aliasReport{}, fmt.Errorf("read this folder's files to check its dependency names: %w", err)
	}
	return checkAliases(f.Manifest, f.selection(), f.Codec, files, folderStems(f.Kind, files), folderFieldRefs(f.Kind, f.Root, files)), nil
}

// folderStems is the local names a folder's own files claim.
//
// For a PIPELINE that is each .sql file's stem, and it is derived through
// tableNameFor rather than re-derived here: that function is what a create
// actually names the table, and a second spelling of "the stem" would disagree
// with it about a file with two dots in its name.
//
// AN AUTOMATION FOLDER claims them too — each .json file's stem is the name the
// automation it creates carries — and it goes through automationNameFor for the
// same reason: that is what the create actually names the row.
//
// Workflows and data apps claim no local names at all, so nothing there can
// shadow a declaration.
func folderStems(kind wfdir.Kind, files map[string]string) []string {
	var name func(string) string
	switch kind.Name {
	case wfdir.KindPipeline:
		name = tableNameFor
	case wfdir.KindAutomation:
		name = automationNameFor
	default:
		return nil
	}
	out := make([]string, 0, len(files))
	for _, path := range sortedPaths(files) {
		out = append(out, name(path))
	}
	return out
}

// folderFieldRefs is folderStems' counterpart for the OTHER thing a structured
// folder claims: the alias-eligible values written in its fields.
//
// Empty for every kind whose references live in source markers, which is what
// keeps the two legs that read it (unusedAliases, literalBindTargets) inert for
// every folder that existed before this kind did.
//
// A file that does not parse contributes nothing rather than failing: the
// pre-flight is run by `status` and by `sync check`, which have to keep
// answering, and the loop's own parse refusal is the better message for that
// file anyway.
//
// It takes the folder ROOT as well as its syncable files because a PIPELINE
// folder's second claim is a FILE NAME rather than a field: a docs sidecar's
// stem is the alias it documents, and those files are not in `files` at all —
// the kind's SyncExt is ".sql", so Enumerate never sees them. Without this leg
// a folder that declares a dependency purely in order to document a table it
// does not build would have that dependency reported as dead config on every
// command.
func folderFieldRefs(kind wfdir.Kind, root string, files map[string]string) []fieldRef {
	if kind.Name == wfdir.KindPipeline {
		// TWO carriers, both of which name aliases as FILE NAMES or as JSON
		// values rather than as markers, so markers.Scan finds nothing in either
		// and every dependency declared for one would otherwise be reported as
		// dead config on every push, status and `sync check`.
		return append(tableDocsFieldRefs(root), metricFieldRefs(root)...)
	}
	if kind.Name != wfdir.KindAutomation {
		return nil
	}
	var out []fieldRef
	for _, path := range sortedPaths(files) {
		file, err := parseAutomationFile(path, files[path])
		if err != nil {
			continue
		}
		for _, field := range file.refFields() {
			out = append(out, fieldRef{
				Where: path + " → " + field.Where, Kind: field.Kind, Value: field.Value,
			})
		}
	}
	return out
}

// describeBindSite names the entry a bind belongs to and the command that
// writes one, which is not the same sentence for the two manifest shapes: a
// legacy unnamed instances[] entry has no name to pass to --stack.
func describeBindSite(sel wfdir.Selection) (where, fix string) {
	if sel.Named() {
		return fmt.Sprintf("stack %q", sel.Name),
			fmt.Sprintf("Run `ronja bind --stack %s`", sel.Name)
	}
	return "the unnamed \"instances\" entry", "Run `ronja bind`"
}

// noteAliasWarnings prints the pre-flight's warnings once, on stderr, wherever a
// command has just run it.
func noteAliasWarnings(r aliasReport) {
	for _, warning := range r.Warnings {
		fmt.Fprintf(os.Stderr, "  Note: %s.\n", warning)
	}
}

// noteAliasReport is what a READ-ONLY command does with the pre-flight: says
// everything, refuses nothing.
//
// A folder mid-promotion — declarations merged, binds not yet filled in — has to
// stay inspectable, and `status` is the command you run when you are already
// suspicious. Refusing there would take away the one report that could explain
// the folder, which is the same trade openFolderForStatus makes about a dead
// token.
func noteAliasReport(r aliasReport) {
	for _, refusal := range r.Refusals {
		fmt.Fprintf(os.Stderr, "  Warning: %s.\n", refusal)
	}
	noteAliasWarnings(r)
}

// noteAliasErrors is what a VALIDATE does with the pre-flight: says everything,
// and labels a refusal as the error it is about to fail on.
//
// The word matters more than it looks. `validate` is the command a CI job gates
// on, and its verdict is the one a person trusts instead of running the push —
// so a refusal printed as "Warning" beside an exit code of 1 reads as a
// mislabelled warning, and the reader goes looking for the error that is not
// there. A `status` keeps the softer word because it refuses nothing.
func noteAliasErrors(r aliasReport) {
	for _, refusal := range r.Refusals {
		fmt.Fprintf(os.Stderr, "  Error: %s.\n", refusal)
	}
	noteAliasWarnings(r)
}
