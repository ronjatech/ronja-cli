package commands

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `pull` for the three folder kinds that deploy ONE row through a draft — a
// workflow, a data app, a module. Each reads the LIVE row (never the author's
// draft), plans its files in the folder's disk form and the manifest metadata
// a push manages (only the fields the folder already manages: a key the
// manifest leaves out stays left out), and checks the plan with the push's own
// parity function — workflowRowHoldsFolder, appRowHoldsFolder,
// moduleRowHoldsFolder — before a byte is written: a folder that would still
// differ from live after the pull is refused, not half-pulled.
//
// The anchor is the kind's live-first push (pushNothingLiveHolds and its
// twins): the head-version and applied-title anchors a `sync apply` of the
// pulled folder would record, recorded now, so that apply writes nothing.

// liveRowOrRefusal reads the row a folder kind's claim names and refuses a row
// with no live version. A 404 is a row gone since the claim was read.
func pullRowGone(f *folder, id string, err error) error {
	if api.StatusOf(err) == 404 || api.StatusOf(err) == 400 {
		return &managedRefusal{Reason: syncReasonLost, Detail: fmt.Sprintf(
			"%s %s, which this folder manages on stack %q, cannot be read (deleted, or out of your reach) — there is nothing live to pull. Run `ronja sync apply --stack %s` to see the claim's state and its remedy",
			f.Kind.Label, id, f.Stack, f.Stack)}
	}
	return fmt.Errorf("read %s %s: %w", f.Kind.Label, id, err)
}

func pullNotLive(f *folder, id, lifecycle string) error {
	return &managedRefusal{Reason: pullReasonNotLive, Detail: fmt.Sprintf(
		"%s %s is %s — it has never been published, so there is no live version to pull. Publish it (`%s publish`), or pull after it is",
		f.Kind.Label, id, api.DescribeLifecycle(lifecycle), f.Kind.Command)}
}

func pullInexpressible(f *folder, what string) error {
	return &managedRefusal{Reason: pullReasonInexpressible, Detail: fmt.Sprintf(
		"this folder cannot hold the live %s as it stands: %s. Nothing was written",
		f.Kind.Label, what)}
}

// draftAppliedState compares a draft kind's folder with what this stack last
// applied, through the head-version anchor: recorded == current means nothing
// was published since, and otherwise the recorded version's files (one GET)
// are what the folder last deployed.
//
// One recorded value is not a version: the row's OWN id, which HeadVersionID
// and its twins answer for a row with no committed version (an import of a
// workflow made before versions existed, or any row a `sync apply` created and
// never updated since). Its files are the LIVE row's, which every publish
// rewrites, so once the head has moved they are what is live now, not what
// this stack applied — and every unchanged file read as a change of the
// folder's own. The server keeps no copy of the content the row held before its
// first version (a commit turns the DRAFT into the version, with the draft's
// content), so that case is answered from this checkout's local baseline, and
// only when it provably describes that content — see unversionedApplied.
//
// moved reports that the live row changed since this stack applied it (a new
// head, or a title the applied-title anchor does not hold); differs names what
// the FOLDER changed since then — files, and ronja.json's title. A folder whose
// stack records no head cannot be compared, and answers moved with nothing
// differing: the pull goes ahead as it always did.
type draftAppliedState struct {
	moved   bool
	differs []string
	err     error
}

// draftHistory is how one draft kind reads its version history.
type draftHistory struct {
	head headReader
	// versionFiles reads one version's files, in the folder's disk form.
	versionFiles func(ctx context.Context, id string) (map[string]string, error)
	// firstCommitted is when the row's OLDEST version was committed, nil when
	// it has none.
	firstCommitted func(ctx context.Context, id string) (*time.Time, error)
}

