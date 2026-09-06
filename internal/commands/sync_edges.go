package commands

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The verdict vocabulary for ONE EDGE — one reference a folder's committed
// content makes to a row that exists in exactly one organization.
//
// Four values, and the third is the one worth reading twice.
//
//	ok           resolved and readable                        — decided by the server
//	unresolved   nothing here could turn the reference into
//	             an id at all: an alias this stack binds
//	             nothing to, a sibling stem no file claims, an
//	             ambiguous one                                 — decided LOCALLY, definite
//	unreachable  a well-formed id the server will not show us  — decided by the server
//	not_checked  with a reason                                 — nobody asked
//
// ⚠️ `unreachable` CANNOT BE SPLIT, and a verifier that tried would be wrong in
// both directions. A row the caller may not see answers 404 deliberately so
// that existence does not leak — the workflow handler's own comment calls it the
// "no-enumeration 404" — while a row that is deleted, trashed or in another
// tenant is removed by RLS or by the store's live pin before the predicate ever
// runs, and surfaces as table.ErrNoRows. That sentinel changed status under us:
// older backends still in the field answer it `400 {"error":"no rows"}`, current
// ones `404 {"error":"not found"}`, and this binary talks to both. So "deleted"
// and "not yours" are indistinguishable BY DESIGN, and the two statuses do not
// even line up with the two meanings — nor with each other across generations.
//
// That is why the id's SHAPE is checked locally first (markers.IsResourceID,
// which takes a kind), and why 400 and 404 on a well-formed id are then treated
// IDENTICALLY — which is also why the 400 arm STAYS rather than being retired
// with the retype: it is what an older instance still answers. A verifier keyed
// on 404 alone would read a deleted table on such an instance as "my request was
// malformed" and say nothing; one that read 400 as a client bug would do the
// same. See Client.GetTable, which already states the half of this the CLI knew
// about: "404s for an id the caller cannot reach — the read gate does not
// distinguish absent from invisible."
const (
	edgeOK          = "ok"
	edgeUnresolved  = "unresolved"
	edgeUnreachable = "unreachable"
	edgeNotChecked  = "not_checked"
)

// Why one edge was not checked. Contract strings, like the sync* walk reasons —
// and, like those, every one of them is NEVER GREEN.
const (
	// syncReasonCodexUnverifiable — the codex route group carries NO AccessScope
	// at all, and requireScope fail-closes on a route that declares none ("this
	// route is not accessible to scoped tokens"). No scoped token can ever read
	// a codex by id, so this CLI cannot verify a `{{ codex(…) }}` reference and
	// must say so rather than pass it.
	//
	// ⚠️ It is a limit of the BY-ID READ, not of codex references in general:
	// POST /workflow/validate resolves codex ids server-side against the
	// caller's own reach, so a workflow folder's codex edges come back decided.
	// Only the pipeline and data-app legs, which ask by id, land here.
	syncReasonCodexUnverifiable = "codex_unverifiable"
	// syncReasonMailboxUnverifiable — there is no GET-by-id route for a mailbox
	// at all, only a list. Nothing to ask.
	syncReasonMailboxUnverifiable = "mailbox_unverifiable"
	// syncReasonPositionalRef — `{{ ref('0') }}` is an INDEX into the table
	// row's own input_models, not a name, so what it points at is a property of
	// the server's copy of the row and not of the file on disk. Reported rather
	// than skipped: an edge nobody looked at must not read as an edge that is
	// fine.
	syncReasonPositionalRef = "positional_ref"
	// syncReasonNoCredential — there is no token, so nothing that needs a
	// request could be asked. The local legs still run, which is the point:
	// "this alias is bound to nothing" is answerable signed out.
	syncReasonNoCredential = "no_credential"
	// syncReasonLookupFailed — the read was attempted and the instance answered
	// something that is not a verdict about the row: a 401, a 403, a 5xx, no
	// network. Distinct from unreachable, which is the server giving a definite
	// negative.
	syncReasonLookupFailed = "lookup_failed"
	// syncReasonNoFeature — POST /api/v2/workflow/validate refuses without a
	// featureID, before it does any work, and a stack with no featureID is the
	// normal state before a folder's first push. See errNoFeature.
	syncReasonNoFeature = "no_feature"
	// syncReasonAccessUnmanaged — the data-app folder does not manage its
	// `access` block, so the allowlist lives on the server and the folder
	// declares nothing to compare source against.
	syncReasonAccessUnmanaged = "access_unmanaged"
	// syncReasonFolderRefused — the folder could not be opened against this
	// credential (an organization nothing names, a manifest the openers refuse).
	syncReasonFolderRefused = "folder_refused"
	// syncReasonNotBoundHere — NO --stack was given, and nothing this folder
	// declares answers to the organization this credential reaches.
	//
	// ⚠️ It is the no-flag half of the rule resolveStackForFolder applies to the
	// named one, and it was missing. Without it a folder belonging to another
	// organization falls through to an UNBOUND Selection, whose Bind is nil — so
	// checkAliases reports every declared alias as bound to nothing and the
	// folder comes back `broken`, exit 1. That is a confident, definite verdict
	// about a folder we cannot speak for: we do not know which organization it
	// targets, so "this alias is unbound" is not a statement anything here is
	// entitled to make. (It also reaches for describeBindSite's unnamed-entry
	// wording and names an "instances" entry a stacks-only manifest does not
	// contain, sending the reader after config that is not there.)
	//
	// The case that must STAY definite is the one this whole project exists to
	// catch: a folder that DOES resolve an entry for this credential and has an
	// alias with no bind is `unresolved`, exit 1, with no network at all. That is
	// why the guard is on Selection.Bound rather than on the alias report — being
	// bound here is exactly "there is a target to be unbound FOR".
	syncReasonNotBoundHere = "not_bound_here"
)

