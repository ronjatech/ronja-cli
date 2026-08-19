package api

// The WRITE half of the table surface: create, code sync, and the four
// lifecycle verbs the pipeline loop needs (checkout, sync, commit, discard) plus
// the review request a non-admin exits through.
//
// Split from table.go — which mirrors the rows the CLI READS — because these are
// the calls that change something, and every one of them is a place where a
// wrong json tag is silently destructive rather than merely blank. Same
// hand-mirror rule as everywhere else: every tag below is copied from the server
// struct named in its comment, and the fixture tests hold them in step.
//
// One route in here does NOT live under /feature/model: request-review is on the
// governance handler at /api/v2/table/draft/:id/request-review, in the `admin`
// scope group. See RequestTableReview.
//
// Create, code-write and commit go through doSlow rather than Do. All three do
// real work inside the request — a create resolves refs and access, a PUT
// re-derives lineage, a commit copies the draft onto its parent and fires the
// cascade — and the 30s read bound killed them with a message about nothing on
// an instance that was merely busy. The wider bound does not make the reconcile
// paths in `pipeline push` / `pipeline publish` redundant: 120s expires too, and
// a create or a commit that timed out may still have landed.

import (
	"context"
	"net/url"
)

// TableEngineDuckDB is the only engine value, mirrored from
// rdb.ModelEngineDuckDB. It means `code` is pure DuckDB SQL with
// `{{ ref('table-…') }}` markers, which is exactly what a pipeline folder's .sql
// files are — so it is sent explicitly at create rather than left to the
// server's default. The default only applies when `code` is present, and stating
// it costs one field and removes a conditional nobody would remember.
const TableEngineDuckDB = "duckdb"

// CreateTableInput is the body of POST /feature/model, mirroring the subset of
// feature.CreateTableInput the CLI writes.
//
// Six fields, where the server's request type has fifteen. Everything else is
// either build state, draft lifecycle, metric recipe or verification — none of
// it the CLI's to set, and the server's own type comment explains why the fat
// body was narrowed in the first place.
//
// InputModels is sent even though the server would derive it from the refs in
// `code`. That is the same rule as on the update path and for the same reason —
// see UpdateTableCode — plus one specific to create: the derivation and the
// access check run off different inputs, and sending the list makes the lineage
// this folder DECLARES the lineage the row gets, rather than whatever a regex
// found.
type CreateTableInput struct {
	Name      string `json:"name"`
	FeatureID string `json:"featureID"`
	// Kind is always TableKindDerived from this loop. A pipeline folder is a
	// folder of SQL; a kind that is not built from SQL has nothing a .sql file
	// could mean.
	Kind        string   `json:"kind"`
	Engine      string   `json:"engine"`
	Code        string   `json:"code"`
	InputModels []string `json:"inputModels"`
}

// UpdateTableInput is the body of PUT /feature/model/:id, mirroring the two
// fields of rmodelv2.Patch the CLI writes.
//
// BOTH are always sent, together, on every write. This is the single most
// load-bearing decision in the client, so it is stated here rather than left to
// a caller to remember:
//
//   - Server-side derivation (resolveDerivedRefs) fires ONLY when the effective
//     declared list is empty, and a checked-out draft INHERITS its parent's
//     non-empty input_models. So a PUT that omitted the field would 400 the
//     moment an edit added a new upstream ref — the commonest pipeline edit
//     there is.
//   - Sending the exact list also REMOVES stale entries. Left behind they are
//     phantom lineage edges that survive in the graph and trigger
//     GetDependents-driven cascade rebuilds of a table that no longer reads
//     from them.
//
// No omitempty on either: an empty `inputModels` is a real declaration ("this
// table reads from nothing"), and omitempty would collapse it into "derive it
// for me", which is a different instruction.
type UpdateTableInput struct {
	Code        string   `json:"code"`
	InputModels []string `json:"inputModels"`
}