func readDraftApplied(ctx context.Context, f *folder, liveID, liveTitle string, local, remote map[string]string, history draftHistory) draftAppliedState {
	recorded := f.headVersion()
	if recorded == "" {
		return draftAppliedState{moved: true}
	}
	current, err := history.head(ctx, liveID)
	if err != nil {
		return draftAppliedState{moved: true, err: fmt.Errorf("could not read the version history of %s (%v), so pull cannot tell a change of this folder's that was never deployed from the live one — pull again, or pull --force to replace the folder with what is live without that check", liveID, err)}
	}
	appliedTitle := ""
	if f.Lock != nil {
		appliedTitle = f.Lock.AppliedTitle(f.Stack)
	}
	st := draftAppliedState{moved: recorded != current || (appliedTitle != "" && !sameName(appliedTitle, liveTitle))}
	switch {
	case recorded == current:
		st.differs = slices.Collect(maps.Keys(fileSetChanges(local, remote)))
	case recorded == liveID && len(fileSetChanges(local, remote)) == 0:
		// The folder already holds every live file: the pull writes none, so
		// there is no change of the folder's it could replace.
	case recorded == liveID:
		if st.differs, st.err = unversionedApplied(ctx, f, liveID, local, history); st.err != nil {
			return st
		}
	default:
		applied, err := history.versionFiles(ctx, recorded)
		if err != nil {
			st.err = fmt.Errorf("could not read the files of version %s, which stack %q last applied (%v), so pull cannot tell a change of this folder's that was never deployed from the live one — pull again, or pull --force to replace the folder with what is live without that check", recorded, f.Stack, err)
			return st
		}
		st.differs = slices.Collect(maps.Keys(fileSetChanges(local, applied)))
	}
	sort.Strings(st.differs)
	if strings.TrimSpace(f.Manifest.Title) != "" && appliedTitle != "" && !sameName(f.Manifest.Title, appliedTitle) {
		st.differs = append(st.differs, wfdir.ManifestName+" (title)")
	}
	return st
}

// baselineBeforeFirstVersion is how much earlier than the first version's
// commit a baseline's timestamp must be to be counted as read before it. The
// baseline records the row's updatedAt to the second, the commit stamps the
// parent's updatedAt from the server's clock as it starts and committedAt from
// the database's a moment later; two seconds keeps a baseline read just AFTER
// that commit from passing for one read before it.
const baselineBeforeFirstVersion = 2 * time.Second

// unversionedApplied names the files the folder changed since its stack
// applied a row that had NO committed version then and has one now — the case
// the recorded anchor is the row's own id (see draftAppliedState).
//
// What the stack applied is the content the row held before its first version,
// which the server no longer has. This checkout's local baseline holds its
// fingerprint when the baseline was read off THIS live row (not another
// stack's row — the baseline is shared between stacks, BL-4e71 — and not a
// draft) before that first version was committed: an import, a clone, the
// publish that created the row, or a pull. A baseline read after it (a discard
// re-reads live) describes the publish, not what was applied.
//
// Anything else answers an error that says the comparison cannot be made and
// why, rather than guessing either way: a guess of "unchanged" would let a pull
// replace a change that was never deployed, and a guess of "changed" is the
// false refusal this exists to end.
func unversionedApplied(ctx context.Context, f *folder, liveID string, local map[string]string, history draftHistory) ([]string, error) {
	cannot := func(why string) error {
		return fmt.Errorf("%s %s had no committed version when stack %q last applied it (%s records the row's own id as the version it applied), and it has been published since. The server keeps no copy of what the row held before its first version, and %s, so pull cannot tell whether this folder holds a change that was never deployed. If the folder's files are what stack %q last applied, or you do not need to keep a change of its own, `%s pull --force` replaces them with what is live (git keeps what it replaces; preview with `%s pull --dry-run`)",
			f.Kind.Label, liveID, f.Stack, wfdir.LockName, why, f.Stack, f.Kind.Command, f.Kind.Command)
	}
	var base *wfdir.InstanceState
	if f.State != nil {
		base = f.State.For(f.Key)
	}
	switch {
	case base == nil:
		return nil, cannot("this checkout has no local sync baseline for it (a fresh checkout has none)")
	case base.SourceID != liveID || base.SourceLifecycle != api.LifecycleLive:
		return nil, cannot(fmt.Sprintf("this checkout's local sync baseline describes %s, not the live %s", orUnknown(base.SourceID), liveID))
	}
	read, err := time.Parse(time.RFC3339, base.BaselineUpdatedAt)
	if err != nil {
		return nil, cannot("this checkout's local sync baseline does not say when it was read")
	}
	first, err := history.firstCommitted(ctx, liveID)
	if err != nil {
		return nil, cannot(fmt.Sprintf("its version history could not be read (%v)", err))
	}
	switch {
	case first == nil:
		return nil, cannot("its version history lists no version")
	case read.After(*first):
		return nil, cannot("this checkout's local sync baseline was read after that publish")
	case read.Add(baselineBeforeFirstVersion).After(*first):
		// Read before the publish by the clocks, but too close to it to be
		// sure: the stamps are to the second and come from two clocks. Waiting
		// changes neither stamp, so the message names --force, not a retry.
		return nil, cannot(fmt.Sprintf("this checkout's local sync baseline was read %s before that publish, too close to it to tell which came first (the two times are recorded to the second, by different clocks)", first.Sub(read).Round(time.Millisecond)))
	}
	var differs []string
	for path, content := range local {
		if had, ok := base.Files[path]; !ok || had.SHA256 != wfdir.HashString(content) {
			differs = append(differs, path)
		}
	}
	for path := range base.Files {
		if _, ok := local[path]; !ok {
			differs = append(differs, path)
		}
	}
	return differs, nil
}