// syncEdgeReport is one reference, and the --json shape a caller acts on.
type syncEdgeReport struct {
	// Kind is the dependency kind of the thing referenced — table, secret,
	// agent, codex, mailbox, workflow — or "dependency" for a finding about the
	// manifest's own declarations rather than about a use of one.
	Kind string `json:"kind"`
	// Ref is what the folder actually WROTE: an alias, a literal id, or a
	// sibling's file stem. It is what a reader searches their source for.
	Ref string `json:"ref,omitempty"`
	// ID is what Ref resolved to, empty when nothing resolved it.
	ID string `json:"id,omitempty"`
	// Where names the file or the manifest slot the reference is written in.
	Where string `json:"where,omitempty"`
	// Verdict is one of the four words above — the contract.
	Verdict string `json:"verdict"`
	// Reason is a sync* constant, set ONLY for not_checked. Detail is prose and
	// may be reworded; Reason may not.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Unused marks a declared data-app grant that no file in the folder
	// mentions. Advisory: a dead grant is wider access than the app needs, not a
	// broken edge, so it never moves a verdict.
	Unused bool `json:"unused,omitempty"`
	// DroppedBinding marks the server's SOFT tier: a `{{ secret }}` marker
	// naming a secret the save would filter out rather than refuse. The edge is
	// `unreachable` like any other — the reference does not resolve and every run
	// that touches the marker fails — and this field is what lets
	// --allow-dropped-bindings change the SCORE without changing the REPORT.
	//
	// Set whether or not the flag was given, for the reason pushResult keeps
	// DroppedBindings and DroppedBindingsAllowed as two fields: the evidence and
	// the decision about it are different facts, and a reader has to be able to
	// see the first even when the second is "accepted".
	DroppedBinding bool `json:"droppedBinding,omitempty"`
}

// edgeKey is one (kind, id) pair — what verification is deduped by.
type edgeKey struct{ kind, id string }

// edgeChecker asks the instance whether an id still resolves, ONCE per
// (kind, id) across the whole tree.
//
// The dedupe is where nearly all of the saving is: ids repeat heavily across a
// feature's folders — one shared dimension table read by nine pipeline files is
// nine edges and one row — and the alternative is a request per occurrence.
//
// SERIAL, deliberately, for the reason syncStatusTree gives: api.Client is
// field-only and safe to share, so concurrency would work, but it would be
// stacked on top of the root threading and the shared stack resolution, which
// is precisely where the subtle bugs would be. Dedupe already fixes the
// pathological case; add parallelism when somebody reports a slow repository.
//
// A NIL client means signed out, and every lookup then answers not_checked
// rather than being skipped — see syncReasonNoCredential.
type edgeChecker struct {
	client *api.Client
	seen   map[edgeKey]edgeOutcome
}

