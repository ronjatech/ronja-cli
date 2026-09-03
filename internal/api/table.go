package api

// The READ half of the table surface: the row, the caller's own draft, the
// review payload a confidence report renders, the feature's table list, and the
// one poll loop every build goes through.
//
// Split from table_write.go — the calls that change something — on the same
// workflow.go / workflow_write.go line, and mirrored under the same rule: every
// json tag below is copied from the server struct named in its comment, and the
// fixture test in table_test.go is what holds them in step. A tag that does not
// exist on the wire decodes to the zero value SILENTLY, which here would show up
// as an empty `code` (a push that "reverts" a colleague's table to nothing) or a
// missing buildVerdict (a failed build reported as success).

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strconv"
	"time"

	"github.com/ronjatech/ronja-cli/internal/tablerefs"
)

// Table kinds, mirrored from rdb.ModelKind
// (backend/ronja/rdb/table_model_v2.go). Only the two the pipeline loop reasons
// about are named: `derived` is what it manages, and `metric` is the other
// DRAFTABLE kind — it runs the identical checkout/sync/commit flow and is out of
// scope by choice, not by capability, so a message about it must never claim
// otherwise. The remaining kinds (foundation, integration, manual_integration,
// dynamic) are reported by their raw string; naming them here would be four
// constants nothing branches on.
const (
	TableKindDerived = "derived"
	TableKindMetric  = "metric"
)

// Table build statuses, mirrored from rdb.ModelStatus.
//
// The CLI reads BuildVerdict, not Status — see WaitForTableBuild. These exist
// only for the fallback that derives a verdict from an older server's response,
// which is the one place the raw status is still branched on.
const (
	TableStatusPending     = "pending"
	TableStatusInvalidated = "invalidated"
	TableStatusBuilding    = "building"
	TableStatusReady       = "ready"
	TableStatusBuildFailed = "build_failed"
)

// Draft lifecycle, mirrored from rdb.ShadowStatus. A row with no shadow status
// is a plain live table.
const (
	ShadowStatusDraft     = "draft"
	ShadowStatusCommitted = "committed"
)

// Build verdicts, mirrored from the buildVerdict* consts in
// backend/api/v2/feature/tableview.go.
//
// This is the field to branch on after a sync, and the reason it exists: a live
// table whose re-sync FAILS stays `ready` and goes on serving the previous
// build's data, so `status` alone reports a failure as success. The CLI branches
// on these by exact string, which makes them a wire contract in the same sense
// the device-flow poll codes are.
const (
	// BuildVerdictOK — the last run succeeded.
	BuildVerdictOK = "ok"
	// BuildVerdictOKPartial — queryable, but the build skipped corrupt source
	// files, so the data is incomplete.
	BuildVerdictOKPartial = "ok_partial"
	// BuildVerdictFailedStale — the last run FAILED and the table is still
	// serving the PREVIOUS build's data. `status` reads `ready` here.
	BuildVerdictFailedStale = "failed_stale"
	// BuildVerdictFailed — the run failed with no earlier data to fall back on.
	BuildVerdictFailed = "failed"
	// BuildVerdictBuilding — still running; keep polling.
	BuildVerdictBuilding = "building"
	// BuildVerdictPending — queued and not started. A sync of a table with no
	// `code` parks here forever, so a `pending` that never advances is a
	// failure, not progress.
	BuildVerdictPending = "pending"
	// BuildVerdictInvalidated — an upstream table changed; this one's data is
	// stale pending a rebuild.
	BuildVerdictInvalidated = "invalidated"
)

// PartialDataCorruptErrorType is the rjerr type string
// (rjerr.ERR_PARTIAL_DATA_CORRUPT) that marks the NON-FATAL "the build completed
// but skipped corrupt source files" notice. Anything else on a `ready` row is a
// real failure. Used only by the verdict fallback below.
const PartialDataCorruptErrorType = "partial_data_corrupt"

