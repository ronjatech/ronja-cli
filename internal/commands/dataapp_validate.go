package commands

import (
	"fmt"
	"os"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja app validate` recompiles your draft and reports whether it is
// publishable. Unlike `ronja wf validate` it checks what is on the server, since
// POST /dataapp/:id/validate compiles a draft, so a dirty folder is warned about.
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
			// newClient, not api.New: the test seam for staging a timeout and an
			// unanswered check, which a fake HTTP handler cannot.
			client := newClient(resolved.URL, resolved.Token)

			_, draft, err := resolveAppDraft(cmd.Context(), client, f, "nothing to validate")
			if err != nil {
				return err
			}
			// A dirty folder only warns, but alias refusals fail `ok`: push
			// refuses such a folder even when the draft compiles, and the server
			// only warns about an unbound secret.
			aliases, err := f.aliasReport()
			if err != nil {
				// Fail closed: CI gates on `ok`, and an unwalked folder is unchecked.
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
			askedAt := time.Now()
			validated, verr := client.ValidateDataApp(cmd.Context(), draft.ID)
			switch {
			case verr == nil:
				compiles := validated.IsValidated()
				result.Compiles = &compiles
			case api.StatusOf(verr) == 400:
				no := false
				result.Compiles = &no
				result.CompileError = &api.CompileError{Message: compileMessageOf(verr, nil)}
			case api.IsTimeout(verr):
				// Our own deadline, as in runAppPush; checked before Unanswered
				// because it is narrower.
				result.CompileCheck = &compileCheck{
					TimedOut: true,
					Status:   api.StatusOf(verr),
					Detail:   verr.Error(),
					Waited:   time.Since(askedAt),
				}
			case api.Unanswered(verr):
				// No verdict: Compiles stays nil, so a 500 never reads as a fact
				// about the draft.
				result.CompileCheck = &compileCheck{
					Status: api.StatusOf(verr),
					Detail: verr.Error(),
				}
			default:
				return fmt.Errorf("check whether %s compiles: %w", draft.ID, verr)
			}

			result.OK = result.Compiles != nil && *result.Compiles && len(result.AliasRefusals) == 0
			if flagJSON {
				if err := emitJSON(result); err != nil {
					return err
				}
			} else {
				printAppValidateReport(result)
			}
			if !result.OK {
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
	// OK is the verdict and the exit code's twin: the draft compiles and push
	// would accept the folder. Compiles is about the draft alone.
	OK bool `json:"ok"`
	// Compiles is nil when the compiler gave no verdict (see CompileCheck).
	Compiles     *bool             `json:"compiles"`
	CompileError *api.CompileError `json:"compileError,omitempty"`
	// CompileCheck has appPushResult.CompileCheck's contract.
	CompileCheck  *compileCheck `json:"compileCheck,omitempty"`
	AliasRefusals []string      `json:"aliasRefusals,omitempty"`
}

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

// printAliasRefusalSummary says the compile verdict is not this command's
// verdict when push would refuse the folder; every ending of
// printAppValidateReport must call it.
func printAliasRefusalSummary(out *os.File, r *appValidateResult) {
	if n := len(r.AliasRefusals); n > 0 {
		fmt.Fprintf(out, "\n  %d alias %s above — `ronja app push` would refuse this folder\n",
			n, plural(n, "problem"))
	}
}

func printAppValidateReport(r *appValidateResult) {
	out := os.Stdout
	if r.CompileCheck != nil {
		// Answered first, and never as "Compiles: NO": no compiler said no.
		fmt.Fprintf(out, "  Compiles: not known\n")
		if r.CompileCheck.TimedOut {
			fmt.Fprintf(out, "\n  No verdict: %s — it may still be running, and this is\n", describeCompileDeadline(r.CompileCheck))
			fmt.Fprintf(out, "  not a verdict on your files. Ask again in a moment.\n")
		} else {
			fmt.Fprintf(out, "\n  The compiler did not answer (%s) — not a verdict on your files.\n",
				describeCompileNonAnswer(r.CompileCheck, r.DraftID))
			fmt.Fprintf(out, "  Try again in a moment; if it persists, report it.\n")
		}
		fmt.Fprintf(out, "\n  Draft:    %s\n", r.DraftID)
		printAliasRefusalSummary(out, r)
		return
	}
	if r.Compiles != nil && *r.Compiles {
		fmt.Fprintf(out, "  Compiles: yes\n")
		fmt.Fprintf(out, "\n  Draft:    %s\n", r.DraftID)
		if len(r.AliasRefusals) > 0 {
			// No "Next: ronja app publish" line: push refuses this folder, and
			// publish has no alias gate of its own.
			printAliasRefusalSummary(out, r)
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
	printAliasRefusalSummary(out, r)
	fmt.Fprintf(out, "\n  The app stays on its last published version until this compiles.\n")
}
