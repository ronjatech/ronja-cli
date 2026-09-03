package commands

import (
	"testing"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// One --stack is typed and the folders each declare their own. This table is
// what stops a mistyped stack name reporting a whole repository healthy, so
// every row that is not "check it" must carry a reason — and the caller counts
// every reason as unknown, never as clean.
func TestSyncResolveStackForFolder(t *testing.T) {
	const instance = "https://ronja.example"
	const elsewhere = "https://other.example"
	const org = "org-1"

	known := wfdir.InstanceKey{URL: instance, TenantID: org}
	// A $RONJA_TOKEN credential arrives knowing no organization, which is a
	// supported state and NOT a mismatch — selectNamed compares the organization
	// only when it is known, and this must agree with it.
	unknownOrg := wfdir.InstanceKey{URL: instance}

	stacks := func(url, tenant string) *wfdir.Manifest {
		return &wfdir.Manifest{Stacks: map[string]wfdir.Stack{"prod": {URL: url, TenantID: tenant}}}
	}

	tests := []struct {
		name       string
		manifest   *wfdir.Manifest
		lock       *wfdir.Lock
		key        wfdir.InstanceKey
		want       string
		wantReason string
	}{
		{
			name:     "declares the stack on this instance and organization",
			manifest: stacks(instance, org),
			key:      known,
			want:     "prod",
		},
		{
			name:       "declares it on another instance",
			manifest:   stacks(elsewhere, org),
			key:        known,
			want:       "prod",
			wantReason: syncReasonStackElsewhere,
		},
		{
			// ⚠️ ronja.json is HAND-EDITABLE, so the same instance arrives spelled
			// several ways. A raw `!=` made each of these `stack_elsewhere`,
			// skipped the folder and exited 2 — with a diagnostic printing two
			// URLs a reader cannot tell apart. wfdir.Matches is what the rest of
			// the module compares with; this pins that it is used here too.
			name:     "a trailing slash is the same instance",
			manifest: stacks(instance+"/", org),
			key:      known,
			want:     "prod",
		},
		{
			name:     "host capitalisation is the same instance",
			manifest: stacks("https://Ronja.example", org),
			key:      known,
			want:     "prod",
		},
		{
			name:       "declares it in another organization",
			manifest:   stacks(instance, "org-2"),
			key:        known,
			want:       "prod",
			wantReason: syncReasonStackElsewhere,
		},
		{
			// selectNamed treats this as "not a mismatch" and carries on, which is
			// right for a SINGLE folder: it is the signed-out case, and the local
			// half of that report is still worth having. A TREE must not, and the
			// difference is `sync check`'s dependency leg, which needs no network
			// — so an unresolved organization does not stop it giving a confident
			// `broken` about a folder belonging to somebody else entirely.
			//
			// It is never green either way. What it buys is that the REMOTE legs
			// do not run, so no id of ours is asked about another organization's
			// rows and reported unreachable.
			name:       "another organization cannot be confirmed when ours is unknown",
			manifest:   stacks(instance, "org-2"),
			key:        unknownOrg,
			want:       "prod",
			wantReason: syncReasonStackUnverified,
		},
		{
			// The same unknown organization against a stack that names NONE:
			// there is nothing to compare, so nothing to be unsure about.
			name: "a stack naming no organization is checked even when ours is unknown",
			manifest: &wfdir.Manifest{Stacks: map[string]wfdir.Stack{
				"prod": {URL: instance},
			}},
			key:  unknownOrg,
			want: "prod",
		},
		{
			name: "names stacks, and none of them is this one",
			manifest: &wfdir.Manifest{Stacks: map[string]wfdir.Stack{
				"dev": {URL: instance, TenantID: org},
			}},
			key:        known,
			want:       "prod",
			wantReason: syncReasonStackAbsent,
		},
		{
			name: "capitalisation is not a match",
			manifest: &wfdir.Manifest{Stacks: map[string]wfdir.Stack{
				"Prod": {URL: instance, TenantID: org},
			}},
			key:        known,
			want:       "prod",
			wantReason: syncReasonStackAbsent,
		},
		{
			name:       "a legacy instances[] folder names no stacks at all",
			manifest:   &wfdir.Manifest{Instances: []wfdir.Instance{{URL: instance, TenantID: org}}},
			key:        known,
			want:       "prod",
			wantReason: syncReasonUnnamedStack,
		},
		{
			name:     "a lock entry with no stack declaring it is adopted, as selectNamed adopts it",
			manifest: &wfdir.Manifest{},
			lock:     &wfdir.Lock{Stacks: map[string]wfdir.LockStack{"prod": {}}},
			key:      known,
			want:     "prod",
		},
		{
			name:     "no --stack checks every folder",
			manifest: &wfdir.Manifest{Instances: []wfdir.Instance{{URL: instance, TenantID: org}}},
			key:      known,
			want:     "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason, detail := resolveStackForFolder(tc.manifest, tc.lock, tc.key, tc.want)
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q (detail: %s)", reason, tc.wantReason, detail)
			}
			// A reason with no sentence behind it is a folder a reader cannot
			// act on, which is most of the value of not being green.
			if reason != "" && detail == "" {
				t.Errorf("reason %q carries no detail", reason)
			}
		})
	}
}

