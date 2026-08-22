package commands

import (
	"fmt"
	"reflect"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The METADATA a push patches onto the row — the title, the entrypoint, the
// parameters and the declared calendar — split out of workflow_push.go alongside
// workflow_push_zone.go so the state machine there reads as the sequence of
// phases it is rather than as a phase list with three field-level rules folded
// into the middle of it.
//
// Four things live here and nothing else does: building the patch
// (metadataPatch), the one comparison it needs that `==` cannot do
// (sameParameters), mirroring an ACCEPTED patch back onto the in-memory row
// (applyPatch), and the question the up-to-date path asks of the baseline
// (baselineDescribes). The declared zone's own rules stay in
// workflow_push_zone.go — it is the one field a 2xx does not confirm, so it has
// a lifecycle none of these share.

// metadataPatch is the difference between what the manifest says and what the
// row says. Empty when they agree, so the round trip is skipped.
//
// A blank manifest field is skipped rather than sent as "": the server's Patch
// fields are optionals, and an explicit empty string CLEARS the value rather
// than leaving it alone.
func metadataPatch(manifest *wfdir.Manifest, target *api.Workflow) api.WorkflowPatch {
	var patch api.WorkflowPatch
	if manifest.Title != "" && manifest.Title != target.Title {
		patch.Title = manifest.Title
	}
	if manifest.Entrypoint != "" && manifest.Entrypoint != target.Entrypoint {
		patch.Entrypoint = manifest.Entrypoint
	}
	// A folder that does not manage parameters never patches them — that is the
	// whole point of the absent/empty distinction, and it is what keeps a push
	// from a pre-parameters folder off a workflow's declaration.
	if manifest.ManagesParameters() {
		declared := manifest.DeclaredParameters()
		if !sameParameters(declared, target.Parameters) {
			// Normalized, so "declares none" always goes up as [] rather than
			// null — the server reads null as absent, which would be a silent
			// no-op exactly when the author is removing their last parameter.
			out := append([]api.WorkflowParameter{}, declared...)
			patch.Parameters = &out
		}
	}
	// The declared execution calendar, on the same terms: an unmanaged folder
	// never patches it, so a folder created before the key existed cannot
	// re-bucket a workflow's calendar by being pushed.
	//
	// It rides this same PUT as the title and the entrypoint, which needs no
	// ordering of its own: the zone is a scalar the server validates on its own
	// terms (rworkflow.resolveDeclaredZonePatch), unrelated to the file set — the
	// entrypoint's write-before-patch / delete-after-patch rule exists because the
	// server refuses an entrypoint the row holds no file for, and nothing
	// equivalent constrains a timezone.
	if manifest.ManagesReportingTimezone() {
		declared := manifest.DeclaredReportingTimezone()
		if effectiveDeclaredZone(declared) != target.ReportingTimezone {
			// Sent VERBATIM, so a folder declaring the reset value sends "" and
			// the server writes the literal "UTC" — the one form of the reset the
			// API accepts.
			zone := declared
			patch.ReportingTimezone = &zone
		}
	}
	// The runtime upgrade. Only ever RAISED, and only from a runtime the server
	// actually told us about: a row whose runtimeVersion came back 0 is an
	// instance that did not say (the field predates this CLI's contract with it),
	// and patching a runtime on that guess would be changing semantics on a hunch.
	// The DOWNGRADE half is not expressible here at all — checkRuntimeDrift
	// refuses it before the push writes anything, because a folder that declares
	// runtime 1 against a Durable row is a mistake to report, not a patch to skip.
	if target.RuntimeVersion > 0 && manifest.RuntimeVersion() > target.RuntimeVersion {
		patch.RuntimeVersion = manifest.RuntimeVersion()
	}
	return patch
}

// checkRuntimeDrift refuses the ONE runtime difference a push can never resolve:
// a folder declaring the standard runtime against a workflow that is already
// Durable.
//
// It is a refusal rather than a silent skip because the two readings of that
// state are far apart — "my ronja.json predates the upgrade somebody made in the
// browser" and "I want this workflow back on runtime 1" — and the second is
// impossible: a Durable workflow's journal is keyed by the v2 derivation, so the
// server refuses the downgrade too. Silently pushing code written for one runtime
// at a workflow running the other is the outcome worth spending an error on.
//
// Runs against the LIVE row, before the push writes anything — the point is to
// fail before a file lands, not after.
//
// A row reporting runtime 0 said nothing (an instance older than this contract),
// and an unbound folder has no row at all: both answer "no opinion" rather than
// a refusal built on a missing value.
func checkRuntimeDrift(f *folder, live *api.Workflow) error {
	if live == nil || live.RuntimeVersion == 0 {
		return nil
	}
	if f.Manifest.RuntimeVersion() >= live.RuntimeVersion {
		return nil
	}
	return fmt.Errorf("this folder declares runtime %d, but %s is already runtime %d (Durable) on the server — the runtime is a one-way upgrade and cannot be lowered.\n  Set \"runtime\": %d in %s to match, or clone the workflow again into a fresh folder",
		f.Manifest.RuntimeVersion(), live.ID, live.RuntimeVersion,
		live.RuntimeVersion, wfdir.ManifestPath(f.Root))
}

// sameParameters compares two declarations for sync purposes. Order is
// significant: it is the order the parameters are presented in, the manifest
// controls it, and reordering is a change worth pushing.
//
// reflect.DeepEqual is safe here despite DefaultValue being `any`: both sides
// arrive by JSON decode (the manifest from ronja.json, the row from the API), so
// a number is float64 on both and there is no int/float64 mismatch to trip on.
func sameParameters(a, b []api.WorkflowParameter) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// applyPatch mirrors an ACCEPTED metadata patch onto the in-memory row.
//
// PUT :id answers with nothing, so this is the only way `target` keeps
// describing the server without the re-read the success path does anyway.
//
// The declared zone is deliberately NOT mirrored here — confirmDeclaredZone
// owns it, because it is the one field whose acceptance a 2xx does not prove.
func applyPatch(row *api.Workflow, patch api.WorkflowPatch) {
	if patch.Title != "" {
		row.Title = patch.Title
	}
	if patch.Entrypoint != "" {
		row.Entrypoint = patch.Entrypoint
	}
	if patch.Parameters != nil {
		row.Parameters = *patch.Parameters
	}
	if patch.RuntimeVersion != 0 {
		row.RuntimeVersion = patch.RuntimeVersion
	}
}

// baselineDescribes reports whether the recorded baseline already describes this
// row — the same id, and the same metadata. False for a baseline written before
// the metadata was recorded at all, which is exactly when it wants replacing.
func baselineDescribes(baseline *wfdir.InstanceState, row *api.Workflow) bool {
	return baseline != nil &&
		baseline.SourceID == row.ID &&
		baseline.Title == row.Title &&
		baseline.Entrypoint == row.Entrypoint &&
		// An unrecorded parameter set (a baseline predating the guard) does not
		// make the baseline stale on its own — same treatment the metadata
		// fields get, and the next push records it.
		(baseline.Parameters == nil || sameParameters(*baseline.Parameters, row.Parameters)) &&
		// Same treatment for an unrecorded declared zone: nil is a baseline
		// written before it was part of one, not a row that lost its declaration.
		(baseline.ReportingTimezone == nil || *baseline.ReportingTimezone == row.ReportingTimezone)
}