// edgeOutcome is a verdict plus its explanation, cached per (kind, id).
type edgeOutcome struct {
	Verdict string
	Reason  string
	Detail  string
}

func newEdgeChecker(client *api.Client) *edgeChecker {
	return &edgeChecker{client: client, seen: map[edgeKey]edgeOutcome{}}
}

// check answers for one (kind, id), from cache when it can.
func (c *edgeChecker) check(ctx context.Context, kind, id string) edgeOutcome {
	// The two kinds no scoped token can verify, answered BEFORE the shape test
	// and before the cache: the answer does not depend on the id, and the reason
	// is about the route rather than about the row.
	switch kind {
	case markers.KindCodex:
		return edgeOutcome{edgeNotChecked, syncReasonCodexUnverifiable,
			"a codex cannot be read by id with a scoped token — its route group declares no access scope, so every scoped token is refused. Nothing here can vouch for this reference either way"}
	case markers.KindMailbox:
		return edgeOutcome{edgeNotChecked, syncReasonMailboxUnverifiable,
			"there is no read-one-mailbox route to ask — only a listing — so nothing here can vouch for this reference either way"}
	}
	// The SHAPE is tested locally first, and it is what makes the 400/404
	// collapse below safe: a 400 on a well-formed id is the server saying "not
	// available to you", while a 400 on something that was never an id would be
	// the server saying "that is not an id". Only the first is unreachable.
	if !markers.IsResourceID(kind, id) {
		return edgeOutcome{Verdict: edgeUnresolved, Detail: fmt.Sprintf(
			"%q is not a %s id and nothing here binds it to one", id, kind)}
	}
	key := edgeKey{kind: kind, id: id}
	if cached, hit := c.seen[key]; hit {
		return cached
	}
	out := c.lookup(ctx, kind, id)
	c.seen[key] = out
	return out
}

// lookup makes the one request, and maps what comes back onto the vocabulary.
func (c *edgeChecker) lookup(ctx context.Context, kind, id string) edgeOutcome {
	if c.client == nil {
		return edgeOutcome{edgeNotChecked, syncReasonNoCredential,
			"there is no credential for this instance, so nothing could be looked up — sign in with `ronja login`"}
	}
	var err error
	switch kind {
	case markers.KindTable:
		_, err = c.client.GetTable(ctx, id)
	case markers.KindWorkflow:
		_, err = c.client.GetWorkflow(ctx, id)
	case markers.KindAgent:
		_, err = c.client.GetAgent(ctx, id)
	case markers.KindSecret:
		_, err = c.client.GetSecret(ctx, id)
	case markers.KindNote:
		// A MARKER-LESS kind, and it still belongs here: an automation declares
		// its notes as a structured reference set rather than in source, and a
		// declared-but-dangling note is exactly the edge this command exists to
		// find. Without it every note-declaring folder answered not_checked,
		// which is `unknown` for the folder and exit 2 for the tree — a verdict
		// nothing the reader does can clear.
		_, err = c.client.GetNote(ctx, id)
	default:
		// A kind this build has no read for. Reached only if markers grows a
		// family and this switch is not extended — which is exactly when saying
		// nothing would be worst.
		return edgeOutcome{edgeNotChecked, syncReasonLookupFailed,
			fmt.Sprintf("this build has no way to read a %s by id", kind)}
	}
	if err == nil {
		return edgeOutcome{Verdict: edgeOK}
	}
	// 400 AND 404 BOTH MEAN UNREACHABLE. See this file's opening comment: the
	// no-enumeration 404 hides a row you may not see, and a deleted / trashed /
	// cross-tenant row surfaces as table.ErrNoRows, which is rjerr.Input and
	// therefore 400. Neither says which, and reading either as "my request was
	// malformed" would swallow the one finding this command exists to make.
	switch api.StatusOf(err) {
	case 400, 404:
		return edgeOutcome{Verdict: edgeUnreachable, Detail: fmt.Sprintf(
			"%s does not resolve on this instance — it may have been deleted, moved to the trash, or belong to an organization this credential does not reach. The read gate answers the same way for all three, on purpose, so that existence does not leak", id)}
	}
	// Everything else — a 401, a 403, a 5xx, no network — is the instance
	// failing to answer, not a verdict about the row.
	return edgeOutcome{edgeNotChecked, syncReasonLookupFailed, err.Error()}
}