// The verdict helper is what the tree's exit code is computed from, so its
// no-baseline clause is the difference between a CI gate that works and one
// that goes green on a fresh checkout.
func TestSyncPipelineVerdict(t *testing.T) {
	report := func(tables ...pipelineTableReport) *pipelineStatusReport {
		return &pipelineStatusReport{
			Bound:  true,
			Remote: &pipelineRemoteReport{Checked: true, Tables: tables},
		}
	}

	tests := []struct {
		name string
		in   *pipelineStatusReport
		// localFiles is how many .sql files the folder holds — what decides
		// whether "nothing is deployed here yet" is clean or drifted. Zero for
		// every case that is not about that.
		localFiles int
		// undeployed is the lock-backed leg: committed SQL that differs from what
		// was last deployed. Nil for every case that is not about it.
		undeployed []string
		want       string
	}{
		{
			name: "every table compared and unchanged",
			in: report(
				pipelineTableReport{Path: "a.sql", Drift: driftNone},
				pipelineTableReport{Path: "b.sql", Drift: driftNone},
			),
			want: verdictClean,
		},
		{
			name: "a table changed on the server",
			in:   report(pipelineTableReport{Path: "a.sql", Drift: driftChanged}),
			want: verdictDrifted,
		},
		{
			name: "the live leg changed under an open draft",
			in:   report(pipelineTableReport{Path: "a.sql", Drift: driftNone, LiveDrift: driftChanged}),
			want: verdictDrifted,
		},
		{
			// THE CORRECTION. pipelineStatusVerdict exits zero here, and for a
			// single folder that is right; for a tree gate it is the fresh
			// `git clone` reporting clean because nothing was compared.
			name: "bound with nothing to compare against is unknown, not clean",
			in:   report(pipelineTableReport{Path: "a.sql", Drift: driftNoBaseline}),
			want: verdictUnknown,
		},
		{
			// THE FRESH-CLONE LEG. Every table's remote comparison passes — the
			// server still holds what the lock recorded — and the committed .sql
			// does not match it, so this repository holds SQL nobody deployed.
			// Nothing but the lock can say so on a checkout with no .ronja/.
			name:       "committed SQL that differs from what was last deployed is drifted",
			in:         report(pipelineTableReport{Path: "a.sql", Drift: driftNone}),
			undeployed: []string{"a.sql"},
			want:       verdictDrifted,
		},
		{
			// And it does not invent a second finding for a file the baseline leg
			// already named: one edited file is one change, not three.
			name:       "a file named by two local legs is reported once",
			in:         report(pipelineTableReport{Path: "a.sql", Drift: driftNone}),
			undeployed: []string{"a.sql"},
			want:       verdictDrifted,
		},
		{
			name: "a table that could not be read is unknown",
			in:   report(pipelineTableReport{Path: "a.sql", Drift: driftUnreadable}),
			want: verdictUnknown,
		},
		{
			// Unknown dominates drifted: the strongest true statement is that
			// not everything was verified.
			name: "one drifted and one unreadable is unknown",
			in: report(
				pipelineTableReport{Path: "a.sql", Drift: driftChanged},
				pipelineTableReport{Path: "b.sql", Drift: driftUnreadable},
			),
			want: verdictUnknown,
		},
		{
			// The folder IS bound and names a feature; the organization simply
			// holds nothing it has pushed. Clean only because there is nothing
			// here to deploy — see the case below, which is the same report with
			// one file on disk.
			name: "bound, nothing deployed, and an empty folder is clean",
			in: &pipelineStatusReport{
				Remote: &pipelineRemoteReport{NotCheckedReason: reasonNothingBound},
			},
			want: verdictClean,
		},
		{
			// THE FALSE GREEN. This report used to be read as clean whatever was
			// on disk, so a folder holding a whole table's SQL that had never
			// been pushed reported a healthy repository and exited zero.
			name:       "bound, nothing deployed, and a .sql file on disk is drifted",
			in:         &pipelineStatusReport{Remote: &pipelineRemoteReport{NotCheckedReason: reasonNothingBound}},
			localFiles: 1,
			want:       verdictDrifted,
		},
		{
			name: "any other not-checked reason is unknown",
			in: &pipelineStatusReport{
				Remote: &pipelineRemoteReport{NotCheckedReason: "not signed in"},
			},
			want: verdictUnknown,
		},
		{
			name: "a broken binding is unknown, not drift",
			in: &pipelineStatusReport{
				Remote: &pipelineRemoteReport{Checked: true, Problem: "binding broken"},
			},
			want: verdictUnknown,
		},
		{
			name: "no remote half at all is unknown",
			in:   &pipelineStatusReport{},
			want: verdictUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := verdictOfPipelineStatus(tc.in, tc.localFiles, tc.undeployed)
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q (detail: %s)", got.Verdict, tc.want, got.Detail)
			}
		})
	}
}