// Table is a HAND-MIRROR of the subset of feature.TableView the CLI reads
// (backend/api/v2/feature/tableview.go).
//
// A SUBSET, and a small one: TableView carries some sixty fields — metric
// recipe columns, build artifacts, PII state, move and soft-delete bookkeeping —
// and a mirrored field nobody reads is a field nobody keeps in step. The rule is
// "everything the pipeline loop reads, plus what makes a read field legible".
//
// Mirroring decisions, the same two as Workflow's:
//
//   - The server's optional.V[T] marshals as the bare value or JSON `null`, and
//     unmarshalling `null` into a non-pointer is a documented no-op — so every
//     optional STRING here is a plain string. "" is exactly what "unset" means
//     for a name, a parent id or a shadow status.
//   - InputModels carries no omitempty server-side, but a hand-written fixture
//     or an older instance can still send null, so Normalize replaces the nil.
type Table struct {
	ID        string `json:"id"`
	FeatureID string `json:"featureID"`

	Name string `json:"name"`
	// Kind is derived|foundation|integration|manual_integration|dynamic|metric.
	Kind string `json:"kind"`
	// Status is pending|invalidated|building|ready|build_failed. Read
	// BuildVerdict instead wherever you mean "did the build work" — see the
	// constants above.
	Status string `json:"status"`

	// Code is the table's SQL, in whichever ref form the row happens to store —
	// id form from an HTTP write, POSITIONAL form from any AI-assisted build.
	// Never hash or write this field raw; use CanonicalCode.
	Code        string   `json:"code"`
	InputModels []string `json:"inputModels"`

	// ParentModelID is empty on a live table and set on a draft (and on a
	// committed version snapshot). ShadowStatus says which.
	ParentModelID string `json:"parentModelID"`
	// ShadowStatus is draft|committed, empty on a live table.
	ShadowStatus string `json:"shadowStatus"`

	// BuildVerdict collapses `status` × "is there a failure on THIS ROW?" into
	// the one value to branch on after a sync.
	//
	// Populated ONLY by the single-row GET — never by the list endpoint, and
	// never on the create/checkout responses, which share the same DTO. ABSENT
	// means "not populated on this response", NOT "no verdict", so read it only
	// off a GetTable answer. WaitForTableBuild owns the fallback for an older
	// instance that does not send it at all.
	BuildVerdict string `json:"buildVerdict,omitempty"`

	// LastBuildError is the last recorded failure, absent when there is none.
	//
	// DRAFT-AWARE, deliberately, and that is not the same scope as BuildVerdict:
	// the server reads the table id PLUS the caller's own open draft, so on a
	// healthy live table you happen to hold a failed draft of, BuildVerdict is
	// `ok` while this describes the draft's failure. The pipeline loop always
	// polls the DRAFT's own id, where the two scopes coincide.
	LastBuildError *TableBuildError `json:"lastBuildError,omitempty"`

	UpdatedAt time.Time `json:"updatedAt"`

	// URL is the absolute frontend page for this row, stamped by the SERVER.
	// Never built here, and EMPTY IS NORMAL AND SILENT — see the identical field
	// on Workflow for why. Absent on the list routes and on rows the server
	// refuses to link (a committed version snapshot).
	URL string `json:"url,omitempty"`
}

// TableBuildError mirrors feature.TableBuildError: the last failure recorded
// against a table's build. The rows are cleared at the start of every rebuild,
// so a present value always describes the LATEST build.
type TableBuildError struct {
	// Message is the human-readable reason. For the commonest derived-table
	// failure — the SQL writer concluding the transformation is impossible with
	// the given inputs — this carries that full explanation, which is the single
	// most useful thing a failed push can print.
	Message string `json:"message"`
	// Type is the stable rjerr type string. PartialDataCorruptErrorType is the
	// non-fatal notice; anything else is an ordinary build failure.
	Type       string    `json:"type,omitempty"`
	OccurredAt time.Time `json:"occurredAt"`
}

// Normalize replaces a nil InputModels with an empty slice, so callers can range
// and compare without nil-checking.
func (t *Table) Normalize() {
	if t == nil {
		return
	}
	if t.InputModels == nil {
		t.InputModels = []string{}
	}
}

// IdentityID is the STABLE id of the table this row belongs to: the parent for a
// draft or a version snapshot, the row itself for a live table.
//
// This is what a manifest binding records, for the reason Workflow.IdentityID
// exists: a draft's id is per-user and disappears on commit, so a committed file
// naming one would point at a row that stops existing the moment its author
// publishes.
func (t *Table) IdentityID() string {
	if t == nil {
		return ""
	}
	if t.ParentModelID != "" {
		return t.ParentModelID
	}
	return t.ID
}

// CanonicalCode is Code in ID ref form, plus whether any positional ref could
// not be resolved.
//
// EVERY remote code read goes through this before it is hashed, written to disk
// or compared — that is what makes a local sha256 baseline comparable to a
// remote row at all. See package tablerefs for why the stored form is mixed and
// why positional is steady state rather than legacy.
func (t *Table) CanonicalCode() (string, bool) {
	if t == nil {
		return "", false
	}
	return tablerefs.Canonicalize(t.Code, t.InputModels)
}

