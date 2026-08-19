package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The DECLARED reporting timezone of a workflow (workflows.reporting_timezone,
// migration 000499) as a PUSH sees it, split out of workflow_push.go so the
// whole rule reads in one place — mirroring rmodelv2/store_zone.go, which does
// the same for the server side of the same column.
//
// Four things live here and nothing else does: the value the server ends up
// holding for a declaration (effectiveDeclaredZone / utcZone), the value a first
// push CREATES with (declaredZoneForCreate), the read-back that finds out
// whether a patched declaration actually landed (confirmDeclaredZone), and the
// way a row's declaration is rendered to a human (describeZone).
//
// They are scattered across the push's phases — create, patch, drift report —
// which is exactly why they belong together: the rule they share is that an
// empty declaration and the literal "UTC" are DIFFERENT states, and every one of
// them exists to keep the two apart.

// effectiveDeclaredZone is what the SERVER ends up holding for a declared zone,
// which is not always what the folder wrote: an explicit "" resets the row to the
// literal "UTC" (rworkflow.resolveDeclaredZonePatch), never to NULL.
//
// Comparing the raw declaration against the row instead would make a folder that
// declares the reset value patch on every single push — "" never equals the "UTC"
// the server just wrote — and no push would ever report itself up to date.
//
// A row's OWN empty value is left alone by this: it means the row declares
// nothing at all — a permanent state, not a migration backlog — which is
// different from UTC and one the folder can only move away from, never back to.
func effectiveDeclaredZone(declared string) string {
	if declared == "" {
		return utcZone
	}
	return declared
}

// utcZone is the literal the server stores for an explicit reset, mirrored from
// lib/reportingtz.UTC.
const utcZone = "UTC"

// declaredZoneForCreate is the zone a first push creates the workflow with: the
// folder's declaration in the form POST /workflow accepts, or "" (the key is
// omitempty) for a folder that does not manage it.
func declaredZoneForCreate(manifest *wfdir.Manifest) string {
	if !manifest.ManagesReportingTimezone() {
		return ""
	}
	return effectiveDeclaredZone(manifest.DeclaredReportingTimezone())
}

// confirmDeclaredZone finds out whether the declared calendar actually LANDED,
// by reading the row back, and reports whether it did.
//
// It exists because of a version skew that is otherwise completely silent. The
// zone rides `reportingTimezone` on POST /workflow and PUT /workflow/:id; an
// instance older than that column binds the body without
// DisallowUnknownFields, so the key is DROPPED and the request still answers
// 2xx. Every other field a push writes is confirmed by the write itself (a
// rejected file PUT is an error, a bad entrypoint is refused); this one is
// accepted, discarded, and reported as success. Without the read-back a folder
// that declares `Europe/Stockholm` pushes clean forever against a server that
// runs every one of its workflows on the caller's zone, which is exactly the
// defect the declaration exists to fix.
//
// It answers TWO questions, and they are not the same one:
//
//	applied — did the row end up holding what we sent? Drives the success report,
//	          so an ignored key is never listed as an applied change.
//	known   — do we know what the row holds AT ALL? False only when the read
//	          itself failed.
//
// Three consequences, and all three are the point:
//   - `target` takes the SERVER's value, never the one we sent. `target` is what
//     the baseline records if the deletions below fail, so baking an unconfirmed
//     value in would have the folder claim a declaration the row does not hold —
//     `wf status` would then report clean, and no later push would ever retry it.
//   - an OLD server that dropped the key is `applied=false, known=true`: we read
//     the row, it declares nothing, and recording that empty value is exactly
//     right — it is what the server holds.
//   - a read that FAILED is `known=false`, which is a different state entirely.
//     `target` still carries the PRE-patch zone (applyPatch deliberately leaves
//     the field alone), so the caller must record NOTHING for it rather than that
//     stale value — otherwise the next push compares the row against a baseline
//     describing the world before our own successful patch and refuses, naming a
//     third party who changed nothing. The push is not stopped for the failed
//     read: the patch was accepted and the files are written, and failing a whole
//     sync over a diagnostic GET would be the worse answer.
//
// Costs one extra GET, and only on a push that actually changes the zone — the
// patch is empty whenever the manifest and the row already agree.
func confirmDeclaredZone(ctx context.Context, client *api.Client, target *api.Workflow, sent string, result *pushResult) (applied, known bool) {
	want := effectiveDeclaredZone(sent)
	row, err := client.GetWorkflow(ctx, target.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: could not read %s back to confirm the reporting timezone (%v) — nothing is recorded for it from this step. A push that COMPLETES records whatever its closing re-read finds; one that stops before then leaves the calendar out of the local baseline, so the next push tries again.\n",
			target.ID, err)
		return false, false
	}
	target.ReportingTimezone = row.ReportingTimezone
	if row.ReportingTimezone == want {
		return true, true
	}
	result.ReportingTimezoneIgnored = true
	if row.ReportingTimezone == "" {
		fmt.Fprintf(os.Stderr, "  Warning: this server does not support reportingTimezone — the key was ignored and %s still declares no calendar.\n    Everything else in this push landed. The declaration is not recorded locally either, so `ronja wf status` keeps showing it as unsynced and a later push against an upgraded instance applies it.\n",
			target.ID)
		return false, true
	}
	fmt.Fprintf(os.Stderr, "  Warning: %s reports its reporting timezone as %q, not the %q this push sent — recording what the server holds.\n",
		target.ID, row.ReportingTimezone, want)
	return false, true
}

// describeZone renders a ROW's declared zone for a human. Empty means the row
// declares none — falling back to the caller's zone at run time — which "none"
// says and `""` does not.
func describeZone(zone string) string {
	if zone == "" {
		return "none"
	}
	return zone
}
