package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja app push` syncs the folder into YOUR draft — never into the live app.
//
// The order of operations is the design, and it differs from `wf push` in two
// places for reasons that are specific to data apps:
//
//   - The ALLOWLISTS go up before the files. The bundle compiler validates a
//     secret reference against allowed_secret_ids, so a file written before its
//     grant exists fails to compile for a reason the author cannot see in the
//     file.
//   - The ENTRYPOINT goes up LAST, where a workflow's goes first. A workflow
//     leads with it because renaming one requires the new file to exist before
//     the row can name it; a data app's entrypoint never moves, so that ordering
//     buys nothing — and every data-app file write recompiles the whole bundle,
//     so writing App.tsx last means the intermediate compiles of a fresh app
//     fail with "entrypoint not found" BEFORE the S3 upload, churning fewer
//     bundles than the other order would.
//
// Intermediate compile failures are expected either way and are NOT fatal: the
// write commits before the recompile runs, so the file is saved regardless, and
// only the final state is meant to compile. The verdict is the validate at the
// end.
func newDataAppPushCmd() *cobra.Command {
	var (
		noValidate bool
		force      bool
	)
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Sync this folder into your draft of the data app",
		Long: `Sync this folder into your draft of the data app.

Pushes to a DRAFT, always. If the app is live and you have no draft, one is
forked for you; if the folder has never been pushed to this instance, the app
is created and the binding recorded in ronja.json.

Every file write recompiles the whole bundle server-side, so a push of N files
is N compiles and takes a noticeable few seconds per file. Intermediate states
that do not compile are normal and are not failures — the file is still saved.
What decides whether the result is publishable is the validate at the end,
which is reported as "compiles: yes/no".

It refuses when the draft changed on the server since your last sync (the web
builder edits the same draft), listing what moved. --force overwrites.
--no-validate skips the final compile check.