// versionListCap is the most versions the server lists for one row (rworkflow
// draftListLimit, rdataapp and rmodule versionHistoryLimit): a list this long
// may not reach the oldest.
const versionListCap = 100

// firstCommitted reads when the oldest version of a newest-first version list
// was committed, nil when the row has none. A FULL list is refused rather than
// read: its last element is the 100th-newest, not the first, and a baseline
// read after the real first commit would pass for one read before it.
//
// The answer is compared with a baseline's BaselineUpdatedAt under a 2-second
// margin (baselineBeforeFirstVersion). Both stamps are the server's — the
// parent's updatedAt from the API pod's clock as the commit starts, committedAt
// from the database's — so the margin assumes those two clocks agree to well
// under a second, which a managed database and its pods do.
func firstCommitted[T any](list func(ctx context.Context, id string) ([]T, error), at func(T) *time.Time) func(ctx context.Context, id string) (*time.Time, error) {
	return func(ctx context.Context, id string) (*time.Time, error) {
		versions, err := list(ctx, id)
		if err != nil || len(versions) == 0 {
			return nil, err
		}
		if len(versions) >= versionListCap {
			return nil, fmt.Errorf("it has at least %d versions, and the server lists only the newest %d, so its first one cannot be read", versionListCap, versionListCap)
		}
		return at(versions[len(versions)-1]), nil
	}
}

// apply decides the plan by what the folder and live each changed since this
// stack last applied: live unmoved leaves the folder's own changes alone (there
// is nothing to absorb, and the next apply deploys them); both moved names what
// the pull would replace, which runPull refuses without --force.
func (st draftAppliedState) apply(plan *pullPlan) *pullPlan {
	plan.undeployedErr = st.err
	switch {
	case st.err != nil:
	case !st.moved && (len(plan.files) > 0 || plan.manifest != nil):
		kept := st.differs
		if plan.manifest != nil && !slices.ContainsFunc(kept, func(p string) bool { return strings.HasPrefix(p, wfdir.ManifestName) }) {
			kept = append(kept, wfdir.ManifestName)
		}
		return &pullPlan{files: map[string]*string{}, kept: kept, leftAlone: true, notes: plan.notes}
	case st.moved && len(st.differs) > 0:
		plan.undeployed = st.differs
	}
	return plan
}