// apply resolves the undecided edges in place. An edge that already carries a
// verdict was decided locally and is left exactly as it is.
func (c *edgeChecker) apply(ctx context.Context, edges []syncEdgeReport) []syncEdgeReport {
	for i := range edges {
		if edges[i].Verdict != "" {
			continue
		}
		out := c.check(ctx, edges[i].Kind, edges[i].ID)
		edges[i].Verdict, edges[i].Reason = out.Verdict, out.Reason
		if out.Detail != "" {
			edges[i].Detail = out.Detail
		}
	}
	return edges
}

// ── Enumeration ────────────────────────────────────────────────────────────

// dependencyEdges turns the folder's own declaration/bind problems into edges.
//
// This is the ONE leg decidable with NO NETWORK AT ALL, and it is the failure
// the whole alias layer exists to fix: an alias a folder declares and the
// selected stack binds nothing to resolves to nothing, the marker ships the
// literal name, and for a secret the server SOFT-FAILS that into a warning — so
// the folder deploys green and fails at run time.
//
// checkAliases is what computes it, rather than a second copy here: it already
// owns the declaration rules, the cross-object bind rules, the literal-bind-
// target refusal and the wrong-family refusal, and a reimplementation would
// disagree with `push` about which folders are pushable.
//
// The refusals become `unresolved` edges (definite, never green) and the
// warnings become the folder's warnings, which is exactly the split
// aliasReport's own doc draws.
func dependencyEdges(m *wfdir.Manifest, aliases aliasReport) []syncEdgeReport {
	names := m.AliasNames()
	out := make([]syncEdgeReport, 0, len(aliases.Refusals))
	for _, refusal := range aliases.Refusals {
		out = append(out, syncEdgeReport{
			Kind:    "dependency",
			Ref:     aliasNamedIn(refusal, names),
			Where:   wfdir.ManifestName,
			Verdict: edgeUnresolved,
			Detail:  refusal,
		})
	}
	return out
}

// aliasNamedIn is which declared alias a refusal is about, or "" when that is
// not one name.
//
// It reads the refusal rather than being handed the name, because checkAliases
// returns prose: its rules live across four helpers and a couple of
// wfdir methods, and threading a name out of every one of them would be a
// refactor of the pre-flight in order to fill in a DISPLAY column. Every message
// that is about one alias quotes it with %q, so a quoted exact match is
// reliable — and the closing quote is what keeps `"orders"` from matching
// `"orders2"`.
//
// ⚠️ Empty on no match AND on more than one, which is the safe direction in both
// cases: a refusal naming two aliases (two bound to one id) is about the pair,
// and labelling the line with whichever was found first would name the wrong
// half. The detail carries the whole sentence either way.
func aliasNamedIn(refusal string, names []string) string {
	found := ""
	for _, name := range names {
		if !strings.Contains(refusal, strconv.Quote(name)) {
			continue
		}
		if found != "" {
			return ""
		}
		found = name
	}
	return found
}

// markerEdges enumerates every alias-eligible marker in a folder's source and
// resolves it as far as the folder itself can.
//
// `pipe` is the pipeline folder's sibling-stem codec, nil for the other kinds.
// Deduped per (file, kind, ref): a file that reads one table four times is one
// edge, because the reader has one thing to fix.
func markerEdges(f *folder, files map[string]string, pipe *pipelineCodec) []syncEdgeReport {
	var out []syncEdgeReport
	for _, path := range sortedPaths(files) {
		seen := map[aliasKey]bool{}
		for _, o := range markers.Scan(files[path]) {
			key := aliasKey{kind: o.Family.Kind, name: o.Arg}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, resolveMarkerEdge(f, pipe, path, o))
		}
	}
	return out
}