// TableListItem is a HAND-MIRROR of the subset of rmodelv2.ModelV2QueryItem the
// CLI reads (backend/resource/rmodelv2/model.go).
//
// A separate type from Table rather than a reuse, because the LIST projection is
// a genuinely different shape and pretending otherwise would be a lie in the one
// direction that costs: it carries NO `code` and NO `buildVerdict`, so a caller
// handed a Table with both empty could not tell "this table has no SQL" from
// "this endpoint does not serve SQL". HasError is the list's own cheap health
// signal and exists on no other surface.
type TableListItem struct {
	ID        string `json:"id"`
	FeatureID string `json:"featureID"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	// ParentModelID / ShadowStatus are the draft markers. The list EXCLUDES
	// shadow rows by default (includeShadows), so in practice both are empty
	// here — they are mirrored so a caller can assert that rather than assume it.
	ParentModelID string `json:"parentModelID"`
	ShadowStatus  string `json:"shadowStatus"`
	// HasError is the list-only "something is recorded against this row" flag.
	// It is a HINT: it says a failure exists, never what it was, and never
	// whether the table is nonetheless serving good data. Read the single-row
	// GET's buildVerdict for the answer.
	HasError  bool      `json:"hasError"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// tableListResponse mirrors model.PaginationResponse[ModelV2QueryItem].
//
// Token is an OFFSET rendered as a string (table.WithToken), present only when
// more rows remain — so an absent token is the end of the list, not an error.
type tableListResponse struct {
	Token  string           `json:"token"`
	Total  uint64           `json:"total"`
	Result []*TableListItem `json:"result"`
}

// GetTable reads one table row. 404s for an id the caller cannot reach — the
// read gate does not distinguish absent from invisible.
//
// This is the only endpoint that serves BuildVerdict, LastBuildError and the
// full `code`, which is why the pipeline loop's status command pays for it per
// file instead of reading everything off the list.
func (c *Client) GetTable(ctx context.Context, id string) (*Table, error) {
	var out Table
	if err := c.Do(ctx, "GET", "feature/model/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return &out, nil
}

// GetTableDraft reads the CALLER'S OWN open draft of a table, returning
// (nil, nil) when they have none.
//
// The endpoint answers 200 with a JSON `null` body rather than 404 in that case,
// so decoding into a **Table is not a stylistic choice: decoding into a struct
// would turn "no draft" into a zero-value Table with an empty id, and every
// caller would then act on a draft that does not exist. Same shape, and same
// reasoning, as GetWorkflowDraft.
//
// `id` is the LIVE table's id; the row that comes back has its own, and that is
// the id the rest of the edit loop takes.
func (c *Client) GetTableDraft(ctx context.Context, id string) (*Table, error) {
	var out *Table
	if err := c.Do(ctx, "GET", "feature/model/"+url.PathEscape(id)+"/draft", nil, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return out, nil
}

// TableFieldDelta mirrors governance.TableFieldDelta — one column-level entry in
// a draft's schema diff.
//
// Change is "added" (only in the draft) or "removed" (only in live). The server
// compares column NAMES only: "retyped" is RESERVED and currently never
// emitted, and DraftType / LiveType are reserved with it — currently always
// absent. Mirrored anyway so the payload decodes if they start arriving; treat
// an empty one as "not reported" rather than as a type of "".
type TableFieldDelta struct {
	Name      string `json:"name"`
	Change    string `json:"change"`
	DraftType string `json:"draftType,omitempty"`
	LiveType  string `json:"liveType,omitempty"`
}

// TableInputModelsDelta mirrors governance.TableInputModelsDelta — the LINEAGE
// change committing a draft would make: the table ids its declared
// `input_models` gains and loses against the live row's.
//
// Mirrored where the payload's nameDelta and descriptionDelta are NOT, and the
// rule is the one this file's header states: a mirrored field nobody reads is a
// field nobody keeps in step. Lineage is the one of the three this loop
// DECLARES — `inputModels` is derived from the refs in the .sql file and sent on
// every write — so a delta here is a consequence of the file just pushed and
// belongs in the confidence report. A name is not: it is stamped once at create
// and never pushed again.
//
// A SET comparison server-side, so a pure REORDER of the same ids is reported as
// nothing. That gap is invisible to this loop by construction: the CLI sends a
// sorted list derived from id-form refs and refuses positional ones outright,
// which is what makes an order change meaningless here.
type TableInputModelsDelta struct {
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// TableInterveningVersion mirrors governance.TableInterveningVersion — one
// committed version of the live table that landed AFTER this draft forked.
type TableInterveningVersion struct {
	VersionID   string    `json:"versionID"`
	Name        string    `json:"name"`
	CommittedAt time.Time `json:"committedAt"`
}

// TableDraftReview mirrors governance.TableDraftReview — the diff payload behind
// a confidence report: what landing this draft would actually change.
//
// Computed entirely from state that already exists (nothing is built or
// executed), so it is cheap and safe to call in a loop.
//
// ⚠️ Note the SCOPE quirk, which is the one thing that can make this call fail
// on an otherwise working loop: the route lives at /api/v2/table/draft/:id/review
// in the ADMIN scope group, while the rest of the authoring loop (checkout / PUT
// / sync / commit) is `data`. A full-access PAT — the CLI default — is
// unaffected; a token scoped to `data` alone runs the whole loop except this.
type TableDraftReview struct {
	DraftID string `json:"draftID"`
	// TableID is the LIVE parent's id.
	TableID string `json:"tableID"`

	// The payload also carries draftSQL / liveSQL / drafterUserID / nameDelta /
	// descriptionDelta, which are deliberately NOT mirrored: this loop already
	// holds the SQL it just wrote, and a mirrored field nobody reads is a field
	// nobody keeps in step (see this file's header).

	// FieldsDelta is empty when the column sets match. It is only meaningful
	// once the draft has been SYNCED: an unbuilt draft has no fields and reports
	// every live column as removed.
	FieldsDelta []TableFieldDelta `json:"fieldsDelta"`

	// InputModelsDelta is the lineage change, ABSENT when the declared inputs
	// agree. Reported separately from the SQL because it is the one change a
	// draft can make with no SQL diff at all — a positional ref resolves by
	// INDEX into this list, so repointing it moves what the same text reads.
	InputModelsDelta *TableInputModelsDelta `json:"inputModelsDelta,omitempty"`

	// DraftRowCount / LiveRowCount are materialized row counts, or -1 for "not
	// available" (never built, no field stats yet). -1 IS NOT ZERO, and printing
	// it as one would report a table that was never built as a table that came
	// back empty.
	DraftRowCount int64 `json:"draftRowCount"`
	LiveRowCount  int64 `json:"liveRowCount"`

	// BaseStale is true when the live table advanced after this draft forked.
	// Landing a stale draft would revert those changes, so the commit call
	// REFUSES it with a 409 rather than letting it through — this plus
	// InterveningVersions is the warning that comes first, and it says what the
	// commit is about to say anyway.
	BaseStale bool `json:"baseStale"`
	// InterveningVersions are the versions that landed in between, newest
	// first. Present only when BaseStale.
	InterveningVersions []TableInterveningVersion `json:"interveningVersions"`
	// HeadVersionID is the live table's CURRENT head committed version, and the
	// value to send as confirmHeadVersionID to overwrite it on purpose.
	//
	// EMPTY is a real state, not "unknown": a table that has never been
	// committed to has no version to name, and the server returns empty rather
	// than falling back to the table's own id the way the workflow surface does.
	// Returned ALWAYS, not only when BaseStale — but it is a point-in-time read,
	// so a commit that carries it can still be refused if somebody lands a
	// version in between. That re-refusal is the point of an override that
	// names a version rather than a bare --force.
	HeadVersionID string `json:"headVersionID"`

	// SubmittedForReview is whether request-review has already been called.
	// `pipeline publish` reads it on the review route it takes and reports
	// "already submitted" instead of filing a second request: the server accepts
	// one and the draft is unharmed, but telling somebody their draft was just
	// submitted when an admin has had it since yesterday reports progress that
	// did not happen.
	SubmittedForReview bool `json:"submittedForReview"`
}

// GetTableDraftReview reads the diff payload for one DRAFT id. A row that is not
// an open draft is a 400 (the server's message says so; it is passed through).
func (c *Client) GetTableDraftReview(ctx context.Context, draftID string) (*TableDraftReview, error) {
	var out TableDraftReview
	if err := c.Do(ctx, "GET", "table/draft/"+url.PathEscape(draftID)+"/review", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// tableListPageSize is how many rows one list request asks for.
//
// The endpoint applies NO default and NO cap, so omitting the limit would fetch
// a whole feature in one response. A page size is asked for anyway, because the
// cost of a surprise is asymmetric: a feature with a thousand tables is unusual
// and a single unbounded response for it is a bad failure mode, while one extra
// round trip for a feature with two hundred is nothing.
const tableListPageSize = 200

// maxTableListPages bounds the pagination loop.
//
// The token is an OFFSET, so a server that answered with a non-advancing token
// would loop forever. This is not a limit on how many tables a feature may have
// (200 x 50 = 10 000, far past any real one) — it is a guard against a loop with
// no other terminating condition.
const maxTableListPages = 50

// ListFeatureTables reads every table belonging to one feature.
//
// Shadow rows are excluded by the endpoint's own default, so this is live rows
// only — which is what a clone wants. Filtering to `derived` is left to the
// CALLER rather than done here: the skipped kinds have to be REPORTED ("not
// cloned (kinds: dynamic, integration)"), and a client that dropped them
// silently could not say what it skipped.
//
// ⚠️ Pagination is offset-based, so a table created or deleted mid-walk can
// shift a row across a page boundary and be seen twice or not at all. Nothing
// here compensates: the fix would be a stable cursor the API does not offer, and
// pretending otherwise (deduplicating, retrying) would hide the skew rather than
// remove it. A clone that missed a table reports one fewer file; it does not
// corrupt anything.
func (c *Client) ListFeatureTables(ctx context.Context, featureID string) ([]*TableListItem, error) {
	var all []*TableListItem
	token := ""
	for page := 0; page < maxTableListPages; page++ {
		query := url.Values{}
		query.Set("featureID", featureID)
		query.Set("limit", strconv.Itoa(tableListPageSize))
		if token != "" {
			query.Set("token", token)
		}
		var out tableListResponse
		// Do joins the path to the instance verbatim, so a query string simply
		// rides along — this is the first caller in the package that needs one.
		if err := c.Do(ctx, "GET", "feature/model?"+query.Encode(), nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Result...)
		if out.Token == "" || out.Token == token || len(out.Result) == 0 {
			return all, nil
		}
		token = out.Token
	}
	return nil, fmt.Errorf("listing the tables of %s did not end after %d pages of %d — the instance keeps offering another page",
		featureID, maxTableListPages, tableListPageSize)
}

// TableBuildPoll bounds WaitForTableBuild's cadence and patience.
//
// A struct rather than five more flat fields on Client: the device flow already
// owns five poll knobs there, and a second flat set would leave every one of
// them needing its name to say which loop it belongs to.
type TableBuildPoll struct {
	// Interval is the wait between the first polls.
	Interval time.Duration
	// MaxInterval caps the backoff.
	MaxInterval time.Duration
	// Timeout bounds the whole wait. A non-positive value is DEFAULTED like
	// every other field rather than read as "no deadline": there is no caller
	// that wants a build watched forever, and a partially-set struct meaning
	// exactly that is a hang nobody asked for.
	Timeout time.Duration
	// MaxFailureWindow bounds an UNBROKEN run of failed polls by ELAPSED TIME.
	// Unbroken rather than cumulative: a poll that succeeds proves the instance
	// is reachable, so the window starts over.
	//
	// Time rather than a count, on the device flow's precedent
	// (DefaultMaxPollFailureWindow): a count silently means "count x interval",
	// so the real tolerance moved whenever the cadence did — three polls at 2.5s
	// is ~5 seconds, which is far less than an ordinary rolling deploy takes, and
	// a build that is running perfectly well server-side was reported as a lost
	// instance.
	MaxFailureWindow time.Duration
	// RateLimitBackoff is added to the interval on a 429, which is the global
	// rate limiter answering. It is the one transient failure where retrying at
	// the same cadence is actively wrong: the server is saying there is too much
	// traffic, and an unchanged cadence is a promise to keep producing it.
	RateLimitBackoff time.Duration
}

// The default poll schedule.
//
// The interval matches `wf test`'s (the web builder polls at 3s; 2.5s is the
// same order), and for the same reason it holds that cadence for the first few
// polls before backing off: most syncs of a small table finish in seconds, and
// backing off immediately would make the common case worse to protect the rare
// one.
//
// The timeout is generous because a derived build over a large partitioned input
// legitimately takes minutes, and giving up early on a healthy build reports a
// working instance as a broken one.
const (
	DefaultTableBuildPollInterval    = 2500 * time.Millisecond
	DefaultMaxTableBuildPollInterval = 10 * time.Second
	DefaultTableBuildTimeout         = 15 * time.Minute
	// DefaultMaxTableBuildFailureWindow is the same 90 seconds the device flow
	// tolerates, and for the same reason: a rolling deploy takes longer than any
	// small number of polls, and the overall Timeout is the independent backstop.
	DefaultMaxTableBuildFailureWindow = 90 * time.Second
)

// The backoff schedule, mirroring the one `wf test` uses: hold the starting
// cadence for the first polls, then grow by a factor until the cap.
const (
	tableBuildPollsAtStartInterval = 8
	tableBuildPollBackoffFactor    = 1.5
	// tableBuildPollJitterPercent spreads the cadence so that N pushes started
	// together by a CI matrix do not keep polling in lockstep for fifteen
	// minutes. Added rather than subtracted, so it can never shorten the
	// interval below the floor the schedule chose.
	tableBuildPollJitterPercent = 10
)

// defaultTableBuildPoll returns the schedule, so a zero-valued Client (one built
// by hand rather than through New) still polls sanely instead of hot-looping.
func defaultTableBuildPoll() TableBuildPoll {
	return TableBuildPoll{
		Interval:         DefaultTableBuildPollInterval,
		MaxInterval:      DefaultMaxTableBuildPollInterval,
		Timeout:          DefaultTableBuildTimeout,
		MaxFailureWindow: DefaultMaxTableBuildFailureWindow,
		RateLimitBackoff: DefaultRateLimitBackoff,
	}
}

// withDefaults fills in the knobs a caller left unset, FIELD BY FIELD.
//
// Not "replace the whole struct when Interval is zero", which is what this used
// to be: a caller that set Interval and Timeout — the natural thing to do in a
// test, or to shorten a wait — kept the failure tolerance at 0, and a budget of
// zero means the FIRST transient failure exceeds it. A poll loop that gives up on
// one dropped connection is worse than one with no budget at all, and nothing
// about setting two fields says "and disable the failure tolerance".
//
// EVERY field is defaulted the same way, Timeout included. A schedule with no
// deadline is not a thing any caller wants, so reading a zero there as "wait
// forever" only ever turned a partially-set struct into a hang.
func (p TableBuildPoll) withDefaults() TableBuildPoll {
	d := defaultTableBuildPoll()
	if p.Interval <= 0 {
		p.Interval = d.Interval
	}
	if p.MaxInterval <= 0 {
		p.MaxInterval = d.MaxInterval
	}
	if p.Timeout <= 0 {
		p.Timeout = d.Timeout
	}
	if p.MaxFailureWindow <= 0 {
		p.MaxFailureWindow = d.MaxFailureWindow
	}
	if p.RateLimitBackoff <= 0 {
		p.RateLimitBackoff = d.RateLimitBackoff
	}
	return p
}

// TableBuildResult is how a build ENDED.
//
// A failed build is DATA here, not an error: the caller renders the reason,
// leaves the draft in place to iterate on, and exits non-zero itself. An error
// return from WaitForTableBuild means something else — we stopped being able to
// ask.
type TableBuildResult struct {
	// Verdict is one of the BuildVerdict* constants. Never empty: an instance
	// that does not send one has it derived (see fallbackVerdict).
	Verdict string
	// Table is the last row read. Never nil on a nil error.
	Table *Table
	// Derived records that the verdict was computed client-side because the
	// instance did not send one — worth knowing before quoting it as the
	// server's answer.
	Derived bool
}

// OK reports a build that produced usable data. `ok_partial` counts: the table
// IS queryable, and the incompleteness is a warning to print rather than a
// reason to refuse.
func (r *TableBuildResult) OK() bool {
	return r != nil && (r.Verdict == BuildVerdictOK || r.Verdict == BuildVerdictOKPartial)
}

// ErrorMessage is the reason the build failed, empty when there is none.
func (r *TableBuildResult) ErrorMessage() string {
	if r == nil || r.Table == nil || r.Table.LastBuildError == nil {
		return ""
	}
	return r.Table.LastBuildError.Message
}

// tableBuildInFlight reports a verdict that is not yet an answer.
//
// `invalidated` is in flight rather than terminal because it means an upstream
// changed and a rebuild is coming — the row is between builds, not finished with
// one. The overall timeout is what stops that being unbounded.
func tableBuildInFlight(verdict string) bool {
	switch verdict {
	case BuildVerdictBuilding, BuildVerdictPending, BuildVerdictInvalidated:
		return true
	}
	return false
}

// fallbackVerdict derives a verdict the way the server does, for an instance
// that predates the field.
//
// It mirrors feature.tableBuildVerdict exactly, with ONE scope difference that
// has to be stated: the server feeds its version a ROW-SCOPED error read, while
// the only error the client has is LastBuildError, which is DRAFT-AWARE (the
// table plus the caller's own open draft). On a live table the caller holds a
// failed draft of, this therefore says `failed_stale` where the server would say
// `ok`.
//
// That is acceptable precisely because of how it is used: every wait in the
// pipeline loop polls the DRAFT's own id, where the two scopes are the same row.
// A caller that waits on a LIVE table on an old instance gets the pessimistic
// answer, which is the safe direction to be wrong in.
//
// An unrecognised status yields "", exactly as the server's does — a verdict we
// cannot name is better absent than guessed at.
func fallbackVerdict(t *Table) string {
	if t == nil {
		return ""
	}
	switch t.Status {
	case TableStatusBuilding:
		return BuildVerdictBuilding
	case TableStatusPending:
		return BuildVerdictPending
	case TableStatusInvalidated:
		return BuildVerdictInvalidated
	case TableStatusBuildFailed:
		return BuildVerdictFailed
	case TableStatusReady:
		switch {
		case t.LastBuildError == nil:
			return BuildVerdictOK
		case t.LastBuildError.Type == PartialDataCorruptErrorType:
			return BuildVerdictOKPartial
		default:
			return BuildVerdictFailedStale
		}
	}
	return ""
}

// WaitForTableBuild polls a table until its build reaches a verdict.
//
// EVERY command polls through this one function, so the `ready`-with-an-error
// trap is decoded in exactly one place. Pass the id of the row that was actually
// synced — in the pipeline loop that is always the DRAFT, which is also what
// keeps the draft-aware LastBuildError describing the row being waited on.
//
// A `failed` or `failed_stale` verdict comes back as a RESULT, not an error.
// Errors are reserved for losing contact with the instance, a cancelled context,
// and the deadline.
//
// The deadline case names the stuck-`pending` cause specifically, because it is
// the one failure with no error row to read: syncing a table with no `code`
// parks it at `pending` forever, and the server reports that nowhere else.
func (c *Client) WaitForTableBuild(ctx context.Context, id string) (*TableBuildResult, error) {
	poll := c.TableBuild.withDefaults()
	deadline := time.Now().Add(poll.Timeout)

	interval := poll.Interval
	// Zero means "no run of failures in progress"; set on the first transient
	// failure and cleared by any clean answer.
	var failingSince time.Time
	polls := 0
	var last *TableBuildResult

	for {
		row, err := c.GetTable(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return last, ctx.Err()
			}
			// A TERMINAL status ends the wait immediately. The failure window below
			// is for TRANSIENT trouble — a dropped connection, a 502 from a pod
			// restarting mid-deploy — and spending it on an answer that will never
			// change ends in "lost contact with the instance", which points at the
			// network for a problem that is nothing of the sort. Each of these has
			// one cause and it is worth naming.
			if err := terminalPollFailure(id, err); err != nil {
				return last, err
			}
			now := time.Now()
			if failingSince.IsZero() {
				failingSince = now
			}
			if failing := now.Sub(failingSince); failing > poll.MaxFailureWindow {
				return last, fmt.Errorf("lost contact with %s while waiting for the build of %s (%s of failures in a row, last: %w) — the build itself is unaffected and continues server-side",
					c.BaseURL, id, failing.Round(time.Second), err)
			}
			// A 429 is the global rate limiter, and the only transient failure that
			// says anything about load. The others resume the schedule's own
			// cadence.
			if StatusOf(err) == 429 {
				interval += poll.RateLimitBackoff
				if interval > poll.MaxInterval {
					interval = poll.MaxInterval
				}
			}
		} else {
			// A successful read proves the instance is reachable, so the window
			// for transient failures starts over.
			failingSince = time.Time{}
			verdict, derived := row.BuildVerdict, false
			if verdict == "" {
				verdict, derived = fallbackVerdict(row), true
			}
			last = &TableBuildResult{Verdict: verdict, Table: row, Derived: derived}
			// An UNRECOGNISED status yields no verdict at all. Ending the wait
			// on it reports what we saw; treating it as still-running would hang
			// until the deadline on a build that already finished.
			if !tableBuildInFlight(verdict) {
				return last, nil
			}
		}

		if time.Now().After(deadline) {
			return last, tableBuildTimeout(id, poll.Timeout, last)
		}

		polls++
		interval = nextTableBuildInterval(interval, polls, poll)
		wait := jitterTableBuildInterval(interval)
		// The deadline is checked against the wait BEFORE sleeping it, not only
		// after: sleeping past it and then issuing one more request spends a round
		// trip on an answer already known to be too late, and reports the deadline
		// a whole interval after it passed.
		if time.Now().Add(wait).After(deadline) {
			return last, tableBuildTimeout(id, poll.Timeout, last)
		}

		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// jitterTableBuildInterval spreads one wait by up to
// tableBuildPollJitterPercent, so concurrent pushes do not poll in lockstep.
//
// Only ever ADDS: a jitter that could subtract would let the cadence drop below
// the floor the schedule picked, which is the one property the interval is for.
func jitterTableBuildInterval(interval time.Duration) time.Duration {
	spread := int64(interval) / tableBuildPollJitterPercent
	if spread <= 0 {
		return interval
	}
	return interval + time.Duration(rand.Int64N(spread))
}

// terminalPollFailure names a poll failure that retrying cannot fix, or nil when
// the failure is the transient kind the budget exists for.
//
// The three statuses here are answers, not outages. 404 is the one that actually
// happens during a normal working day: the row being polled is a DRAFT, and a
// colleague committing it — or the author discarding it from the web UI — makes
// it stop existing mid-build. 401 and 403 are credential problems, and a
// credential does not recover by being used three more times.
//
// Everything else (5xx, 429, a dropped connection, a proxy hiccup) falls through
// to the consecutive-failure budget, which is what it is for.
func terminalPollFailure(id string, err error) error {
	switch StatusOf(err) {
	case 404:
		return fmt.Errorf("%s no longer exists while waiting for its build (%w).\n  A draft stops existing the moment it is committed or discarded, so this usually means somebody published or dropped it from the web UI while this command was watching — re-read the table to see where it landed",
			id, err)
	case 401:
		return fmt.Errorf("the credential was refused while waiting for the build of %s (%w) — a token expires or is revoked whether or not a build is running, so run `ronja login` and check the table's state afterwards; the build itself continues server-side",
			id, err)
	case 403:
		return fmt.Errorf("access to %s was refused while waiting for its build (%w) — retrying will not change that; the build itself continues server-side",
			id, err)
	}
	return nil
}

// tableBuildTimeout explains a wait that ran out, naming the codeless-sync cause
// when the row never left `pending`.
//
// That case gets its own sentence because it is not a slow build: a sync of a
// table with no `code` is ACCEPTED with a 200, dispatched, and parked back at
// `pending` with nothing recorded anywhere. Without this the only thing the
// caller sees is a build that took fifteen minutes and then stopped being
// watched, which points at the wrong problem entirely.
func tableBuildTimeout(id string, timeout time.Duration, last *TableBuildResult) error {
	if last != nil && last.Verdict == BuildVerdictPending {
		return fmt.Errorf("table %s never left `pending` after %s.\n  A build that is queued and never starts usually means the row has no `code` to run — a sync of a codeless table is accepted, dispatched, and parked back at `pending` forever, and the failure is recorded nowhere.\n  Check that the file you pushed is not empty, then sync again",
			id, timeout)
	}
	verdict := "unknown"
	if last != nil && last.Verdict != "" {
		verdict = last.Verdict
	}
	return fmt.Errorf("gave up waiting for the build of %s after %s (last verdict: %s) — it keeps going whether or not this command is watching",
		id, timeout, verdict)
}

// nextTableBuildInterval is the wait before poll number polls+1, given the wait
// that preceded poll number polls.
//
// A separate function because the SCHEDULE is the thing worth pinning: it has to
// hold the starting cadence for the first polls and it has to stop growing, and
// neither property is visible from inside the loop.
func nextTableBuildInterval(interval time.Duration, polls int, poll TableBuildPoll) time.Duration {
	if polls <= tableBuildPollsAtStartInterval || interval >= poll.MaxInterval {
		return interval
	}
	grown := time.Duration(float64(interval) * tableBuildPollBackoffFactor)
	if grown > poll.MaxInterval {
		return poll.MaxInterval
	}
	return grown
}