// draftKindPull is one draft kind's part of a pull: the reads and the steps
// that differ between a workflow, a data app and a module. pullDraftKind is the
// skeleton they share — get live, refuse a row that is not live, list its files,
// refuse a path the folder cannot hold, plan the files and the manifest, check
// the plan with the kind's own parity function, compare with what the stack
// last applied, and wire the gate, the anchor and the baseline.
type draftKindPull[R, F any] struct {
	kind wfdir.Kind
	// id is the row the binding names.
	id  string
	get func(ctx context.Context, id string) (*R, error)
	row func(*R) liveDraftKindRow
	// list reads a row's files (the live row's, or a version's); paths and
	// content read them — content in the folder's disk form.
	list    func(ctx context.Context, id string) ([]F, error)
	paths   func([]F) []string
	content func([]F) map[string]string
	// draftOpen reads whether the caller has a draft of the live row open.
	draftOpen func(ctx context.Context, id string) (bool, error)
	// metadata writes the live metadata the folder manages into m (a copy of
	// the manifest): whether anything changed, the literal ids it spelled, or a
	// refusal (pullInexpressible) for a value the folder cannot declare.
	metadata func(m *wfdir.Manifest, live *R) (changed bool, literals []pullLiteral, err error)
	// holds is the kind's *RowHoldsFolder for the planned folder.
	holds func(planned *folder, live *R, remote map[string]string) bool
	// fileLiterals names the literal ids a pulled file spells (nil: none).
	fileLiterals   func(path string, before *string, after string) []pullLiteral
	head           headReader
	firstCommitted func(ctx context.Context, id string) (*time.Time, error)
	// gate is the write gate over the live row (nil: the kind has none).
	gate func(ctx context.Context, live *R, plan *pullPlan) error
	// push is the kind's live-first push, answering whether it found the
	// folder up to date.
	push     func(ctx context.Context, f *folder) (upToDate bool, err error)
	baseline func(live *R, files []F) *wfdir.InstanceState
}

func pullDraftKind[R, F any](ctx context.Context, f *folder, k draftKindPull[R, F]) (*pullPlan, error) {
	live, err := k.get(ctx, k.id)
	if err != nil {
		return nil, pullRowGone(f, k.id, err)
	}
	row := k.row(live)
	if row.Lifecycle != api.LifecycleLive {
		return nil, pullNotLive(f, k.id, row.Lifecycle)
	}
	files, err := k.list(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("read the files of %s: %w", row.ID, err)
	}
	if err := wfdir.CheckLocalPaths(k.paths(files), k.kind); err != nil {
		return nil, pullInexpressible(f, err.Error())
	}
	draftOpen, draftErr := k.draftOpen(ctx, row.ID)
	local, _, err := readLocalFiles(f.Root, k.kind)
	if err != nil {
		return nil, err
	}
	remote := k.content(files)

	m, err := copyManifest(f.Manifest)
	if err != nil {
		return nil, err
	}
	changed, metaLiterals, err := k.metadata(m, live)
	if err != nil {
		return nil, err
	}
	if !k.holds(f.withManifest(m), live, remote) {
		return nil, pullInexpressible(f, "a pushed copy of it would still differ from live")
	}

	plan := &pullPlan{files: fileSetChanges(local, remote)}
	if changed {
		plan.manifest = m
	}
	if k.fileLiterals != nil {
		for _, path := range sortedKeys(plan.files) {
			if after := plan.files[path]; after != nil {
				plan.literals = append(plan.literals, k.fileLiterals(path, localOrNil(local, path), *after)...)
			}
		}
	}
	plan.literals = append(plan.literals, metaLiterals...)
	plan.notes = draftNotes(f, draftOpen, draftErr)
	plan = readDraftApplied(ctx, f, row.ID, row.Title, local, remote, draftHistory{
		head: k.head,
		versionFiles: func(ctx context.Context, id string) (map[string]string, error) {
			files, err := k.list(ctx, id)
			return k.content(files), err
		},
		firstCommitted: k.firstCommitted,
	}).apply(plan)
	if plan.leftAlone {
		return plan, nil
	}
	if k.gate != nil {
		plan.gate = func(ctx context.Context) error { return k.gate(ctx, live, plan) }
	}
	plan.anchor = func(ctx context.Context, f *folder) ([]string, error) {
		upToDate, err := k.push(ctx, f)
		if err != nil {
			return nil, err
		}
		if !upToDate {
			return nil, fmt.Errorf("the live-first push found something to deploy")
		}
		return nil, nil
	}
	plan.afterAnchor = func(f *folder) error {
		if draftOpen || draftErr != nil {
			// The baseline describes the DRAFT a push writes to, and that draft
			// is the author's: left exactly as it is.
			return nil
		}
		f.State.Set(f.Key, k.baseline(live, files))
		return wfdir.SaveState(f.Root, f.State)
	}
	return plan, nil
}