// The workflow and data-app loops never had a verdict of their own, and their
// no-baseline case reads WORSE than the pipeline one: DiffHashes against an
// absent baseline reports every remote file as added, so a fresh clone would
// otherwise be reported as wholesale drift — a confident answer to a question
// nobody asked.
func TestSyncFileDriftVerdict(t *testing.T) {
	dirty := &wfdir.Diff{Added: []string{"main.py"}}
	clean := &wfdir.Diff{}
	baseline := &baselineReport{SourceID: "wf-1"}

	tests := []struct {
		name     string
		remote   *remoteReport
		baseline *baselineReport
		// localFiles — see the pipeline table above.
		localFiles int
		want       string
	}{
		{
			name:     "compared and unchanged",
			remote:   &remoteReport{Checked: true, Drift: clean},
			baseline: baseline,
			want:     verdictClean,
		},
		{
			name:     "compared and changed",
			remote:   &remoteReport{Checked: true, Drift: dirty},
			baseline: baseline,
			want:     verdictDrifted,
		},
		{
			name:   "bound with no baseline is unknown, not wholesale drift",
			remote: &remoteReport{Checked: true, Drift: dirty},
			want:   verdictUnknown,
		},
		{
			name:     "the remote files could not be read",
			remote:   &remoteReport{Checked: true},
			baseline: baseline,
			want:     verdictUnknown,
		},
		{
			// Clean only because the folder holds nothing to deploy — see below.
			name:   "nothing created there yet, and an empty folder, is clean",
			remote: &remoteReport{NotCheckedReason: reasonNoWorkflowYet},
			want:   verdictClean,
		},
		{
			// THE FALSE GREEN, the workflow loop's copy of it: a whole workflow
			// on disk that has never been pushed used to report clean, exit 0.
			name:       "nothing created there yet, with source on disk, is drifted",
			remote:     &remoteReport{NotCheckedReason: reasonNoWorkflowYet},
			localFiles: 2,
			want:       verdictDrifted,
		},
		{
			name:   "any other not-checked reason is unknown",
			remote: &remoteReport{NotCheckedReason: "not signed in"},
			want:   verdictUnknown,
		},
		{
			name:   "a broken binding is unknown",
			remote: &remoteReport{Checked: true, Problem: "binding broken"},
			want:   verdictUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := verdictOfWorkflowStatus(&statusReport{Remote: tc.remote, Baseline: tc.baseline}, tc.localFiles)
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q (detail: %s)", got.Verdict, tc.want, got.Detail)
			}
		})
	}
}

