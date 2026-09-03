package commands

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja automation` is the CLI's fifth folder-sync loop, after `wf`, `app`,
// `pipeline` and `db`.
//
// It is the SMALLEST of them, and the smallness is structural rather than a
// staging decision. `scheduled_jobs` has no draft, no version history and no
// build, so there is nothing to check out, nothing to publish, nothing to
// discard and no confidence report to render. What is left is the part a sync
// loop is actually for: a committed declaration, an id recorded per stack, and a
// set of refusals that keep a push from doing something nobody asked for.
//
// Three ways it differs from `pipeline`, which is otherwise its closest
// relative:
//
//  1. THE FILE IS A CURATED SCHEMA, not the wire row. The row and the create /
//     update bodies disagree about two field names, and the row carries state a
//     committed file must not own (nextRunAt, disabledReason, the email and
//     webhook tokens, run health). See automationFile.
//  2. ALIASES RESOLVE BY FIELD, not through markers. An automation is a
//     structured document whose id-bearing fields are known and finite, and the
//     kind it most needs — `note` — has no marker family at all. A `{{ … }}`
//     marker anywhere in one of these files is REFUSED rather than sent; see
//     refuseAutomationMarkers.
//  3. THERE IS NO LOCAL BASELINE. Every other loop keeps one in .ronja/ because
//     it cannot otherwise tell an edit from a colleague's write. Here GET
//     returns the whole live configuration, so "does the row already say what
//     this file says" is answered directly, on any machine, including a fresh CI
//     checkout. The committed lock carries the one thing the server cannot
//     answer — which row this path IS, and when we last agreed with it.
//
// Verbs: init, push, status. No publish and no discard (there is no draft to
// commit or throw away), and no clone: it is the verb a second person needs, not
// the one that proves the loop.
func newAutomationCmd() *cobra.Command {
	auto := &cobra.Command{
		Use:   "automation",
		Short: "Develop a feature's automations from a local folder",
		Long: `Develop a feature's automations from a local folder.

An automation folder holds one .json file per automation plus a small manifest
(ronja.json) recording which automation each file is on each instance. The
filename is the automation's name, and anything that is not a .json file is
ignored — so the folder can also hold a README or whatever else your repo needs.

  ronja automation init --feature <id>   start a folder
  ronja automation status                what a push would change, and what moved
  ronja automation push                  make each automation match its file

There is no publish and no discard: an automation has no draft and no version
history, so a push is the whole write.

What the folder deliberately cannot do:

  * change an automation's trigger kind, whether it is reference-based, or the
    event it subscribes to — the update body carries none of the three, so a
    push REFUSES rather than letting the field be silently ignored
  * delete an automation because its file is gone — that needs --prune
  * re-assert a webhook signing secret, which is readable exactly once

Bindings are per-stack, so the same folder can target a local backend and
production without either overwriting the other's.`,
	}
	auto.AddCommand(
		newAutomationInitCmd(), newAutomationStatusCmd(), newAutomationPushCmd(),
	)
	addStackFlag(auto)
	return auto
}

// automationNameFor is the automation name a file's path claims: its stem.
//
// The filename IS the name — there is no `name` key in the file, and
// parseAutomationFile refuses one — so this is the single place the mapping
// lives. Same shape as tableNameFor and for the same reason: two spellings of
// "the stem" would disagree about a file with two dots in its name, and the
// disagreement would show up as a rename nobody asked for.
func automationNameFor(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// readAutomationFiles enumerates an automation folder and reads every .json
// file, so the same set can be parsed, guarded and pushed without walking twice.
func readAutomationFiles(root string) (map[string]string, *wfdir.Enumeration, error) {
	return readLocalFiles(root, wfdir.AutomationKind)
}

// parseAutomationFolder decodes every file in a folder, keeping the refusals
// per FILE rather than aborting on the first one.
//
// Per file because the reader is going to fix them all in one sitting: a folder
// of nine automations where two files have a typo is two edits, and a command
// that reports one, gets fixed, and then reports the other is two round trips
// through a person's attention for no reason. The caller decides whether a
// refusal is fatal — push says yes, status says no and reports it.
func parseAutomationFolder(files map[string]string) (map[string]*automationFile, map[string]error) {
	parsed := make(map[string]*automationFile, len(files))
	problems := map[string]error{}
	for path, body := range files {
		file, err := parseAutomationFile(path, body)
		if err != nil {
			problems[path] = err
			continue
		}
		parsed[path] = file
	}
	return parsed, problems
}

// automationOrphans names the paths this folder's LOCK still binds and the
// folder no longer holds — the deleted-file case.
//
// It reads the LOCK rather than the disk on both sides on purpose: the lock is
// committed, so a fresh checkout after a bad rebase knows exactly as much as the
// machine the file was deleted on. Sorted, because it is printed.
func automationOrphans(binding wfdir.Binding, onDisk map[string]string) []string {
	var out []string
	for path := range binding.Automations {
		if _, present := onDisk[path]; !present {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// automationFeatureAdvice is the "this folder names no feature" refusal, shared
// by push and by the status report's not-checked reason.
//
// One sentence in one place because the two are read minutes apart by the same
// person, and a status that describes the problem differently from the push that
// refused reads as two different problems.
func automationFeatureAdvice(f *folder) string {
	return fmt.Sprintf("this folder names no feature — add \"featureID\" to the %s entry in %s",
		f.Resolved.URL, wfdir.ManifestName)
}

// automationRowsByID indexes a feature's automations for the per-path lookups
// both push and status make.
func automationRowsByID(rows []*api.Automation) map[string]*api.Automation {
	out := make(map[string]*api.Automation, len(rows))
	for _, row := range rows {
		if row != nil {
			out[row.ID] = row
		}
	}
	return out
}