// CreateTable creates a derived table inside a feature.
//
// The row it returns is LIVE, not a draft — unlike a workflow or a data app,
// whose create endpoints mint a parentless draft that publish promotes in place.
// A table created here is visible to the feature immediately, with no data until
// something syncs it. (Parentless table drafts are a known follow-up; until then
// a caller that creates a table has created something colleagues can see.)
//
// Admin-only server-side, even though editing a draft is not: create is the one
// step of the loop a non-admin cannot perform.
func (c *Client) CreateTable(ctx context.Context, in CreateTableInput) (*Table, error) {
	var out Table
	if err := c.doSlow(ctx, "POST", "feature/model", in, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return &out, nil
}

// UpdateTableCode writes a table's SQL and its declared inputs.
//
// `id` is the row being written, which in the edit loop is always the DRAFT's
// id. Writing the LIVE row instead is silently lost work: committing a draft
// overwrites the parent's fields from the draft, so a PUT on the canonical
// vanishes the moment anyone commits.
//
// The code sent should be the CANONICAL id-ref form (see tablerefs). A
// positional ref written here is re-resolved server-side against whatever
// inputModels this same call declares, which is not necessarily the list it was
// numbered against.
//
// The endpoint answers with nothing, so a caller that needs the row's current
// state re-reads it.
func (c *Client) UpdateTableCode(ctx context.Context, id, code string, inputModels []string) error {
	if inputModels == nil {
		// A null would read as "no declaration" and re-arm server-side
		// derivation; an empty list is the declaration this means.
		inputModels = []string{}
	}
	return c.doSlow(ctx, "PUT", "feature/model/"+url.PathEscape(id),
		UpdateTableInput{Code: code, InputModels: inputModels}, nil)
}

// CheckoutTable gets-or-creates the CALLER'S OWN draft of a table and returns
// it. The returned row has a NEW id, and every subsequent call in the edit loop
// takes that id rather than the original's.
//
// NOT idempotent, unlike CheckoutWorkflow: a second checkout while a draft is
// already open is an error. GetTableDraft is how an existing draft is picked
// back up, which is why the push path always tries that first.
//
// The draft copies the original's config and its data references, so it is
// queryable before anything changes — but a draft that has been checked out and
// never SYNCED has no partitions of its own, so sample rows are only ever read
// after a successful sync.
func (c *Client) CheckoutTable(ctx context.Context, id string) (*Table, error) {
	var out Table
	if err := c.Do(ctx, "POST", "feature/model/"+url.PathEscape(id)+"/checkout", nil, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return &out, nil
}

// SyncTable triggers a DETERMINISTIC materialization: the server executes the
// stored `code` exactly as written, with no AI and no code rewriting.
//
// This is the golden path, and the distinction from POST /:id/build matters
// enough to state: `/build` is AI-ASSISTED and may REWRITE the stored SQL, which
// for a folder-synced table would silently replace the file's own content. The
// CLI never calls it.
//
// Two further properties, both of which shape the loop around this call:
//
//   - It syncs EXACTLY the row named. It does not mint a draft (`/build` does),
//     so the id passed here decides whether the live table or the draft is
//     materialized.
//   - The 200 means ACCEPTED, not started successfully. Every failure — down to
//     syncing a table with no code at all — is reported on the ROW, never in
//     this response. WaitForTableBuild is the other half of this call.
//
// A sync does NOT cascade: dependent tables are not rebuilt. Commit is what
// cascades.
func (c *Client) SyncTable(ctx context.Context, id string) error {
	return c.Do(ctx, "POST", "feature/model/"+url.PathEscape(id)+"/sync", nil, nil)
}

// CommitTableDraft publishes a draft onto the table it was forked from. Takes
// the DRAFT's id — this is the one call in the loop where the wrong id silently
// targets the wrong row.
//
// Two consequences worth holding on to:
//
//   - It OVERWRITES the parent's fields from the draft, and there is no
//     compare-and-swap. A stale draft silently reverts whatever landed in
//     between; the review payload's baseStale + interveningVersions is the only
//     warning that exists, and nothing on this call will stop it.
//   - It CASCADES. The server emits a materialized event and dependent tables
//     are invalidated and rebuilt asynchronously — which is why the loop needs
//     no `run` verb, and why a publish is worth saying out loud.
//
// Refused on a shared feature for a non-admin, and possibly for an admin
// committing their own draft under a self-approval policy. Decide which branch
// to take from the feature's scope and the caller's role BEFORE calling, rather
// than from the rejection prose.
func (c *Client) CommitTableDraft(ctx context.Context, draftID string) error {
	return c.doSlow(ctx, "POST", "feature/model/"+url.PathEscape(draftID)+"/commit", nil, nil)
}

// DiscardTableDraft deletes a draft, throwing its uncommitted changes away. The
// parent table is untouched. Takes the DRAFT's id, and is not reversible.
func (c *Client) DiscardTableDraft(ctx context.Context, draftID string) error {
	return c.Do(ctx, "POST", "feature/model/"+url.PathEscape(draftID)+"/discard", nil, nil)
}

// RequestTableReview submits the caller's own draft for admin review. Takes the
// DRAFT's id, and is idempotent.
//
// NOTE the different prefix, exactly as with RequestWorkflowReview: this route
// lives on the GOVERNANCE handler at /api/v2/table/draft/:id/request-review,
// whose group is scoped `admin` rather than the table loop's `data`. The CLI's
// full-access PAT covers both; a scoped token would half-work, which is why the
// failure is reported as-is rather than swallowed.
//
// This is the only exit from the loop for a non-admin in a SHARED feature, where
// commit is refused.
func (c *Client) RequestTableReview(ctx context.Context, draftID string) error {
	return c.Do(ctx, "POST", "table/draft/"+url.PathEscape(draftID)+"/request-review", nil, nil)
}
