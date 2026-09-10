package api

// The two verbs that AUTHOR a metric: mint a kind=metric row from a recipe, and
// stage a new recipe on a DRAFT of one.
//
// Everything else in a metric's lifecycle is already in table_write.go and is
// kind-generic — checkout, sync, commit, discard and request-review all work on
// a metric exactly as they work on a derived table, which is what lets the
// pipeline loop drive a metric with no third lifecycle invented. These two are
// the whole of what a metric needs beyond that.
//
// SAME HAND-MIRROR RULE as everywhere else in this package: every tag below is
// copied from the server struct named in its comment
// (backend/api/v2/feature/api_metric_authoring.go), and the fixture tests hold
// them in step.
//
// ⚠️ THE REQUEST TYPES ARE MIRRORED WITH PLAIN GO TYPES AND `omitempty`, NEVER
// WITH THE SERVER'S optional.V. An optional.V is a struct: `omitempty` cannot
// suppress it, and an empty one marshals as `null` — so a body built around one
// would carry `"reportingTimezone": null` on every write, and a route that reads
// an explicit null as "reset this to UTC" would then reset it on every push.
// CreateTableInput and UpdateTableDocsInput dodge the same trap the same way.

import (
	"context"
	"encoding/json"
	"net/url"
)

// Metric verification statuses, the COMPLETE mirror of rdb.MetricStatus
// (backend/ronja/rdb/table_model_v2.go), which declares exactly these three and
// registers them as an enum.
//
// Only `verified` is branched on in the loop, and it is the one that has to be:
// it is the admin's assertion that this definition of the company's number was
// checked, and overwriting a colleague's committed change to a verified metric
// is the one destructive act in the whole pipeline loop. `unvetted` appears in
// the harness fixtures as the state a freshly created metric lands in.
//
// ⚠️ `retired` has no reference anywhere, tests included, and is kept anyway —
// the value of a mirror is that it is COMPLETE. Two-thirds of an enum reads as
// if the axis had two states, which is how a later reader comes to write
// `!= verified` where they meant `== unvetted`. The server's own list is the
// thing to check this against when the axis grows.
const (
	MetricStatusUnvetted = "unvetted"
	MetricStatusVerified = "verified"
	MetricStatusRetired  = "retired"
)

// CreateMetricInput is the body of POST /feature/model/metric, mirroring the
// subset of feature.MetricCreateInput the CLI writes.
//
// WHAT IS NOT HERE IS THE POINT, and the server's own type says so at length:
// there is no field for metricStatus, verifiedAt, verifiedBy or
// verifiedAgainstDefinitionHash (a body carrying them could mint a row asserting
// it is a verified metric attributed to someone else), and none for anything
// DERIVED from the recipe — additivity, the time and value columns, the
// dimension list, the compiled SQL, the source closure. Sending a derived column
// is how a metric comes to contradict its own definition, and the endpoint
// exists to make that impossible.
//
// ⚠️ There is no `ownerID` here either, and that is a decision rather than an
// omission. The server defaults it to the caller, and the caller is whoever ran
// the push — which is the honest answer for a folder: the person deploying it is
// the one who can be asked about the number. A folder-declared owner would be a
// user id committed to git, which goes stale the day that person leaves and
// names somebody who cannot be reached from the file that names them.
type CreateMetricInput struct {
	Name      string `json:"name"`
	FeatureID string `json:"featureID"`
	// Recipe is the whole definition, carried as raw bytes — see
	// Table.MetricRecipe for why it is never decoded and re-encoded on the way
	// through.
	Recipe json.RawMessage `json:"recipe"`

	// Description is the metric's prose, from the file.
	//
	// ⚠️ ABSENT AND AN EXPLICIT "" ARE THE SAME INSTRUCTION HERE, which is why
	// the field is a plain string with omitempty rather than the pointer the
	// recipe route's timezone is. The server's own field is optional, but the
	// handler collapses it (`in.Description.GetWithDefault("")`) and the create
	// stamps prose — and `descriptionSource=user` with it — only when the
	// trimmed value is non-empty. There is no "the author says there is none"
	// state anywhere in the model for a body to claim: the row lands with no
	// description either way.
	//
	// And nothing fills the gap in behind the file. A metric created over HTTP is
	// NOT auto-described — the describer runs in the background at the end of the
	// createMetric CHAT tool, and on no other create path — so a file that says
	// nothing leaves a metric that says nothing until somebody writes some. See
	// valueOrEmpty in the pipeline loop, which flattens the file's three-state
	// claim for exactly this reason.
	Description string `json:"description,omitempty"`
	// ReportingTimezone is the IANA name the metric is EVALUATED at, for every
	// viewer. omitempty for the same three-state reason: absent takes the
	// organization default.
	ReportingTimezone string `json:"reportingTimezone,omitempty"`
}

// CreateMetricResult mirrors feature.MetricCreateResult.
//
// The warnings are not cosmetic and are reported rather than swallowed: "the
// source closure could not be resolved" means the row landed with an EMPTY
// closure and the engine will walk its sources live, and a session-table
// promotion is a permanent rebind of another row. Both are things the push did
// not ask for.
type CreateMetricResult struct {
	Metric   *Table   `json:"metric"`
	Warnings []string `json:"warnings,omitempty"`
}