// draftOpenOf adapts a kind's GetXDraft to "is a draft open".
func draftOpenOf[R any](get func(ctx context.Context, id string) (*R, error)) func(ctx context.Context, id string) (bool, error) {
	return func(ctx context.Context, id string) (bool, error) {
		draft, err := get(ctx, id)
		return draft != nil, err
	}
}

func pullWorkflow(ctx context.Context, client *api.Client, f *folder) (*pullPlan, error) {
	return pullDraftKind(ctx, f, draftKindPull[api.Workflow, api.WorkflowFile]{
		kind: f.Kind, id: f.Binding.WorkflowID,
		get: client.GetWorkflow,
		row: func(w *api.Workflow) liveDraftKindRow {
			return liveDraftKindRow{ID: w.ID, Lifecycle: w.Lifecycle, Title: w.Title}
		},
		list:      client.ListWorkflowFiles,
		paths:     pathsOf,
		content:   func(files []api.WorkflowFile) map[string]string { return contentByPath(f.Codec, files) },
		draftOpen: draftOpenOf(client.GetWorkflowDraft),
		metadata: func(m *wfdir.Manifest, live *api.Workflow) (bool, []pullLiteral, error) {
			changed := false
			if m.Title != "" && !sameName(m.Title, live.Title) {
				m.Title, changed = live.Title, true
			}
			if m.Entrypoint != "" && live.Entrypoint != "" && m.Entrypoint != live.Entrypoint {
				m.Entrypoint, changed = live.Entrypoint, true
			}
			if m.ManagesParameters() && !sameParameters(f.Codec.parametersToWire(m.DeclaredParameters()), live.Parameters) {
				m.SetParameters(f.Codec.parametersToDisk(live.Parameters))
				changed = true
			}
			if m.ManagesReportingTimezone() && effectiveDeclaredZone(m.DeclaredReportingTimezone()) != live.ReportingTimezone {
				if live.ReportingTimezone == "" {
					return false, nil, pullInexpressible(f, "the live workflow declares no reporting timezone, and this folder's \"reportingTimezone\" can only declare one — remove the key to stop managing it, then pull")
				}
				m.SetReportingTimezone(live.ReportingTimezone)
				changed = true
			}
			if m.Runtime != 0 && live.RuntimeVersion != 0 && m.RuntimeVersion() != live.RuntimeVersion {
				if !wfdir.ValidRuntime(live.RuntimeVersion) {
					return false, nil, pullInexpressible(f, fmt.Sprintf("the live workflow runs runtime %d, which this ronja does not know — upgrade it (`ronja update`)", live.RuntimeVersion))
				}
				m.Runtime, changed = live.RuntimeVersion, true
			}
			var literals []pullLiteral
			if changed && m.ManagesParameters() {
				literals = parameterLiterals(f, m)
			}
			return changed, literals, nil
		},
		holds: func(g *folder, live *api.Workflow, remote map[string]string) bool {
			return workflowRowHoldsFolder(g, live, remote, remote)
		},
		fileLiterals:   markerLiterals,
		head:           workflowHeadReader(client),
		firstCommitted: firstCommitted(client.ListWorkflowVersions, func(v api.Workflow) *time.Time { return v.CommittedAt }),
		gate: func(ctx context.Context, live *api.Workflow, _ *pullPlan) error {
			return gateWorkflowRow(ctx, client, f, live)
		},
		push: func(ctx context.Context, f *folder) (bool, error) {
			res, err := runPush(ctx, f, pushOptions{Validate: true, LiveFirst: true})
			return err == nil && res.UpToDate, err
		},
		baseline: func(live *api.Workflow, files []api.WorkflowFile) *wfdir.InstanceState {
			return baselineFrom(f.Codec, live, files)
		},
	})
}

