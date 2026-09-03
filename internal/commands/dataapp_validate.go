package commands

import (
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja app validate` recompiles your draft and reports whether it is
// publishable.
//
// It is NOT the analogue of `ronja wf validate`, and the difference is worth
// stating: a workflow can be validated before it exists, because
// POST /workflow/validate takes a whole candidate folder and persists none of
// it. A data app has no such endpoint — POST /dataapp/:id/validate compiles a
// DRAFT — so this checks what is ON THE SERVER, and a folder with local changes
// is told so rather than silently checked as if it had been pushed.
//
// Push runs this at the end, so most of the time it is already done. What it is
// for is the two cases where it is not: a push run with --no-validate, and a
// draft someone edited in the web builder since.
func newDataAppValidateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Recompile your draft and report whether it can be published",
		Long: `Recompile your draft and report whether it can be published.

Checks what is ON THE SERVER, not what is in your folder: it compiles your open
draft and reports the diagnostics. If the folder holds changes you have not
pushed, they are not part of what is checked, and you are told so.

  ronja app validate

A data app must compile before it can be published — POST :id/commit refuses a
draft whose last save failed — so this is the same gate ` + "`ronja app publish`" + `
applies, run on its own.

The folder's own dependency names are checked too — a name no stack binds, or
one used under the wrong kind of marker. Those exit non-zero even when the draft
compiles, because ` + "`ronja app push`" + ` refuses the folder. With --json, "ok" is
the verdict and "compiles" is the draft alone.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			f, err := openFolder(cmd.Context(), resolved, wfdir.DataAppKind)
			if err != nil {
				return err
			}
			client := api.New(resolved.URL, resolved.Token)

			_, draft, err := resolveAppDraft(cmd.Context(), client, f, "nothing to validate")
			if err != nil {
				return err
			}
			// A dirty folder is a WARNING, not a refusal: validating the draft as it
			// stands is a reasonable thing to want. But it is also the one way to
			// get a green verdict about code you are not looking at.
			//
			// The alias pre-flight is NOT in that category, and it is the reason
			// this command has an `ok` of its own beside `compiles`. A draft that
			// compiles perfectly says nothing about a folder whose allowlist
			// names an alias no stack binds — `ronja app push` refuses that
			// folder, and a CI job gating on this one would have waved it
			// through, twice over: an unbound secret is only a WARNING on the
			// server side too.
			aliases, err := f.aliasReport()
			if err != nil {
				// Fail closed. This command's `ok` is what a CI job gates on, and
				// a folder whose files could not be walked has not been checked —
				// which is a different answer from "checked and clean", and only
				// one of the two may exit zero.
				return err
			}
			noteAliasErrors(aliases)
			if err := warnIfAppDirty(f); err != nil {
				return err
			}

			result := &appValidateResult{
				DataAppID:     f.Binding.DataAppID,
				DraftID:       draft.ID,
				AliasRefusals: aliases.Refusals,
			}
			validated, verr := client.ValidateDataApp(cmd.Context(), draft.ID)
			switch {
			case verr == nil:
				result.Compiles = validated.IsValidated()
			case api.StatusOf(verr) == 400:
				result.Compiles = false
				result.CompileError = &api.CompileError{Message: compileMessageOf(verr, nil)}
			default:
				return fmt.Errorf("check whether %s compiles: %w", draft.ID, verr)
			}

			result.OK = result.Compiles && len(result.AliasRefusals) == 0
			if flagJSON {
				if err := emitJSON(result); err != nil {
					return err
				}
			} else {
				printAppValidateReport(result)
			}
			if !result.OK {
				// Non-zero so CI notices. The message has already been printed in the
				// caller's chosen format, so this adds nothing to it.
				return errAlreadyReported
			}
			return nil
		},
	}
	return cmd
}

type appValidateResult struct {
	DataAppID string `json:"dataAppID"`
	DraftID   string `json:"draftID"`
	// OK is the command's VERDICT and the exit code's twin: the draft compiles
	// AND the folder is one push would accept. Separate from Compiles because
	// the two answer different questions about different things — Compiles is
	// about the draft on the server, and an alias refusal is about the folder in
	// front of you — and collapsing them would make "compiles: false" a claim
	// about a compiler that was perfectly happy.
	OK           bool              `json:"ok"`
	Compiles     bool              `json:"compiles"`
	CompileError *api.CompileError `json:"compileError,omitempty"`
	// AliasRefusals are the local findings — see validateReport.AliasRefusals,
	// which carries the same contract for the workflow loop.
	AliasRefusals []string `json:"aliasRefusals,omitempty"`
}

// warnIfAppDirty says so when the folder holds changes the draft does not.
func warnIfAppDirty(f *folder) error {
	enumeration, err := wfdir.Enumerate(f.Root, wfdir.DataAppKind)
	if err != nil {
		return err
	}
	diff := wfdir.DiffHashes(enumeration.Files, f.State.For(f.Key).Hashes())
	if diff.Dirty() {
		fmt.Fprintf(os.Stderr,
			"  Warning: %d local file(s) differ from your last sync and are NOT part of what is being checked — run `ronja app push` first if you meant to include them.\n",
			diff.Total())
	}
	return nil
}

func printAppValidateReport(r *appValidateResult) {
	out := os.Stdout
	if r.Compiles {
		fmt.Fprintf(out, "  Compiles: yes\n")
		fmt.Fprintf(out, "\n  Draft:    %s\n", r.DraftID)
		if n := len(r.AliasRefusals); n > 0 {
			// The refusals themselves are already on stderr. What has to be said
			// HERE is that "Compiles: yes" is not the verdict, because the next
			// line would otherwise be an instruction to run a command that
			// refuses.
			fmt.Fprintf(out, "\n  %d alias %s above — `ronja app push` would refuse this folder\n",
				n, plural(n, "problem"))
			return
		}
		fmt.Fprintf(out, "\n  Next: ronja app publish\n")
		return
	}
	fmt.Fprintf(out, "  Compiles: NO\n")
	if r.CompileError != nil && r.CompileError.Message != "" {
		fmt.Fprintf(out, "\n  %s\n", stripBundleNamespace(r.CompileError.Message))
	}
	fmt.Fprintf(out, "\n  Draft:    %s\n", r.DraftID)
	fmt.Fprintf(out, "\n  The app stays on its last published version until this compiles.\n")
}