// WriteMetricRecipeInput is the body of PUT /feature/model/:id/metric/recipe,
// mirroring feature.MetricRecipeInput.
//
// `id` is a DRAFT's id, always. The route refuses a live metric outright, for
// every caller including an admin, and that refusal is what makes it safe to
// publish at all: a recipe written onto a live row would serve numbers from the
// new definition while definition_hash still equalled
// verified_against_definition_hash — a permanent "verified · clean" over a
// definition nobody vetted.
type WriteMetricRecipeInput struct {
	// Recipe is the FULL new definition (replace semantics — a partial recipe is
	// not merged into the stored one). omitempty so a timezone-only write leaves
	// the draft's recipe alone.
	Recipe json.RawMessage `json:"recipe,omitempty"`

	// ReportingTimezone is three-state on the wire and therefore a POINTER here:
	// ABSENT leaves the draft's declaration alone, an explicit "" resets it to
	// the UTC literal, and a value re-declares it. Collapsing it into a plain
	// string with omitempty would make "reset" unexpressible and would silently
	// mean "leave alone" — which is the wrong half of the pair to lose, because
	// a folder that deleted its `reportingTimezone` key means to stop declaring
	// one.
	ReportingTimezone *string `json:"reportingTimezone,omitempty"`

	// BaseRecipeSha256 is a PRECONDITION rather than a column: the sha256 of the
	// metricRecipe this client read off THIS DRAFT, lowercase hex over the raw
	// bytes.
	//
	// omitempty is load-bearing, exactly as it is on UpdateTableInput's
	// BaseCodeSha256 and for the identical reason. The server reads three states:
	// ABSENT is "write unconditionally", a 64-hex digest is compare-and-swap, and
	// "" is a 400 rather than a silent no-check — deliberately, so a caller who
	// thought they were asserting something is told they were not. Without the
	// tag every unconditional write would send `""` and 400.
	//
	// ⚠️ It must be a digest of bytes the SERVER sent. The stored recipe is
	// canonical (its version defaulted, its source a real id); the local file
	// carries aliases and may omit a default, so a hash of the file is refused
	// with a 409 describing a conflict nobody caused.
	BaseRecipeSha256 string `json:"baseRecipeSha256,omitempty"`
}

// MetricRecipeResult mirrors feature.MetricRecipeResult: what the write staged
// on the draft.
//
// Every field but `recipe` is COMPUTED from the write rather than re-read
// (`recipe` is read back off the draft, so that the bytes it hands out are the
// ones a later baseRecipeSha256 is taken over — see the server's own note). Both
// ways, nothing here describes the LIVE metric: nothing on the live row has
// changed until the draft is built and committed.
type MetricRecipeResult struct {
	DraftID  string `json:"draftID"`
	MetricID string `json:"metricID"`

	// Recipe is the CANONICAL recipe now staged: the same definition with its
	// version defaulted and its source resolved. These are the bytes to hash for
	// a later baseRecipeSha256 — raw, for Table.MetricRecipe's reason.
	Recipe json.RawMessage `json:"recipe"`

	Additive    bool     `json:"additive"`
	Dimensions  []string `json:"dimensions"`
	TimeColumn  string   `json:"timeColumn"`
	InputModels []string `json:"inputModels"`
	// SourceClosure EMPTY means the resolve did not succeed and the engine will
	// walk the sources live — a degrade, never a statement that the metric reads
	// nothing. The server normalises it to `[]`; Normalize below covers an older
	// instance, and the two other slices, which it does not.
	SourceClosure []string `json:"sourceClosure"`

	ReportingTimezone string   `json:"reportingTimezone"`
	Warnings          []string `json:"warnings,omitempty"`
}

// Normalize replaces the nil slices with empty ones.
//
// Three of the four are NOT normalised server-side — only sourceClosure is — so
// `dimensions`, `inputModels` and an older instance's `sourceClosure` can each
// arrive as JSON `null`. Callers range over them and compare them, and a nil
// that reached a report as "no dimensions" would be indistinguishable from a
// metric that really declares none.
func (r *MetricRecipeResult) Normalize() {
	if r == nil {
		return
	}
	if r.Dimensions == nil {
		r.Dimensions = []string{}
	}
	if r.InputModels == nil {
		r.InputModels = []string{}
	}
	if r.SourceClosure == nil {
		r.SourceClosure = []string{}
	}
}

// CreateMetric mints a kind=metric row from a recipe.
//
// The row it returns is LIVE and UNBUILT: definitionHash is null and
// metricStatus is `unvetted` until something syncs it and an admin verifies it.
// Live, exactly as CreateTable's is — colleagues can see it the moment it exists
// — which is why the loop records the binding before it attempts anything else.
//
// Admin-only server-side, matching POST /feature/model rather than the
// createMetric agent tool (which additionally lets a non-admin create in their
// own private feature). Editing a metric that already exists is not admin-only,
// and that asymmetry is what the refusal message has to say.
//
// doSlow rather than Do, for CreateTable's reason and one more: this create
// COMPILES the recipe, extracts its lineage and resolves the source closure
// inside the request.
func (c *Client) CreateMetric(ctx context.Context, in CreateMetricInput) (*CreateMetricResult, error) {
	var out CreateMetricResult
	if err := c.doSlow(ctx, "POST", "feature/model/metric", in, &out); err != nil {
		return nil, err
	}
	out.Metric.Normalize()
	return &out, nil
}

// WriteMetricRecipe stages a new definition on a metric DRAFT.
//
// `draftID` is the DRAFT's id — writing the live metric's id is not silently
// lost work here the way a table PUT is, it is REFUSED, which is the whole
// safety property of the route.
//
// It does not build. The draft's next `POST /:id/sync` is what re-derives the
// definition hash, exactly as the rest of the pipeline loop already works.
func (c *Client) WriteMetricRecipe(ctx context.Context, draftID string, in WriteMetricRecipeInput) (*MetricRecipeResult, error) {
	var out MetricRecipeResult
	if err := c.doSlow(ctx, "PUT", "feature/model/"+url.PathEscape(draftID)+"/metric/recipe", in, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return &out, nil
}