func pullDataApp(ctx context.Context, client *api.Client, f *folder) (*pullPlan, error) {
	return pullDraftKind(ctx, f, draftKindPull[api.DataApp, api.DataAppFile]{
		kind: wfdir.DataAppKind, id: f.Binding.DataAppID,
		get: client.GetDataApp,
		row: func(a *api.DataApp) liveDraftKindRow {
			return liveDraftKindRow{ID: a.ID, Lifecycle: a.Lifecycle, Title: a.Name}
		},
		list:      client.ListDataAppFiles,
		paths:     appPathsOf,
		content:   func(files []api.DataAppFile) map[string]string { return appContentByPath(f.Codec, files) },
		draftOpen: draftOpenOf(client.GetDataAppDraft),
		metadata: func(m *wfdir.Manifest, live *api.DataApp) (bool, []pullLiteral, error) {
			changed := false
			if m.Title != "" && !sameName(m.Title, live.Name) {
				m.Title, changed = live.Name, true
			}
			var literals []pullLiteral
			if m.ManagesAccess() && len(diffAccess(live.DataAppAccess, f.declaredAccess())) > 0 {
				before := m.DeclaredAccess()
				after := aliasedAccess(live.DataAppAccess, f.Codec)
				m.SetAccess(after)
				changed = true
				literals = accessLiteralsOf(before, after)
			}
			return changed, literals, nil
		},
		holds: func(g *folder, live *api.DataApp, remote map[string]string) bool {
			return appRowHoldsFolder(g, live, remote, remote)
		},
		fileLiterals: func(path string, before *string, after string) []pullLiteral {
			out := markerLiterals(path, before, after)
			if isAppSource(path) {
				out = append(out, quotedIDLiterals(path, before, after)...)
			}
			return out
		},
		head:           dataAppHeadReader(client),
		firstCommitted: firstCommitted(client.ListDataAppVersions, func(v api.DataApp) *time.Time { return v.CommittedAt }),
		gate: func(ctx context.Context, live *api.DataApp, plan *pullPlan) error {
			if f.Manifest.ManagesAccess() || (plan.manifest != nil && plan.manifest.ManagesAccess()) {
				return gateAppAccess(ctx, client, f, live.DataAppAccess)
			}
			return nil
		},
		push: func(ctx context.Context, f *folder) (bool, error) {
			res, err := runAppPush(ctx, f, appPushOptions{Validate: true, LiveFirst: true})
			return err == nil && res.UpToDate, err
		},
		baseline: func(live *api.DataApp, files []api.DataAppFile) *wfdir.InstanceState {
			return baselineFromApp(f.Codec, live, files)
		},
	})
}

func pullModule(ctx context.Context, client *api.Client, f *folder) (*pullPlan, error) {
	return pullDraftKind(ctx, f, draftKindPull[api.Module, api.ModuleFile]{
		kind: wfdir.ModuleKind, id: f.Binding.ModuleID,
		get: client.GetModule,
		row: func(m *api.Module) liveDraftKindRow {
			return liveDraftKindRow{ID: m.ID, Lifecycle: m.Lifecycle, Title: m.Title}
		},
		list:  client.ListModuleFiles,
		paths: modulePathsOf,
		// A module's files are marker-free (refuseModuleMarkers): nothing to
		// de-alias, so the live bytes are the disk form.
		content:   moduleContentByPath,
		draftOpen: draftOpenOf(client.GetModuleDraft),
		metadata: func(m *wfdir.Manifest, live *api.Module) (bool, []pullLiteral, error) {
			changed := false
			if strings.TrimSpace(m.Title) != "" && !sameName(m.Title, live.Title) {
				m.Title, changed = live.Title, true
			}
			if strings.TrimSpace(m.Name) != "" && strings.TrimSpace(m.Name) != live.Name {
				m.Name, changed = live.Name, true
			}
			return changed, nil, nil
		},
		holds: func(g *folder, live *api.Module, remote map[string]string) bool {
			return moduleRowHoldsFolder(g, live, remote, remote)
		},
		head:           moduleHeadReader(client),
		firstCommitted: firstCommitted(client.ListModuleVersions, func(v api.Module) *time.Time { return v.CommittedAt }),
		push: func(ctx context.Context, f *folder) (bool, error) {
			res, err := runModulePush(ctx, f, modulePushOptions{LiveFirst: true})
			return err == nil && res.UpToDate, err
		},
		baseline: moduleBaselineFrom,
	})
}