// resolveMarkerEdge decides what one marker occurrence points at, using exactly
// the resolution order a push uses — sibling stem first, then a declared alias,
// then a literal id — so an edge this reports as resolved is one the push would
// resolve the same way.
func resolveMarkerEdge(f *folder, pipe *pipelineCodec, path string, o markers.Occurrence) syncEdgeReport {
	edge := syncEdgeReport{Kind: o.Family.Kind, Ref: o.Arg, Where: path}
	if o.Positional {
		edge.Verdict, edge.Reason = edgeNotChecked, syncReasonPositionalRef
		edge.Detail = fmt.Sprintf("%s is an index into this table's own input_models on the server, not a name — what it points at is a property of the row rather than of this file, so nothing here can resolve it", o.Marker)
		return edge
	}
	// A SIBLING'S STEM WINS over a declared alias, which is pipelineCodec's own
	// rule: the file you can see beats a declaration you have to scroll for.
	if pipe != nil && o.Family.Kind == markers.KindTable {
		if sibling, claimed := pipe.pathByStem[o.Arg]; claimed {
			if len(pipe.ambiguous[o.Arg]) > 0 {
				edge.Verdict = edgeUnresolved
				edge.Detail = fmt.Sprintf("two files in this folder are called %q (%s), so this reference has no single answer — rename one of them, or write the table's id",
					o.Arg, strings.Join(pipe.ambiguous[o.Arg], " and "))
				return edge
			}
			// A sibling is verified by `sync status`, not here: this folder's own
			// files are its content, and a first push creates the table the
			// reference names. There is nothing to ask the server about.
			edge.Verdict = edgeOK
			edge.ID = f.Binding.Tables[sibling]
			edge.Detail = "names " + sibling + " in this folder"
			return edge
		}
	}
	if id, bound := f.Codec.toID[aliasKey{kind: o.Family.Kind, name: o.Arg}]; bound {
		// Undecided: the checker verifies it.
		edge.ID = id
		return edge
	}
	if markers.IsResourceID(o.Family.Kind, o.Arg) {
		edge.ID = o.Arg
		return edge
	}
	edge.Verdict = edgeUnresolved
	edge.Detail = fmt.Sprintf("%s is neither a %s id nor a %s dependency this stack binds, so the server receives the literal name",
		o.Marker, o.Family.Kind, o.Family.Kind)
	return edge
}

// accessEdges enumerates a data app's DECLARED grants — the dependencies that
// are not in its content.
//
// `access` must already have crossed the codec (folder.declaredAccess), so what
// arrives here is ids for every alias this stack binds and the verbatim name for
// every one it does not — which is exactly the split the two verdicts want.
func accessEdges(access api.DataAppAccess) []syncEdgeReport {
	var out []syncEdgeReport
	for _, dep := range accessDependencies(&access) {
		for _, id := range *dep.IDs {
			out = append(out, syncEdgeReport{
				Kind: dep.Kind, Ref: id, ID: id,
				Where: wfdir.ManifestName + " access." + dep.Key,
			})
		}
	}
	return out
}

// idLiteralPattern finds a quoted resource id in a data app's source.
//
// QUOTED LITERALS ONLY, and that conservatism is deliberate. This scan feeds
// `used_but_undeclared`, which is a finding a reader acts on, so a false
// positive costs somebody a red CI run. Requiring the quotes means an id
// written in prose is not matched, while every form the runtime can actually
// reach — a call argument, a const, a template literal — is. It is the same
// hoisted `const X = "secret-…"` form the server's own auto-registration scans
// for.
//
// ⚠️ DERIVED from markers.IDPrefixes(), never spelled here. It used to hand-list
// the prefixes, with nothing pinning the two together: adding a kind to markers
// would have silently stopped this check covering it, and no test would have
// failed. Building the pattern from that table makes the coupling mechanical
// instead of remembered.
var idLiteralPattern = regexp.MustCompile(
	"['\"`]((?:" + strings.Join(quotedIDPrefixes(), "|") + ")[A-Za-z0-9_.:-]+)['\"`]")