// The data-app verdict reads the same five fields off its own report type, so
// the one thing worth pinning is that it really does read them — a struct
// literal that dropped a field would report every app clean.
func TestSyncAppVerdictReadsItsOwnReport(t *testing.T) {
	drifted := verdictOfAppStatus(&appStatusReport{
		Baseline: &baselineReport{SourceID: "app-1"},
		Remote:   &appRemoteReport{Checked: true, Drift: &wfdir.Diff{Modified: []string{"App.tsx"}}},
	}, 1)
	if drifted.Verdict != verdictDrifted {
		t.Errorf("verdict = %q, want %q", drifted.Verdict, verdictDrifted)
	}
	noBaseline := verdictOfAppStatus(&appStatusReport{
		Remote: &appRemoteReport{Checked: true, Drift: &wfdir.Diff{Added: []string{"App.tsx"}}},
	}, 1)
	if noBaseline.Verdict != verdictUnknown {
		t.Errorf("no-baseline verdict = %q, want %q", noBaseline.Verdict, verdictUnknown)
	}
	// Bound, nothing deployed, and an EMPTY folder: clean, because there is
	// nothing here to deploy.
	nothingYet := verdictOfAppStatus(&appStatusReport{
		Remote: &appRemoteReport{NotCheckedReason: reasonNoAppYet},
	}, 0)
	if nothingYet.Verdict != verdictClean {
		t.Errorf("nothing-yet verdict = %q, want %q", nothingYet.Verdict, verdictClean)
	}
	// The same report with an App.tsx on disk: an app that has never been
	// deployed, reported as a healthy folder. That was the false green.
	neverDeployed := verdictOfAppStatus(&appStatusReport{
		Remote: &appRemoteReport{NotCheckedReason: reasonNoAppYet},
	}, 1)
	if neverDeployed.Verdict != verdictDrifted {
		t.Errorf("never-deployed verdict = %q, want %q", neverDeployed.Verdict, verdictDrifted)
	}
}

// The tree verdict's precedence, which the exit-code contract rests on.
func TestSyncWorstVerdict(t *testing.T) {
	tests := []struct {
		in   []string
		want string
	}{
		{
			// ⚠️ `broken` is `sync check`'s middle answer where `sync status`'s is
			// `drifted` — one RANK, two words. Without a row for it here, deleting
			// verdictBroken from verdictRank makes a broken folder rank as CLEAN
			// and nothing fails: a whole command's exit code, unpinned.
			in:   []string{verdictClean, verdictBroken},
			want: verdictBroken,
		},
		{
			// And unknown beats broken, exactly as it beats drifted.
			in:   []string{verdictBroken, verdictUnknown},
			want: verdictUnknown,
		},
		{in: nil, want: verdictClean},
		{in: []string{verdictClean, verdictClean}, want: verdictClean},
		{in: []string{verdictClean, verdictDrifted}, want: verdictDrifted},
		{in: []string{verdictDrifted, verdictUnknown}, want: verdictUnknown},
		{in: []string{verdictUnknown, verdictDrifted}, want: verdictUnknown},
	}
	for _, tc := range tests {
		if got := worstVerdict(tc.in); got != tc.want {
			t.Errorf("worstVerdict(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
