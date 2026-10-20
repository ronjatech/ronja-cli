package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A workflow's declared reporting timezone as a push sees it. The rule every
// function here keeps: an empty declaration and the literal "UTC" are DIFFERENT
// states.

// effectiveDeclaredZone maps an explicit "" to the literal "UTC" the server
// stores for it (rworkflow.resolveDeclaredZonePatch), or a folder declaring the
// reset would patch on every push. A row's own empty value means it declares
// nothing, a permanent state distinct from UTC.
func effectiveDeclaredZone(declared string) string {
	if declared == "" {
		return utcZone
	}
	return declared
}

// utcZone mirrors lib/reportingtz.UTC.
const utcZone = "UTC"

// declaredZoneForCreate returns "" (the key is omitempty) for a folder that does
// not manage the zone.
func declaredZoneForCreate(manifest *wfdir.Manifest) string {
	if !manifest.ManagesReportingTimezone() {
		return ""
	}
	return effectiveDeclaredZone(manifest.DeclaredReportingTimezone())
}

// confirmDeclaredZone reads the row back because an instance older than the
// column drops `reportingTimezone` and still answers 2xx. `target` takes the
// server's value, never the one sent, or the baseline would claim an unconfirmed
// declaration. known=false means the read failed: `target` still holds the
// pre-patch zone, so the caller must record nothing for it.
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

// describeZone renders an empty row zone as "none": it falls back to the
// caller's zone at run time.
func describeZone(zone string) string {
	if zone == "" {
		return "none"
	}
	return zone
}