// quotedIDPrefixes is markers.IDPrefixes() made safe to splice into a pattern.
// Longest-first order comes from there and is load-bearing: an alternation takes
// the first branch that matches.
func quotedIDPrefixes() []string {
	prefixes := markers.IDPrefixes()
	out := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		out = append(out, regexp.QuoteMeta(prefix))
	}
	return out
}

// appSourceExts are the files a data app's BUNDLE actually executes, and
// therefore the only ones an id in can be a real reference.
//
// ⚠️ It exists because wfdir.DataAppKind declares NO SyncExt — an app folder
// syncs everything the structural rules allow, README and test fixtures
// included — so a scan over the enumeration is a scan over the whole folder. A
// quoted `table-…` in a committed README, a fixture or a design note would then
// fail the tree, and the remedy the finding advertised was to add that id to the
// app's real `access` allowlist. A false positive whose fix WIDENS privileges is
// worse than the miss it prevents, so the scan is scoped and the message no
// longer leads with "declare it" (see compareDeclaredAndUsed).
//
// What this gives up, stated rather than hidden: an id that reaches the app
// through a non-source file — `import cfg from './config.json'` — is not seen.
// That app fails at run time with rejected_access exactly as it did before this
// check existed, which is a miss, not a regression.
var appSourceExts = map[string]bool{
	".tsx": true, ".ts": true, ".jsx": true, ".js": true, ".mjs": true, ".cjs": true,
}

// isAppSource reports whether a path is a file the app's bundle executes.
func isAppSource(path string) bool { return appSourceExts[strings.ToLower(filepath.Ext(path))] }

// usedResourceIDs is every resource id a data app's source actually reaches
// for: the marker arguments that resolved to one, plus the quoted literals.
//
// Keyed by (kind, id) rather than by id alone because the kind is what decides
// which allowlist has to carry it.
//
// SOURCE FILES ONLY — see appSourceExts for why, and for what that costs.
func usedResourceIDs(f *folder, files map[string]string) map[edgeKey]string {
	out := map[edgeKey]string{}
	for _, path := range sortedPaths(files) {
		if !isAppSource(path) {
			continue
		}
		content := files[path]
		for _, o := range markers.Scan(content) {
			if o.Positional {
				continue
			}
			id := o.Arg
			if bound, ok := f.Codec.toID[aliasKey{kind: o.Family.Kind, name: o.Arg}]; ok {
				id = bound
			}
			if markers.IsResourceID(o.Family.Kind, id) {
				out[edgeKey{kind: o.Family.Kind, id: id}] = path
			}
		}
		for _, match := range idLiteralPattern.FindAllStringSubmatch(content, -1) {
			if kind := kindOfResourceID(match[1]); kind != "" {
				out[edgeKey{kind: kind, id: match[1]}] = path
			}
		}
	}
	return out
}

// kindOfResourceID names the dependency kind an id belongs to, or "" when the
// string is not shaped like one.
//
// Derived from markers.IsResourceID rather than from a second prefix table, so
// a kind added there cannot be missed here. The prefixes do not collide across
// kinds, so the first match is the answer.
func kindOfResourceID(id string) string {
	for _, kind := range markers.Kinds() {
		if markers.IsResourceID(kind, id) {
			return kind
		}
	}
	return ""
}

// declaredResourceIDs is the set an access block grants, keyed the way
// usedResourceIDs is.
//
// ⚠️ A METRIC rides the `table-` prefix and is declared under
// allowedMetricIDs, so both slots fold into the `table` kind — the same
// collapse accessDependencies makes, and for the same reason. Keeping them
// apart here would report every metric an app queries as an undeclared table.
func declaredResourceIDs(access api.DataAppAccess) map[edgeKey]bool {
	out := map[edgeKey]bool{}
	for _, dep := range accessDependencies(&access) {
		for _, id := range *dep.IDs {
			out[edgeKey{kind: dep.Kind, id: id}] = true
		}
	}
	return out
}

