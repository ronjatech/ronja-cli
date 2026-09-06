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
	"errors"
	"io"
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

	// Description is the table's prose, from the file's `-- @table` header, and
	// DescriptionSource is always DescriptionSourceUser when it is set: a person
	// committed those words to a repository, so they are protected from the
	// agent's regeneration exactly as prose typed into the web app is.
	//
	// omitempty on BOTH, and it is load-bearing on the second: a create that
	// declared a source with no description would claim human authorship of
	// nothing.
	Description       string `json:"description,omitempty"`
	DescriptionSource string `json:"descriptionSource,omitempty"`

	// Fields is the column documentation the header declares.
	//
	// ⚠️ A create has MEASURED NOTHING, so under the join rule every name here
	// comes back unmatched — the prose is stored against the name and attaches at
	// the first build that produces the column. It is sent anyway so a table does
	// not exist even briefly with prose its own file already carries; the report
	// the push actually shows a reader is taken from the documentation write that
	// follows the build. See UpdateTableDocs.
	Fields []TableFieldInput `json:"fields,omitempty"`
}

// TableFieldInput is one entry of a `fields` write, mirroring the subset of
// rdb.ModelFieldV2 the CLI sends.
//
// DescriptionSource is always DescriptionSourceUser from this client. Every
// carrier the CLI has is a file a person committed to a repository, and the
// distinction the column protects is exactly that: prose Ronja may rewrite
// versus prose it may not.
type TableFieldInput struct {
	Name              string `json:"name"`
	Description       string `json:"description"`
	DescriptionSource string `json:"descriptionSource,omitempty"`
}

// DescriptionSourceUser marks prose a PERSON wrote, which the agent may not
// overwrite. Mirrored from rdb.DescriptionSourceUser.
const DescriptionSourceUser = "user"

// ColumnDocReport is what a `fields` write answers: THE JOIN RULE, reported.
//
// The measured column set is the truth and prose is joined onto it, so a name
// the table does not have is neither an error nor a new column — its prose is
// stored and re-attaches by itself if a later build produces the column, and the
// caller is told rather than refused. Drift between a committed file and the
// data is expected: a table is rebuilt on a schedule, a staging column gets
// renamed, a file is committed ahead of the pipeline that fills it.
//
// Both lists are omitempty server-side and both are sorted. An ABSENT report —
// every field empty — means the write carried no `fields` at all, never that
// nothing attached.
type ColumnDocReport struct {
	AttachedColumns  []string `json:"attachedColumns,omitempty"`
	UnmatchedColumns []string `json:"unmatchedColumns,omitempty"`
}