// draftNotes says what a pull did not touch: the open draft, which it never
// reads and never writes. A failed check is said too — the pull went ahead
// from live, which it would have done either way.
func draftNotes(f *folder, draftOpen bool, err error) []string {
	switch {
	case err != nil:
		return []string{fmt.Sprintf("could not check for an open draft (%v); the pull read the live version, as it always does, and left any draft alone", err)}
	case draftOpen:
		return []string{fmt.Sprintf("you have an open draft of this %s. The pull read the LIVE version and left the draft exactly as it is; a push of the draft is checked for drift as before, and publishing it would put its content live over what was just pulled", f.Kind.Label)}
	}
	return nil
}

func localOrNil(local map[string]string, path string) *string {
	if s, ok := local[path]; ok {
		return &s
	}
	return nil
}

// parameterLiterals names the select-parameter optionsQuery references the
// pulled parameters spell as literal ids.
func parameterLiterals(f *folder, m *wfdir.Manifest) []pullLiteral {
	var before string
	for _, p := range f.Manifest.DeclaredParameters() {
		before += p.OptionsQuery + "\n"
	}
	var out []pullLiteral
	for _, p := range m.DeclaredParameters() {
		for _, l := range markerLiterals(wfdir.ManifestName, &before, p.OptionsQuery) {
			l.Line = 0
			l.Field = "parameters." + p.Name + ".optionsQuery"
			out = append(out, l)
		}
	}
	return out
}

// accessLiteralsOf names the ids a pulled access block lists that no alias of
// the folder names and the folder did not already list.
func accessLiteralsOf(before, after api.DataAppAccess) []pullLiteral {
	had := map[string]bool{}
	for _, dep := range accessDependencies(&before) {
		for _, id := range *dep.IDs {
			had[dep.Key+"\x00"+id] = true
		}
	}
	var out []pullLiteral
	for _, dep := range accessDependencies(&after) {
		for _, id := range *dep.IDs {
			if had[dep.Key+"\x00"+id] || !isAnyResourceID(id) {
				continue
			}
			out = append(out, pullLiteral{Path: wfdir.ManifestName, Field: "access." + dep.Key, Kind: dep.Kind, ID: id})
		}
	}
	return out
}

// quotedIDLiterals names the quoted id literals (`"table-…"`) a data app's
// source gained — references the runtime reaches that no marker carries, and
// so no alias can name.
func quotedIDLiterals(path string, before *string, after string) []pullLiteral {
	had := map[string]bool{}
	if before != nil {
		for _, m := range idLiteralPattern.FindAllStringSubmatch(*before, -1) {
			had[m[1]] = true
		}
	}
	var out []pullLiteral
	for i, line := range strings.Split(after, "\n") {
		for _, m := range idLiteralPattern.FindAllStringSubmatch(line, -1) {
			if had[m[1]] {
				continue
			}
			out = append(out, pullLiteral{Path: path, Line: i + 1, Kind: kindOfID(m[1]), ID: m[1]})
		}
	}
	return out
}

// kindOfID is the resource kind an id's prefix names, "" for none.
func kindOfID(id string) string {
	for _, kind := range markers.Kinds() {
		if markers.IsResourceID(kind, id) {
			return kind
		}
	}
	return ""
}

func isAnyResourceID(id string) bool { return kindOfID(id) != "" }