// compareDeclaredAndUsed reports both directions of a data app's allowlist
// against its source: what it grants and does not use, and what it uses and does
// not grant.
//
// ⚠️ THE SECOND DIRECTION IS THE ONE THAT COSTS, and it costs because of how the
// CLI writes. The agent path auto-repairs an app's allowlist —
// autoRegisterDataAppRefs derives the grants from the source it is writing — but
// the HTTP route the CLI pushes through, PUT /dataapp/:id/files/*path, calls
// UpsertFileChecked and NOTHING ELSE. So a CLI-pushed app's allowlist is NEVER
// auto-repaired: an app that queries a table it never declared pushes perfectly
// clean, compiles, publishes, and then fails at run time with rejected_access,
// with nothing in the deploy that said so.
//
// The first direction is a warning, not a finding: a grant nothing uses is
// wider access than the app needs, which is worth saying and is not broken.
//
// `unused` marks the declared edges in place; the returned strings are the
// findings.
func compareDeclaredAndUsed(edges []syncEdgeReport, access api.DataAppAccess, used map[edgeKey]string) []string {
	declared := declaredResourceIDs(access)
	for i := range edges {
		// An entry that is not id-shaped is already an `unresolved` edge; calling
		// it unused as well would be two complaints about one broken line.
		if !markers.IsResourceID(edges[i].Kind, edges[i].ID) {
			continue
		}
		if _, isUsed := used[edgeKey{kind: edges[i].Kind, id: edges[i].ID}]; !isUsed {
			edges[i].Unused = true
		}
	}

	var out []string
	for _, key := range sortedEdgeKeys(used) {
		if declared[key] {
			continue
		}
		// ⚠️ THE REMEDY IS NOT "ADD IT", and the wording matters more than it
		// looks. The `access` block is the app's PRIVILEGES, so a message whose
		// default advice is to widen it turns every false positive here into a
		// grant nobody meant to make. So: name the file, ask what the line is
		// doing, and offer deleting the reference as the equal half of the
		// answer. Only an id the app genuinely reads belongs in the allowlist.
		out = append(out, fmt.Sprintf(
			"%s writes the id %s, and %s does not grant it — a data app may only reach what its \"access\" block lists, and unlike the in-app agent the CLI's push does NOT repair the allowlist for you, so this app would push clean, compile, publish and then fail at run time with rejected_access. Look at what that line in %s is doing before you change anything: if the app really reads that row, declare it under \"access\"; if it is a leftover, a copied example, or a row the app no longer uses, delete the reference instead — widening the allowlist to silence this would grant the app a row it does not need",
			used[key], key.id, wfdir.ManifestName, used[key]))
	}
	return out
}

// sortedEdgeKeys orders a (kind, id) set, so a report reads the same way twice.
func sortedEdgeKeys(set map[edgeKey]string) []edgeKey {
	out := make([]edgeKey, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].kind != out[j].kind {
			return out[i].kind < out[j].kind
		}
		return out[i].id < out[j].id
	})
	return out
}

// ── The server's own answer, for workflows ─────────────────────────────────

// validateFindingKinds maps the server's per-marker finding codes onto the
// dependency kind each one is about.
//
// These codes are a WIRE CONTRACT (rworkflow's own comment: "a client branches
// on them, so they may be added to but never renamed"), which is what makes
// keying on them safe. A code not in this table is not a per-reference finding
// and is reported as a folder-level one — `mixed_ref_style` and
// `missing_entrypoint` are both real problems and neither is about whether one
// id resolves.
// findingSecretDropped is the one per-reference code that has a hatch behind it
// (--allow-dropped-bindings), so it is named rather than spelled inline twice.
const findingSecretDropped = "secret_dropped"

var validateFindingKinds = map[string]string{
	"unresolved_ref":      markers.KindTable,
	"unresolved_write":    markers.KindTable,
	"unresolved_agent":    markers.KindAgent,
	"unresolved_codex":    markers.KindCodex,
	"unresolved_mailbox":  markers.KindMailbox,
	"unresolved_workflow": markers.KindWorkflow,
	// `{{ module }}` is a per-reference marker like the rest, so an unreachable
	// one is an EDGE. Without this entry it still reached the reader — an
	// unmapped error code falls through to the folder-level findings list, and
	// the verdict still goes red — but as a bare sentence rather than as a row
	// naming the module, the file it is imported in, and the verdict. The edge
	// shape is the whole output of `sync check`, so a marker family that lands
	// outside it is the one family you cannot scan for.
	"unresolved_module":  markers.KindModule,
	findingSecretDropped: markers.KindSecret,
}