Publishing is a separate step:

  ronja app publish                commit the draft, or submit it for review`,
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
			result, err := runAppPush(cmd.Context(), f, appPushOptions{Validate: !noValidate, Force: force})
			// The report is emitted even on failure: a push that stopped halfway
			// has already written files, and "which ones" is the first thing
			// anyone needs to know.
			if result != nil {
				if flagJSON {
					if emitErr := emitJSON(result); emitErr != nil {
						return emitErr
					}
				} else {
					printAppPushReport(result)
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&noValidate, "no-validate", false,
		"skip the compile check after syncing")
	cmd.Flags().BoolVar(&force, "force", false,
		"push even though the draft changed on the server since your last sync")
	return cmd
}

type appPushOptions struct {
	Validate bool
	Force    bool
}

// appPushResult is the --json shape and the human renderer's input, so the two
// cannot describe different things.
type appPushResult struct {
	// Created reports that this push brought the data app into existence — as an
	// unpublished draft, which is worth saying out loud.
	Created bool `json:"created"`
	// DataAppID is the STABLE identity (what ronja.json records); DraftID is the
	// row the files were written to.
	DataAppID string   `json:"dataAppID"`
	DraftID   string   `json:"draftID"`
	Pushed    []string `json:"pushed"`
	Deleted   []string `json:"deleted"`
	Unchanged int      `json:"unchanged"`
	// UpToDate reports a push that had nothing to do.
	UpToDate bool `json:"upToDate"`
	// DraftUnderReview reports that the draft written to has already been
	// submitted for admin review — the push changed what someone is reviewing.
	DraftUnderReview bool `json:"draftUnderReview"`
	// AccessChanges is every grant and revocation this push made, in full.
	AccessChanges []accessChange `json:"accessChanges,omitempty"`
	// Compiles is the verdict from the closing validate: nil when it was skipped
	// (--no-validate), otherwise whether the draft is publishable as it stands.
	Compiles *bool `json:"compiles,omitempty"`
	// CompileError is the diagnostics from a failed final compile.
	CompileError *api.CompileError `json:"compileError,omitempty"`
	// Uncertain names files whose write failed and whose outcome could not be
	// established — the re-read that would have answered failed too. They are
	// neither pushed nor not-pushed, and saying so is the only honest report.
	Uncertain []string `json:"uncertain,omitempty"`
	// Error describes a push that stopped part-way. The result above then
	// describes what DID happen before it stopped.
	Error string `json:"error,omitempty"`
	// Target names the instance and organization this push landed in.
	Target string `json:"target,omitempty"`
	// URL is the frontend page for the draft this push wrote to, as the SERVER
	// reported it, and is rendered by the human report only — see pushResult.URL
	// for why it stays out of the --json payload. (appPublishResult.AppURL is
	// published, because that field was already part of that command's shape.)
	URL string `json:"-"`
	// LiveURL is the frontend page of the LIVE app when this push wrote to an
	// edit draft of it — printed beside URL so the author sees which link keeps
	// showing the published version (a draft is its own address; the server
	// never swaps the draft in for the live id). Empty on a first push, where
	// the draft IS the app and there is no live version behind it. Human report
	// only, for the same reason as URL; --json already carries dataAppID.
	LiveURL string `json:"-"`
}

// runAppPush is the whole state machine, kept out of the cobra closure so it is
// testable as a function and so the reporting path has exactly one shape.
//
// Returns (result, err): a non-nil result with a non-nil error is the
// partial-push case, and both halves matter.
func runAppPush(ctx context.Context, f *folder, opts appPushOptions) (*appPushResult, error) {
	client := api.New(f.Resolved.URL, f.Resolved.Token)
	result := &appPushResult{
		DataAppID: f.Binding.DataAppID,
		Target:    describeTarget(f.Resolved),
		Pushed:    []string{},
		Deleted:   []string{},
	}

	// 1. Local guards. Nothing here needs the network, and each refusal names a
	// file the author can act on immediately.
	local, enumeration, err := readLocalFiles(f.Root, wfdir.DataAppKind)
	if err != nil {
		return nil, err
	}
	for _, s := range enumeration.Skipped {
		fmt.Fprintf(os.Stderr, "  Note: skipping %s — %s\n", s.Path, s.Reason)
	}
	if err := checkAppPushable(local); err != nil {
		return nil, err
	}
	entrypoint := f.Manifest.Entrypoint
	if _, ok := local[entrypoint]; !ok {
		return nil, fmt.Errorf("the entrypoint %q is not in this folder — a data app compiles from it, so it has to exist.\n  Create it, or check \"entrypoint\" in %s",
			entrypoint, wfdir.ManifestPath(f.Root))
	}
	// The capability vocabulary is closed and the SERVER does not police it —
	// an unknown name is dropped at token-mint time, so a typo publishes green
	// and fails in a viewer's browser. Checked here, before anything is sent.
	if f.Manifest.ManagesAccess() {
		if err := checkDeclaredCapabilities(f.Manifest.DeclaredAccess(), wfdir.ManifestPath(f.Root)); err != nil {
			return nil, err
		}
	}

	// 2. Read what is already on the server, resolving the draft explicitly.
	existing, err := inspectAppTarget(ctx, client, f)
	if err != nil {
		return nil, err
	}
	// The row's entrypoint is IMMUTABLE (rdataapp.Patch has no field for it), so
	// a manifest naming a different one is a folder that can never sync — and
	// silently pushing to the row's entrypoint instead would write the author's
	// code into a file they are not looking at. Refused, with the only remedy
	// there is.
	if existing.App != nil && existing.App.Entrypoint != "" && existing.App.Entrypoint != entrypoint {
		return nil, fmt.Errorf("this folder's entrypoint is %q but data app %s compiles from %q, and a data app's entrypoint cannot be changed.\n  Point \"entrypoint\" in %s back at %q, or create a new app for the renamed one",
			entrypoint, existing.App.ID, existing.App.Entrypoint,
			wfdir.ManifestPath(f.Root), existing.App.Entrypoint)
	}

	// 3. Resolve the row to write to — the first step that changes anything.
	target, err := resolveAppPushTarget(ctx, client, f, existing, result)
	if err != nil {
		return nil, err
	}
	result.DraftID = target.ID
	// Recorded here rather than at the end, so every success path reports the
	// same link — including the up-to-date one, which can return before the
	// closing validate re-reads the row.
	result.URL = target.URL
	if existing.App != nil && existing.App.ID != target.ID {
		result.LiveURL = existing.App.URL
	}
	if target.SubmittedForReviewAt != nil {
		result.DraftUnderReview = true
		fmt.Fprintf(os.Stderr, "  Warning: draft %s has already been submitted for review — this push changes what the admin is reviewing.\n",
			target.ID)
	}

	// 4. Read what the row holds, addressed to the row we RESOLVED. Passing the
	// live id here would let the server answer with the draft anyway, and the
	// baseline would then record draft bytes under a row we never named.
	files, err := client.ListDataAppFiles(ctx, target.ID)
	if err != nil {
		return nil, fmt.Errorf("read files of %s: %w", target.ID, err)
	}
	remoteFiles := appContentByPath(files)

	baselineClean := false
	if !result.Created {
		baselineClean, err = checkAppDrift(f, remoteFiles, opts.Force)
		if err != nil {
			return nil, err
		}
	}

	// landed tracks what the baseline may honestly claim as the sync proceeds, so
	// a push that stops half way can still record an accurate one. It starts as
	// what is ALREADY acknowledged rather than as the whole remote, so a --force
	// push that stops does not hand the retry a baseline containing the very
	// content it was forcing past.
	landed := acknowledgedRemote(f.State.For(f.Key), remoteFiles, local, result.Created)
	stop := func(err error) (*appPushResult, error) {
		result.Error = err.Error()
		saveAppBaseline(f, target, landed)
		return result, err
	}

	// 5. The ALLOWLISTS first, before any file. The bundle compiler checks a
	// secret reference against allowed_secret_ids, so a file that reaches the
	// server before its grant does fails to compile for a reason that is nowhere
	// in the file.
	//
	// A create already carried them in its body, so there is nothing to PATCH —
	// but they are still REPORTED, because a first push is precisely when every
	// grant is new. Saying nothing there would make the one command that shows
	// privileges silent on the occasion they are all being handed out.
	if result.Created && f.Manifest.ManagesAccess() {
		result.AccessChanges = diffAccess(api.DataAppAccess{}, f.Manifest.DeclaredAccess())
	}
	if !result.Created && f.Manifest.ManagesAccess() {
		declared := f.Manifest.DeclaredAccess()
		changes := diffAccess(target.DataAppAccess, declared)
		if len(changes) > 0 {
			var patch api.DataAppPatch
			patch.SetAccess(declared)
			updated, err := client.UpdateDataApp(ctx, target.ID, patch)
			if err != nil {
				return stop(fmt.Errorf("update what %s is allowed to reach: %w", target.ID, err))
			}
			result.AccessChanges = changes
			// The row holds this now, and `target` is what a baseline recorded from
			// here on describes.
			target.DataAppAccess = updated.DataAppAccess
		}
	}

	// 6. The TITLE, which is the row's `name`.
	if f.Manifest.Title != "" && f.Manifest.Title != target.Name {
		title := f.Manifest.Title
		updated, err := client.UpdateDataApp(ctx, target.ID, api.DataAppPatch{Name: &title})
		if err != nil {
			return stop(fmt.Errorf("update the title of %s: %w", target.ID, err))
		}
		target.Name = updated.Name
	}

	// 7. Files: everything but the entrypoint, then the entrypoint, then the
	// deletions. See the command comment for why the entrypoint goes last and
	// not first as a workflow's does.
	compileErr, err := putAppFiles(ctx, client, target.ID, entrypoint, local, remoteFiles, landed, result)
	if err != nil {
		return stop(err)
	}

	if err := deleteAppFiles(ctx, client, target.ID, local, remoteFiles, landed, result); err != nil {
		return stop(err)
	}

	// 8. A push that wrote nothing at all, against a baseline that already
	// described the server, has nothing to re-read.
	nothingChanged := !result.Created && baselineClean &&
		len(result.Pushed) == 0 && len(result.Deleted) == 0 && len(result.AccessChanges) == 0
	if nothingChanged && !opts.Validate {
		result.UpToDate = true
		if !appBaselineDescribes(f.State.For(f.Key), target) {
			f.State.Set(f.Key, baselineFromAppLocal(target, landed))
			if err := wfdir.SaveState(f.Root, f.State); err != nil {
				return result, err
			}
		}
		return result, nil
	}

	// 9. The verdict. ALWAYS asked for, even when the last write compiled
	// cleanly: a file DELETE also recompiles, the row's validated_at is what
	// publish actually gates on, and a stale "it compiled three writes ago" is
	// exactly the thing this loop must not report as ready.
	if opts.Validate {
		validated, verr := client.ValidateDataApp(ctx, target.ID)
		switch {
		case verr == nil:
			ok := validated.IsValidated()
			result.Compiles = &ok
			target = validated
		case api.StatusOf(verr) == 400:
			// The server's compile refusal. Not a push failure — every byte landed
			// — so it is reported as the verdict it is, and the exit code below
			// makes it non-zero so CI notices.
			no := false
			result.Compiles = &no
			result.CompileError = &api.CompileError{Message: compileMessageOf(verr, compileErr)}
		default:
			return stop(fmt.Errorf("check whether %s compiles: %w", target.ID, verr))
		}
	}

	// 10. The new baseline is what we just pushed: the server now holds exactly
	// these bytes.
	f.State.Set(f.Key, baselineFromAppLocal(target, local))
	if err := wfdir.SaveState(f.Root, f.State); err != nil {
		return result, err
	}
	if result.Compiles != nil && !*result.Compiles {
		return result, fmt.Errorf("the draft does not compile — it cannot be published until it does")
	}
	if nothingChanged {
		result.UpToDate = true
	}
	return result, nil
}

// compileMessageOf picks the most useful compile message available: the
// validate's own refusal, falling back to the last diagnostics a file write
// reported.
//
// They are usually the same failure seen twice, but not always — a delete can
// break a build no write ever saw — so the validate's answer wins.
func compileMessageOf(verr error, fromWrite *api.CompileError) string {
	if code := api.CodeOf(verr); code != "" {
		return code
	}
	if fromWrite != nil && fromWrite.Message != "" {
		return fromWrite.Message
	}
	return verr.Error()
}

// resolveAppPushTarget decides which row the files go to: creating the app on a
// first push, and forking a draft when there is none.
//
// Never auto-creates a REPLACEMENT: a binding naming an app that has been
// deleted, archived or turned into a proposal is reported as broken by
// inspectAppTarget. Quietly creating a second app would leave the old one's
// viewers pointing at a row nobody edits any more.
func resolveAppPushTarget(ctx context.Context, client *api.Client, f *folder, existing appTarget, result *appPushResult) (*api.DataApp, error) {
	if existing.App == nil {
		featureID := f.Binding.FeatureID
		if featureID == "" {
			return nil, fmt.Errorf("this folder has no feature to create the data app in — add \"featureID\" to its entry in %s, or start again with `ronja app init --feature <id>`",
				wfdir.ManifestPath(f.Root))
		}
		// A first push is the one path here that WRITES a binding, so this is
		// where the organization has to be authoritative rather than adopted from
		// an entry that does not exist yet.
		if _, err := f.bindNew(ctx); err != nil {
			return nil, err
		}
		created, err := client.CreateDataApp(ctx, api.CreateDataAppInput{
			FeatureID: featureID,
			Name:      f.Manifest.Title,
			// Declared at creation rather than left to the patch above: it saves a
			// round trip, and the compiler checks secret references against
			// allowed_secret_ids on the very first file write, so the grants have to
			// be on the row before any file reaches it.
			DataAppAccess: f.Manifest.DeclaredAccess(),
		})
		if err != nil {
			return nil, explainCreateFailure(err, featureID, f.Resolved.URL)
		}
		// Recorded IMMEDIATELY, before any file is written: the app now exists —
		// live and visible — and a push that died before saving the manifest would
		// leave an orphan the next push could not find and would create again.
		f.Manifest.SetBinding(f.Key, wfdir.Binding{DataAppID: created.ID, FeatureID: featureID})
		if err := wfdir.SaveManifest(f.Root, f.Manifest); err != nil {
			return nil, fmt.Errorf("record the new data app %s in %s: %w\n  The app EXISTS on %s. Re-run the push once %s is writable, or delete it with `ronja api -X DELETE /api/v2/dataapp/%s`",
				created.ID, wfdir.ManifestPath(f.Root), err, f.Resolved.URL, wfdir.ManifestName, created.ID)
		}
		result.Created = true
		result.DataAppID = created.ID

		// The created row IS the draft — POST /dataapp mints a parentless draft,
		// and publish promotes that same row in place. So there is nothing to check
		// out: the app id and the draft id are one id, for its whole life.
		//
		// This is what makes an abandoned first push cost nothing. It used to
		// create a LIVE app and fork a draft to write into, which left anyone who
		// stopped before publishing with a visible, file-less app serving the
		// pending page and no way to remove it.
		return created, nil
	}

	if row := existing.Row(); row != nil {
		return row, nil
	}
	draft, err := client.CheckoutDataApp(ctx, existing.App.ID, api.DataAppPatch{})
	if err != nil {
		return nil, fmt.Errorf("open a draft of %s: %w", existing.App.ID, err)
	}
	return draft, nil
}

// explainCreateFailure turns rdataapp.Create's two access refusals into
// something that names the actual problem.
//
// The private-feature refusal is a deliberately opaque "feature not found"
// (backend/resource/rdataapp/store.go) so a non-owner cannot probe for the
// existence of someone's private feature. That is right for the API and wrong
// as the last thing a CLI says, because the author is looking straight at the
// feature they named: passing it through reads as "your id is wrong" when the
// truth is "that feature is someone else's".
func explainCreateFailure(err error, featureID, instanceURL string) error {
	message := api.CodeOf(err)
	switch {
	case strings.Contains(message, "admin required"):
		return fmt.Errorf("feature %s is shared with the organization, and creating a data app in a shared feature needs an admin.\n  Ask an admin to create the app (then `ronja app clone` it), or point this folder at a feature you own",
			featureID)
	case strings.Contains(message, "feature not found"):
		return fmt.Errorf("feature %s cannot be used on %s: either it does not exist, or it is another user's private feature.\n  Check the id with `GET /api/v2/feature/query`",
			featureID, instanceURL)
	}
	return fmt.Errorf("create the data app in feature %s: %w", featureID, err)
}

// checkAppDrift refuses a push that would overwrite server-side changes made
// since the last sync.
//
// The comparison is REMOTE vs BASELINE, not remote vs local: the question is
// whether anything moved underneath us, and a file the author edited locally is
// exactly what a push is for.
//
// Unlike the workflow version this covers FILES only. The row's metadata is not
// compared because the two fields a push would overwrite behave differently
// here: the entrypoint is immutable (a mismatch is refused outright, above,
// rather than treated as drift), and the allowlists are reported per-id by the
// access diff — which is strictly more informative than a drift line saying
// they moved.
//
// Reports whether the baseline was CLEAN — an existing baseline that already
// described the server exactly.
func checkAppDrift(f *folder, remote map[string]string, force bool) (bool, error) {
	baseline := f.State.For(f.Key)
	remoteHashes := make(map[string]string, len(remote))
	for path, content := range remote {
		remoteHashes[path] = wfdir.HashString(content)
	}
	drift := wfdir.DiffHashes(remoteHashes, baseline.Hashes())
	if !drift.Dirty() {
		return baseline != nil, nil
	}
	if force {
		fmt.Fprintf(os.Stderr, "  Note: --force — overwriting %d file(s) that changed on the server since your last sync.\n",
			drift.Total())
		return false, nil
	}
	if baseline == nil {
		return false, fmt.Errorf("this folder has no sync baseline for %s, and the data app already has %d file(s) there — .ronja/ is local-only, so a copy cloned from git starts without one.\n  Clone the app into a fresh folder to get one, or push --force to overwrite the remote files with what you have here",
			f.Resolved.URL, len(remote))
	}
	return false, fmt.Errorf("the draft changed on the server since your last sync — pushing would overwrite it:\n%s  Run `ronja app status` to see the detail, or push --force to overwrite",
		driftSummary(drift, nil))
}

// putAppFiles writes the folder's files into the target row.
//
// Ordering: everything but the entrypoint in path order, then the entrypoint
// last. See the command comment.
//
// A COMPILE failure is not a write failure. The server saves the file and then
// recompiles, and reports a failed recompile in a 200 alongside the saved row
// (api.DataAppFileSaveResponse) — because pushing a multi-file app walks through
// states that cannot compile by construction. So diagnostics are collected and
// the sync continues; anything else stops it.
func putAppFiles(ctx context.Context, client *api.Client, targetID, entrypoint string, local, remote, landed map[string]string, result *appPushResult) (*api.CompileError, error) {
	order := []string{}
	for _, path := range sortedPaths(local) {
		if path != entrypoint {
			order = append(order, path)
		}
	}
	if _, ok := local[entrypoint]; ok {
		order = append(order, entrypoint)
	}

	var lastCompileErr *api.CompileError
	for _, path := range order {
		if remoteContent, ok := remote[path]; ok && remoteContent == local[path] {
			result.Unchanged++
			continue
		}
		// Said out loud: each of these is a full esbuild compile plus an S3
		// upload, so a folder of any size sits here for a while and silence would
		// read as a hang.
		fmt.Fprintf(os.Stderr, "  pushing %s\n", path)
		saved, err := client.PutDataAppFile(ctx, targetID, path, local[path])
		if err != nil {
			// Whether the write LANDED is a different question from whether the
			// request succeeded, and the answer decides what the baseline may say.
			//
			// A workflow file save is one transaction, so a failure there means
			// the write did not happen. A data-app save COMMITS and only then
			// recompiles and uploads the bundle (rdataapp.UpsertFile calls
			// publishAndLog out of tx), so a failure from that second half — an S3
			// error, a throttle, anything that is not the typed compile error
			// folded into a 200 — arrives from a write that already landed.
			// Assuming it did not is what turns a half-finished push into a
			// refusal of its own retry: the file is on the server, the baseline
			// says it is not, and the next push reports the author's own work as
			// somebody else's drift and demands --force.
			//
			// But that is only true of the failures that can reach the commit.
			// See writeOutcomeUncertain: a client rejection never does, and
			// asking after one is worse than not asking.
			if writeOutcomeUncertain(err) {
				want := local[path]
				switch reconcileUncertainAppWrite(ctx, client, targetID, path, &want, landed) {
				case appWriteLanded:
					// It did land after all, so the report has to say so — a
					// report that disagrees with the baseline beside it is worse
					// than either being wrong on its own.
					result.Pushed = append(result.Pushed, path)
				case appWriteUnknown:
					result.Uncertain = append(result.Uncertain, path)
				}
			}
			return lastCompileErr, fmt.Errorf("push %s: %w", path, err)
		}
		landed[path] = local[path]
		result.Pushed = append(result.Pushed, path)
		if saved.CompileError != nil {
			lastCompileErr = saved.CompileError
		}
	}
	return lastCompileErr, nil
}

// writeOutcomeUncertain reports whether a failed file write could have taken
// effect anyway — the only case worth a reconciling read.
//
// The split is the HTTP status. A 4xx is the server saying it considered the
// request and refused it, before any of the commit-then-recompile sequence ran:
// the file certainly kept its previous content. A 5xx, or no status at all
// (api.StatusOf answers 0 for a timeout or a dropped connection, where the
// deadline was ours and the transaction was the server's), leaves the outcome
// genuinely unknown — and for a data app that is not theoretical, since the
// write commits before the bundle recompile that produces most of those 5xxs.
//
// Reconciling a definite rejection is not merely wasteful, it is the bug this
// split exists to close: the read answers with the server's CURRENT bytes, and
// under --force those are the colleague's change we are forcing past. Recording
// them as acknowledged disarms the drift guard, so the next plain push
// overwrites that change in silence.
//
// 408 and 429 sit on the 4xx side deliberately. Both are refusals issued before
// a handler ran, and where the classification is arguable the safe direction is
// "did not land": a baseline that under-claims costs the author one drift
// refusal they can look at and force past, while one that over-claims destroys
// somebody else's work without saying anything.
func writeOutcomeUncertain(err error) bool {
	status := api.StatusOf(err)
	return status < 400 || status >= 500
}

// appWriteVerdict is what a reconciling read established about one failed write.
type appWriteVerdict int

const (
	// appWriteUnknown — the read failed too, so nothing was learnt.
	appWriteUnknown appWriteVerdict = iota
	// appWriteLanded — the server holds the state the write was aiming for.
	appWriteLanded
	// appWriteMissed — it does not; the write left no trace.
	appWriteMissed
)

// reconcileUncertainAppWrite asks what the server actually holds for one file
// after a write whose outcome is unknown, and repairs the baseline with the
// answer.
//
// Both callers need it and for the same reason: rdataapp commits the file row
// (UpsertFile) or removes it (DeleteFile) inside a transaction and recompiles
// the bundle AFTERWARDS, so a failure from that second half comes from a write
// that already took effect. `want` is the content the write meant to leave
// behind — nil for a DELETE, whose intended end state is absence.
//
// One read, no retry loop: the question has a single answer and it is worth
// exactly one round trip. A read that ALSO fails changes nothing — the file
// keeps whatever the baseline already acknowledged, which is the conservative
// half of the same choice, and the next push reports it as drift the author can
// look at.
//
// The baseline only ever gains what we were TRYING to write, never simply
// whatever the server answers with — acknowledgeIntendedWrite, which the
// workflow loop's reconcile calls too so the rule cannot come to mean two
// things: content nobody here has acknowledged must stay unacknowledged, or a
// stopped --force push hands its own retry a baseline containing the very change
// it was forcing past.
func reconcileUncertainAppWrite(ctx context.Context, client *api.Client, targetID, path string, want *string, landed map[string]string) appWriteVerdict {
	saved, err := client.GetDataAppFile(ctx, targetID, path)
	if err != nil {
		if api.StatusOf(err) == 404 {
			// Nothing there: a PUT that did not land, or a DELETE that did.
			// Either way the baseline must stop claiming the path — left standing,
			// it would make the next push read a deletion this one performed as a
			// colleague's addition.
			delete(landed, path)
			if want == nil {
				return appWriteLanded
			}
			return appWriteMissed
		}
		return appWriteUnknown
	}
	if want != nil && acknowledgeIntendedWrite(landed, path, *want, saved.Content) {
		return appWriteLanded
	}
	return appWriteMissed
}

// deleteAppFiles removes what the server holds and the folder does not.
//
// Enumerated from `remote` rather than from the baseline-recording `landed`,
// which no longer describes the whole row: a forced push past a colleague's
// added file must still delete it, since that is what forcing the folder onto
// the row means.
//
// The entrypoint is never in this set — it is in `local` by the guard at the top
// of the push — which matters because the server refuses to delete it.
func deleteAppFiles(ctx context.Context, client *api.Client, targetID string, local, remote, landed map[string]string, result *appPushResult) error {
	deletions := []string{}
	for path := range remote {
		if _, ok := local[path]; !ok {
			deletions = append(deletions, path)
		}
	}
	sort.Strings(deletions)
	for _, path := range deletions {
		if _, err := client.DeleteDataAppFile(ctx, targetID, path); err != nil {
			// Same split as a failed PUT, for the same reason: the row is removed
			// before the recompile runs, so a 5xx or a lost answer may well have
			// deleted the file — while a rejection certainly did not. See
			// writeOutcomeUncertain and reconcileUncertainAppWrite.
			if writeOutcomeUncertain(err) {
				switch reconcileUncertainAppWrite(ctx, client, targetID, path, nil, landed) {
				case appWriteLanded:
					result.Deleted = append(result.Deleted, path)
				case appWriteUnknown:
					result.Uncertain = append(result.Uncertain, path)
				}
			}
			return fmt.Errorf("delete %s: %w", path, err)
		}
		delete(landed, path)
		result.Deleted = append(result.Deleted, path)
	}
	return nil
}

// appBaselineDescribes reports whether the recorded baseline already describes
// this row — the same id, and the same metadata.
func appBaselineDescribes(baseline *wfdir.InstanceState, row *api.DataApp) bool {
	return baseline != nil &&
		baseline.SourceID == row.ID &&
		baseline.Title == row.Name &&
		baseline.Entrypoint == row.Entrypoint &&
		// An unrecorded allowlist (a baseline predating the guard) does not make
		// the baseline stale on its own, and the next push records it.
		(baseline.Access == nil || api.SameAccess(*baseline.Access, row.DataAppAccess))
}

// saveAppBaseline records a baseline on the partial-push path, where the push
// has already failed and a second failure must not replace the first in the
// report.
func saveAppBaseline(f *folder, source *api.DataApp, files map[string]string) {
	f.State.Set(f.Key, baselineFromAppLocal(source, files))
	if err := wfdir.SaveState(f.Root, f.State); err != nil {
		fmt.Fprintf(os.Stderr, "  Note: could not record what was pushed in the local baseline (%v).\n", err)
	}
}

func printAppPushReport(r *appPushResult) {
	out := os.Stdout

	if r.UpToDate && r.Error == "" && len(r.Pushed) == 0 && len(r.Deleted) == 0 {
		fmt.Fprintf(out, "  Up to date — the draft %s already holds this folder.\n", r.DraftID)
		printAppPushURLs(out, r)
		printCompileVerdict(out, r)
		return
	}

	if r.Created {
		fmt.Fprintf(out, "  Created data app %s (an unpublished draft — nobody else can see it yet)\n", r.DataAppID)
	}
	for _, c := range r.AccessChanges {
		for _, id := range c.Added {
			fmt.Fprintf(out, "    grant    %-10s %s\n", c.Label, id)
		}
		for _, id := range c.Removed {
			fmt.Fprintf(out, "    revoke   %-10s %s\n", c.Label, id)
		}
	}
	for _, path := range r.Pushed {
		fmt.Fprintf(out, "    pushed   %s\n", path)
	}
	for _, path := range r.Deleted {
		fmt.Fprintf(out, "    deleted  %s\n", path)
	}
	// Listed with the rest rather than folded into the closing sentence: the
	// per-file lines are what anyone scans, and a file whose fate is unknown
	// belongs among them.
	for _, path := range r.Uncertain {
		fmt.Fprintf(out, "    unknown  %s\n", path)
	}
	if r.Unchanged > 0 {
		fmt.Fprintf(out, "    %d unchanged\n", r.Unchanged)
	}

	if r.Error != "" && r.Compiles == nil {
		fmt.Fprintf(out, "\n  Push stopped part-way: %s\n", r.Error)
		if len(r.Uncertain) > 0 {
			// The confident sentence would be a lie here: the write failed AND the
			// read that would have settled it failed too, so the draft may or may
			// not hold those files.
			fmt.Fprintf(out, "  The draft %s holds what is listed above, except the file(s) marked unknown —\n", r.DraftID)
			fmt.Fprintf(out, "  asking what became of those failed too. `ronja app status` compares again.\n")
			return
		}
		fmt.Fprintf(out, "  The draft %s holds what is listed above; fix the problem and push again.\n", r.DraftID)
		return
	}
	fmt.Fprintf(out, "\n  Draft:    %s\n", r.DraftID)
	if r.Target != "" {
		fmt.Fprintf(out, "  Target:   %s\n", r.Target)
	}
	printAppPushURLs(out, r)
	printCompileVerdict(out, r)
}

// printAppPushURLs prints where to look at what was pushed. When the push wrote
// to an edit draft of a live app, BOTH links are printed: the draft's, which is
// where these files are, and the live app's, which is unchanged — the server
// serves exactly the row a link names, so the author's live link keeps showing
// the published version until `ronja app publish`.
func printAppPushURLs(out io.Writer, r *appPushResult) {
	printResourceURL(out, reportKeyWidth, r.URL)
	if r.LiveURL != "" {
		fmt.Fprintf(out, "  %-*s%s  (unchanged until `ronja app publish`)\n", reportKeyWidth, "Live URL:", r.LiveURL)
	}
}

// printCompileVerdict renders the closing validate, which is the only thing that
// says whether the draft can be published.
func printCompileVerdict(out *os.File, r *appPushResult) {
	switch {
	case r.Compiles == nil:
		fmt.Fprintf(out, "  Compiles: not checked (--no-validate)\n")
		fmt.Fprintf(out, "\n  Next: ronja app validate\n")
	case *r.Compiles:
		fmt.Fprintf(out, "  Compiles: yes\n")
		fmt.Fprintf(out, "\n  Next: ronja app publish\n")
	default:
		fmt.Fprintf(out, "  Compiles: NO\n")
		if r.CompileError != nil && r.CompileError.Message != "" {
			// Same message, same treatment as `ronja app validate` — push is
			// where most authors meet it first.
			fmt.Fprintf(out, "\n  %s\n", stripBundleNamespace(r.CompileError.Message))
		}
		fmt.Fprintf(out, "\n  Your files are saved on the draft. Fix the diagnostics above and push again;\n")
		fmt.Fprintf(out, "  the app stays on its last published version until this compiles.\n")
	}
}