// Documented reports whether this answer describes a documentation write at all.
func (r ColumnDocReport) Documented() bool {
	return len(r.AttachedColumns) > 0 || len(r.UnmatchedColumns) > 0
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
//
// BaseCodeSha256 is the opposite case, and it MUST carry omitempty — the one
// place on this type where the tag is load-bearing in the other direction. The
// server reads three states (rmodelv2.checkCodePrecondition): ABSENT is "write
// unconditionally", a digest is compare-and-swap, and "" is a 400 rather than
// "no check", deliberately, so that a caller who thought they were asserting
// something is told they were not. An `omitempty`-less tag would send `""` on
// every unconditional write and turn the ordinary push into a 400.
type UpdateTableInput struct {
	Code        string   `json:"code"`
	InputModels []string `json:"inputModels"`
	// BaseCodeSha256 is the sha256 of the `code` the caller believes the row
	// currently holds, lowercase hex over the raw bytes — wfdir.HashString,
	// which rmodelv2.CodeSHA256 is held byte-identical to.
	//
	// ⚠️ It is a fingerprint of the WIRE form, because that is what the row
	// stores: the id-ref SQL this client SENT, not the aliased SQL on disk.
	BaseCodeSha256 string `json:"baseCodeSha256,omitempty"`
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
//
// baseCodeSha256 is empty for an unconditional write, which sends no such field
// at all — the shape this route took before the precondition existed. Supply it
// only as the fingerprint of the CODE THIS CLIENT LAST SENT to this same row;
// anything else (the disk form of an aliased file, a hash taken from a
// different row) is refused with a 409 that describes a conflict nobody caused.
func (c *Client) UpdateTableCode(ctx context.Context, id, code string, inputModels []string, baseCodeSha256 string) error {
	if inputModels == nil {
		// A null would read as "no declaration" and re-arm server-side
		// derivation; an empty list is the declaration this means.
		inputModels = []string{}
	}
	return c.doSlow(ctx, "PUT", "feature/model/"+url.PathEscape(id),
		UpdateTableInput{Code: code, InputModels: inputModels, BaseCodeSha256: baseCodeSha256}, nil)
}

// UpdateTableDocsInput is the body of the DOCUMENTATION half of PUT
// /feature/model/:id — a second, deliberately separate body from
// UpdateTableInput, which writes the SQL.
//
// SEPARATE BECAUSE THE ORDER MATTERS. Documentation is applied AFTER the build,
// never with it: the join rule attaches prose to the columns the table has
// MEASURED, so a `fields` write folded into the same PUT as the SQL would run
// before anything had been measured and report every column as unmatched — on a
// first push, correctly but uselessly, and on every later one it would report
// against the PREVIOUS build's columns. Push → build → attach → report.
//
// Every field is three-state, which is what makes a committed file able to
// document part of a table:
//
//   - Description nil        the folder does not manage the table's prose;
//     the key is absent and the row keeps what it has.
//   - Description to ""      an explicit claim that there is none. The server
//     clears the column and, deliberately, does NOT stamp
//     "user" — empty prose is the caller handing the field
//     back to Ronja, not a claim of authorship.
//   - Fields nil             no column is managed; the key is absent.
//   - Fields with an entry   that column's prose, "" to clear it. A column NOT
//     named is untouched, which is the whole reason a
//     folder may document three columns of twelve.
type UpdateTableDocsInput struct {
	Description       *string           `json:"description,omitempty"`
	DescriptionSource string            `json:"descriptionSource,omitempty"`
	Fields            []TableFieldInput `json:"fields,omitempty"`
}

// UpdateTableDocs writes a table's DOCUMENTATION and reports what attached.
//
// `id` is the row being documented — in the pipeline loop's edit cycle that is
// always the DRAFT's id, for UpdateTableCode's reason: committing a draft
// overwrites the parent's fields and its column rows from the draft, so prose
// written on the live row while a draft is open is lost the moment anyone
// publishes. The docs-sidecar path is the exception and writes the LIVE row,
// because a table this folder does not build has no draft in this loop at all.
//
// ALLOWED FOR EVERY ROLE on the caller's own draft. Documenting a table is not
// an admin act: a drafter may write the prose, and the admin who reviews the
// draft sees the column-description delta before approving it.
//
// A nil report is not a failure — it is the answer to a write that carried no
// `fields`, and it is also what an instance older than the report answers. The
// write landed either way; only the join rule's read-back is missing, which is
// why an EOF is folded into "no report" rather than into an error about a
// documentation write that actually succeeded.
//
// ⚠️ NIL THEREFORE MEANS "NOTHING WAS REPORTED", NEVER "NOTHING WAS WRITTEN",
// and a caller that reads it as the latter says nothing at all about a write
// that really happened. It is not disambiguated here because this layer cannot:
// the answer is empty in both cases, and only the caller still holds the file
// that says how many columns went out. commands.docsResultFrom is where the two
// are told apart, from `docs` rather than from this return.
func (c *Client) UpdateTableDocs(ctx context.Context, id string, in UpdateTableDocsInput) (*ColumnDocReport, error) {
	var out ColumnDocReport
	err := c.doSlow(ctx, "PUT", "feature/model/"+url.PathEscape(id), in, &out)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}
	if !out.Documented() {
		return nil, nil
	}
	return &out, nil
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
//   - It OVERWRITES the parent's fields from the draft, and a base-version CAS
//     decides whether it may: the draft records the committed version it forked
//     from, and a commit onto a table that has moved since is refused with a
//     409. Nothing is written when it is — the CAS runs inside the commit's own
//     transaction, under the parent's row lock — so the draft is intact.
//   - It CASCADES. The server emits a materialized event and dependent tables
//     are invalidated and rebuilt asynchronously — which is why the loop needs
//     no `run` verb, and why a publish is worth saying out loud.
//
// Refused on a shared feature for a non-admin, and possibly for an admin
// committing their own draft under a self-approval policy. Decide which branch
// to take from the feature's scope and the caller's role BEFORE calling, rather
// than from the rejection prose.
// confirmHeadVersionID is empty for an ordinary commit, which sends no body at
// all — the shape this route took before the CAS existed. Supply it only after a
// 409 has named the version being overwritten, and only on a deliberate
// override (`pipeline publish --overwrite-remote`). It must equal the head AT
// COMMIT TIME, not merely be non-empty, so a caller who learns the head and then
// deliberates while a third version lands gets another 409 — which is why
// nothing here retries.
func (c *Client) CommitTableDraft(ctx context.Context, draftID, confirmHeadVersionID string) error {
	var body any
	if confirmHeadVersionID != "" {
		body = TableCommitDraftInput{ConfirmHeadVersionID: confirmHeadVersionID}
	}
	return c.doSlow(ctx, "POST", "feature/model/"+url.PathEscape(draftID)+"/commit", body, nil)
}

// TableCommitDraftInput is the (optional) body of POST
// /feature/model/:id/commit, mirroring rmodelv2.CommitDraftInput
// (backend/resource/rmodelv2/model.go). Named apart from the workflow package's
// CommitDraftInput because both live in this one package; the wire shape is the
// same because the server's two types are.
//
// An empty body is the normal case and means "no override": the commit then
// requires the live table to still be at the version this draft forked from, and
// is refused with a 409 when it is not.
type TableCommitDraftInput struct {
	ConfirmHeadVersionID string `json:"confirmHeadVersionID,omitempty"`
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