// edgesFromValidation turns POST /workflow/validate's answer into edges.
//
// The server's `resolved` bindings are the ok edges — it looked, against the
// caller's own reach, and it found them — so there is nothing to re-ask. That
// includes CODEX ids, which this CLI cannot read by id but validate resolves
// perfectly well; see syncReasonCodexUnverifiable.
//
// An `unresolved_*` finding is `unreachable` rather than `unresolved`, and the
// split is exact rather than approximate: validateFilesOf sends the RESOLVED
// source, so every alias this stack binds has already become an id by the time
// the server sees it, and a name that could not be resolved was already refused
// locally by the alias pre-flight (dependencyEdges). What is left for the
// server to fail on is a well-formed id it will not show us — which is what
// `unreachable` means.
//
// ⚠️ `secret_dropped` is a WARNING server-side and a FINDING here, deliberately
// — and this is shipped precedent rather than a new opinion. `ronja wf push`
// already exits non-zero on exactly this code, for exactly this reason: the
// warning TIER is right for a person about to create the secret, and what was
// wrong was the exit code, because that is the only thing CI reads and a deploy
// that shipped an unrunnable workflow used to report success. The save path only
// warns because refusing a save over a credential somebody is about to connect
// would be obstructive; that is a policy about writing, not an answer about
// resolving. The alias layer takes the same stricter line locally (see
// misusedAliases).
//
// And, like push, the strictness comes with the "I will bind it later" hatch:
// the edge is marked DroppedBinding, and --allow-dropped-bindings takes it out
// of the SCORE while leaving it in the REPORT.
func edgesFromValidation(result *api.ValidateResult) (edges []syncEdgeReport, findings, warnings []string) {
	for _, group := range []struct {
		ids  []string
		kind string
	}{
		{result.Resolved.InputTableIDs, markers.KindTable},
		{result.Resolved.OutputTableIDs, markers.KindTable},
		{result.Resolved.SecretIDs, markers.KindSecret},
		{result.Resolved.QuerySecretIDs, markers.KindSecret},
		{result.Resolved.AgentIDs, markers.KindAgent},
		{result.Resolved.CodexIDs, markers.KindCodex},
	} {
		for _, id := range group.ids {
			edges = append(edges, syncEdgeReport{
				Kind: group.kind, Ref: id, ID: id, Where: "(instance)",
				Verdict: edgeOK, Detail: "resolved by the instance",
			})
		}
	}
	for _, finding := range result.Findings {
		kind, perReference := validateFindingKinds[finding.Code]
		if !perReference {
			if finding.IsError() {
				findings = append(findings, fmt.Sprintf("%s: %s", finding.Code, finding.Message))
			} else {
				warnings = append(warnings, fmt.Sprintf("%s: %s", finding.Code, finding.Message))
			}
			continue
		}
		edges = append(edges, syncEdgeReport{
			Kind:    kind,
			Ref:     markerArgOf(finding.Marker),
			Where:   findingPath(finding),
			Verdict: edgeUnreachable,
			Detail:  finding.Message,
			// The one code with a hatch behind it — see DroppedBinding, and
			// `ronja wf push`, which pairs the same strictness with the same flag.
			DroppedBinding: finding.Code == findingSecretDropped,
		})
	}
	return edges, findings, warnings
}

// markerArgOf pulls the referenced name out of a finding's canonical marker
// form, so the report can say WHICH reference rather than quoting the whole
// marker in a column meant for a name. Empty when the marker is absent or is
// not one this build's grammar recognises — the field is a hint, not a promise.
func markerArgOf(marker string) string {
	if found := markers.Scan(marker); len(found) > 0 {
		return found[0].Arg
	}
	return ""
}

// findingPath names where a finding belongs, keeping the server's own word for
// a finding that is about no file in particular.
func findingPath(finding api.ValidateFinding) string {
	if finding.Path == "" {
		return "(no file)"
	}
	return finding.Path
}
