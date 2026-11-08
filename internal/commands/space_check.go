package commands

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A stack that declares a "spaceID" deploys into one Space and never writes or
// runs outside it. Each refusal below is a machine contract: add, never rename.
const (
	// syncReasonSpaceMismatch — the stack's feature is not in its declared
	// Space (a feature id pasted from another stack, say).
	syncReasonSpaceMismatch = "space_mismatch"
	// syncReasonBindSpaceMismatch — the folder would write, run or use a
	// secret outside the stack's Space, or in a row in no feature, through a
	// bind or a raw id; or write files from a stack outside the organization's
	// default Space.
	syncReasonBindSpaceMismatch = "bind_space_mismatch"
	// syncReasonAddressSpaceMismatch — an address resolves to a row outside the
	// stack's Space, or names a folder whose stack of the same name declares
	// another Space.
	syncReasonAddressSpaceMismatch = "address_space_mismatch"
)

// spaceFindings is what checkSpaces found: the refusal, if any, and the
// warnings `sync check` reports.
type spaceFindings struct {
	refusal  error
	warnings []string
	// files are the folder's local files, when a full check read them.
	files map[string]string
}

// checkSpaces holds a stack that declares a Space to it: the stack's feature
// must be there (space_mismatch); with full set, so must every row a
// dependency declared to write or run is bound to, and every secret the folder
// binds or names (bind_space_mismatch), and any other raw id the code reads
// from another Space is a warning. What the code actually writes and runs is
// the write gate's (spaceRule). A stack with no "spaceID" is not checked at
// all.
func (f *folder) checkSpaces(ctx context.Context, full bool) spaceFindings {
	if f.Stack == "" {
		return spaceFindings{}
	}
	stack, declared := f.Manifest.Stacks[f.Stack]
	if !declared || stack.SpaceID == "" {
		return spaceFindings{}
	}
	l := f.spaceLookup(ctx)
	var out spaceFindings
	refuse := func(reason, detail string) spaceFindings {
		out.refusal = &managedRefusal{Reason: reason, Detail: detail}
		return out
	}
	// The command's first requests carrying the Space. describeSpace swallows a
	// failed read, and the client's latch then refuses the feature read with
	// the same answer, so an instance that does not confirm the Space is
	// refused below.
	here := l.describeSpace(stack.SpaceID)
	if stack.Space != "" && l.spaceName(stack.SpaceID) != "" && l.spaceName(stack.SpaceID) != stack.Space {
		fmt.Fprintf(os.Stderr, "  Note: stack %q is labelled Space %s in %s, and that Space is now called %s — update its \"space\" label.\n",
			f.Stack, stack.Space, wfdir.ManifestName, l.spaceName(stack.SpaceID))
	}
	if featureID := f.Binding.FeatureID; featureID != "" {
		feature, err := l.feature(featureID)
		switch {
		case err != nil:
			out.refusal = fmt.Errorf("check which Space stack %q's feature %s is in: %w — the stack declares Space %s, and nothing was written", f.Stack, featureID, err, orUnknown(stack.Space))
			if _, refused := asSpaceEntry(err); refused {
				out.refusal = err
			}
			return out
		case feature.SpaceID == "":
			out.refusal = fmt.Errorf("stack %q declares a Space, and %s does not say which Space feature %s is in — it is older than this CLI. Use a `ronja` that matches it, or remove \"spaceID\" from the stack in %s; nothing was written", f.Stack, f.Resolved.URL, featureID, wfdir.ManifestName)
			return out
		case feature.SpaceID != stack.SpaceID:
			return refuse(syncReasonSpaceMismatch, fmt.Sprintf("stack %q is declared for %s, and its feature %s (%s) is in %s. Point the stack's \"featureID\" at a feature in %s, or, if the stack really is for %s, change its \"spaceID\" in %s. Nothing was written",
				f.Stack, here, feature.Name, featureID, l.describeSpace(feature.SpaceID), here, l.describeSpace(feature.SpaceID), wfdir.ManifestName))
		}
	}
	if !full {
		return out
	}
	files, _, err := readLocalFiles(f.Root, f.Kind)
	if err != nil {
		out.refusal = err
		return out
	}
	out.files = files
	problems, err := f.bindSpaceProblems(l, stack.SpaceID)
	if err != nil {
		out.refusal = err
		return out
	}
	secrets, warnings, err := f.literalSpaceProblems(l, stack.SpaceID, files)
	if err != nil {
		out.refusal = err
		return out
	}
	if problems = append(problems, secrets...); len(problems) > 0 {
		return refuse(syncReasonBindSpaceMismatch, fmt.Sprintf("stack %q is declared for %s, and it would write, run or use a secret outside it:\n    %s\n  Bind each one in %s's \"bind\" to a row in %s — a stack reads other Spaces, but writes, runs and uses secrets only in its own. Nothing was written",
			f.Stack, here, strings.Join(problems, "\n    "), describeStackFor(f.Stack), here))
	}
	out.warnings = warnings
	return out
}

// bindSpaceProblems lists each write or run bind, and each secret bind, of the
// selected stack that names a row outside spaceID; a refusal of the Space
// itself is returned as the error.
func (f *folder) bindSpaceProblems(l *spaceLookup, spaceID string) ([]string, error) {
	var out []string
	for _, alias := range f.Manifest.AliasNames() {
		dep := f.Manifest.Dependencies[alias]
		// A secret is the Space's own credential to an outside system, so one
		// is placed however it is used.
		secret := dep.Kind == markers.KindSecret
		if dep.ByAddress() || dep.Output || !(dep.Write || dep.Run || secret) || markers.IsPathKind(dep.Kind) {
			continue
		}
		id := f.Bind[alias]
		if id == "" {
			continue
		}
		verb := "written"
		switch {
		case secret:
			verb = "used"
		case dep.Run && !dep.Write:
			verb = "run"
		}
		in, featureID, _, ok, err := l.rowSpace(dep.Kind, id)
		if _, refused := asSpaceEntry(err); refused {
			return nil, err
		}
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%q is %s by this folder and names %s, a %s this ronja cannot place in a Space", alias, verb, id, dep.Kind))
		case err != nil:
			out = append(out, fmt.Sprintf("%q is %s by this folder and names %s, which this credential cannot read (%v), so nothing can say which Space it is in", alias, verb, id, err))
		case featureID == "":
			out = append(out, fmt.Sprintf("%q is %s by this folder and names %s, which is in no feature and so in no Space", alias, verb, id))
		case in != spaceID:
			out = append(out, fmt.Sprintf("%q is %s by this folder and names %s in %s", alias, verb, id, l.describeSpace(in)))
		}
	}
	return out, nil
}

// literalRead is one raw id a folder's code reads through a marker.
type literalRead struct {
	kind, id, where string
}

// rawIDReads lists the raw ids the folder's code reads, once each. A raw id it
// writes or runs is the write gate's, which places it from validate's own
// scan.
func rawIDReads(files map[string]string) []literalRead {
	seen := map[string]bool{}
	var out []literalRead
	for _, path := range sortedPaths(files) {
		for _, o := range markers.Scan(files[path]) {
			kind := o.Family.Kind
			if o.Positional || !markers.IsResourceID(kind, o.Arg) || seen[o.Arg] ||
				kind == markers.KindWorkflow || kind == markers.KindAgent || kind == markers.KindCodex {
				continue
			}
			seen[o.Arg] = true
			out = append(out, literalRead{kind: kind, id: o.Arg, where: path})
		}
	}
	return out
}

// literalSpaceProblems places the raw ids the code reads: a secret in another
// Space, or in none, is refused like a secret bind; any other row read in
// another Space is a `sync check` warning, allowed and better declared so each
// stack binds its own. A refusal of the Space itself is returned as the error.
func (f *folder) literalSpaceProblems(l *spaceLookup, spaceID string, files map[string]string) (refused, warnings []string, err error) {
	for _, u := range rawIDReads(files) {
		in, featureID, _, ok, rowErr := l.rowSpace(u.kind, u.id)
		if _, spaceRefused := asSpaceEntry(rowErr); spaceRefused {
			return nil, nil, rowErr
		}
		if u.kind == markers.KindSecret {
			switch {
			case rowErr != nil:
				refused = append(refused, fmt.Sprintf("%s uses the secret %s, which this credential cannot read (%v), so nothing can say which Space it is in", u.where, u.id, rowErr))
			case featureID == "":
				refused = append(refused, fmt.Sprintf("%s uses the secret %s, which is in no feature and so in no Space", u.where, u.id))
			case in != spaceID:
				refused = append(refused, fmt.Sprintf("%s uses the secret %s in %s", u.where, u.id, l.describeSpace(in)))
			}
			continue
		}
		if !ok || rowErr != nil || featureID == "" || in == spaceID {
			continue
		}
		warnings = append(warnings, fmt.Sprintf("%s reads %s in %s, outside stack %q's %s — declare it as a dependency so this stack gets its own", u.where, u.id, l.describeSpace(in), f.Stack, l.describeSpace(spaceID)))
	}
	sort.Strings(refused)
	sort.Strings(warnings)
	return refused, warnings, nil
}

// addressSpaceMismatch is the same-Space half of the address rule: two stacks
// of one name in two folders that both declare a Space must declare the same
// one, or a Dev deployment could resolve an address to a Main row.
func addressSpaceMismatch(here, there wfdir.Stack, from, stack string) *wfdir.AddressError {
	if here.SpaceID == "" || there.SpaceID == "" || here.SpaceID == there.SpaceID {
		return nil
	}
	return &wfdir.AddressError{Reason: syncReasonAddressSpaceMismatch, Detail: fmt.Sprintf(
		"%s's stack %q is declared for Space %s and this folder's for Space %s — an address resolves only within one Space",
		from, stack, orUnknown(spaceLabel(there)), orUnknown(spaceLabel(here)))}
}

func spaceLabel(s wfdir.Stack) string {
	if s.Space != "" {
		return s.Space
	}
	return s.SpaceID
}

// treeStackSpace is the Space label the first folder declaring stack under a
// tree carries, for the report's stack line.
func treeStackSpace(found []discoveredFolder, stack string) string {
	for _, folder := range found {
		if folder.Manifest == nil {
			continue
		}
		if st, ok := folder.Manifest.Stacks[stack]; ok && st.SpaceID != "" {
			return spaceLabel(st)
		}
	}
	return ""
}

// stackSpaceID is the Space the selected stack declares, "" for none.
func (f *folder) stackSpaceID() string {
	if f.Stack == "" || f.Manifest == nil {
		return ""
	}
	return f.Manifest.Stacks[f.Stack].SpaceID
}

// gateApplies reports whether the write gate runs for this folder: its feature
// rule in an adopted folder, its Space rule wherever the stack declares a Space.
func (f *folder) gateApplies() bool {
	return f.adoptedHere() || f.stackSpaceID() != ""
}

// spaceRule is the write gate's Space half. Every target the gate lets out of
// the stack's feature — bound on purpose in an adopted folder, or any target
// in a folder the feature rule does not cover — must still be in the stack's
// Space, or the deployment is refused bind_space_mismatch.
type spaceRule struct {
	f       *folder
	l       *spaceLookup
	spaceID string
	outside []gateOutside
}

// spaceRuleFor is the folder's Space rule, nil when its stack declares no
// Space. Its methods are no-ops on nil.
func (f *folder) spaceRuleFor(ctx context.Context) *spaceRule {
	spaceID := f.stackSpaceID()
	if spaceID == "" {
		return nil
	}
	return &spaceRule{f: f, l: f.spaceLookup(ctx), spaceID: spaceID}
}

// spaceLookup is the folder's one Space lookup, so a row is read once however
// many checks place it.
func (f *folder) spaceLookup(ctx context.Context) *spaceLookup {
	if f.spaces == nil {
		f.spaces = newSpaceLookupOn(ctx, f.client())
	}
	return f.spaces
}

// placeOwnRow holds a stack that records a Space to the row the folder itself
// deploys: its workflow, app or module, or one of a pipeline's tables, by the
// id its binding records. A row outside the Space is another Space's, and a
// push would deploy into it. featureID is the feature the row's own read
// named; readErr is why that read failed, and only a 403 is answered here (a
// 404 is the push's "no longer exists"). An adopted folder is placed by its
// claims instead.
func (f *folder) placeOwnRow(ctx context.Context, kind, id, featureID string, readErr error) error {
	spaceID := f.stackSpaceID()
	if spaceID == "" || id == "" || f.adoptedHere() {
		return nil
	}
	l := f.spaceLookup(ctx)
	refuse := func(where string) error {
		here := l.describeSpace(spaceID)
		return &managedRefusal{Reason: syncReasonBindSpaceMismatch, Detail: fmt.Sprintf(
			"stack %q is declared for %s, and the %s %s that %s records for it %s, so a push would deploy into another Space's row. Clear that id for stack %q so the next push creates the stack's own %s in %s. Nothing was written",
			f.Stack, here, kind, id, f.bindingFiles(), where, f.Stack, kind, here)}
	}
	if readErr != nil {
		if api.StatusOf(readErr) == 403 {
			return refuse("cannot be read with this credential, so nothing can say which Space it is in")
		}
		return nil
	}
	if featureID != "" && featureID == f.Binding.FeatureID {
		return nil
	}
	if featureID == "" {
		return refuse("is in no feature and so in no Space")
	}
	feature, err := l.feature(featureID)
	switch {
	case err != nil && unreachable(err):
		return refuse(fmt.Sprintf("is in feature %s, which this credential cannot read", featureID))
	case err != nil:
		return fmt.Errorf("check which Space feature %s is in: %w — nothing was written", featureID, err)
	case feature.SpaceID != spaceID:
		return refuse(fmt.Sprintf("is in feature %s, in %s", featureID, l.describeSpace(feature.SpaceID)))
	}
	return nil
}

// boundRow is the row the folder's binding deploys — its workflow, app or
// module, or a pipeline's first table — or "" for none.
func (f *folder) boundRow() (kind, id string) {
	b := f.Binding
	switch {
	case b.WorkflowID != "":
		return markers.KindWorkflow, b.WorkflowID
	case b.DataAppID != "":
		return markers.KindDataApp, b.DataAppID
	case b.ModuleID != "":
		return markers.KindModule, b.ModuleID
	}
	for _, path := range sortedKeys(b.Tables) {
		return markers.KindTable, b.Tables[path]
	}
	return "", ""
}

// placeOwnRows is placeOwnRow for every row the folder's binding records,
// each read once: what `sync check` and a dry-run `sync apply` say, since no
// push reads them. An automation folder's rows are listed in the stack's own
// feature, so they are placed by construction.
func (f *folder) placeOwnRows(ctx context.Context) error {
	if f.stackSpaceID() == "" || f.adoptedHere() {
		return nil
	}
	l := f.spaceLookup(ctx)
	place := func(kind, id string) error {
		featureID, _, _, err := l.rowFeature(kind, id)
		return f.placeOwnRow(ctx, kind, id, featureID, err)
	}
	b := f.Binding
	for kind, id := range map[string]string{markers.KindWorkflow: b.WorkflowID, markers.KindDataApp: b.DataAppID, markers.KindModule: b.ModuleID} {
		if id == "" {
			continue
		}
		if err := place(kind, id); err != nil {
			return err
		}
	}
	for _, path := range sortedKeys(b.Tables) {
		if err := place(markers.KindTable, b.Tables[path]); err != nil {
			return err
		}
	}
	return nil
}

// unreachable reports whether err is the server saying the caller cannot read
// the row, as against the read itself failing.
func unreachable(err error) bool {
	status := api.StatusOf(err)
	return status == 400 || status == 403 || status == 404
}

// placeRead places a target the gate read: featureID is the feature the read
// answered, readErr why it answered none. A read that failed for any reason
// but the row being out of reach is returned, since nothing was decided.
func (r *spaceRule) placeRead(o gateOutside, featureID string, readErr error) error {
	if r == nil {
		return nil
	}
	if readErr != nil {
		if !unreachable(readErr) {
			return fmt.Errorf("check which Space %s is in: %w — nothing was written", o.Target, readErr)
		}
		o.Why = "which this credential cannot read, so nothing can say which Space it is in"
		r.outside = append(r.outside, o)
		return nil
	}
	return r.placeFeature(o, featureID)
}

// placeFeature places a target that lives in featureID.
func (r *spaceRule) placeFeature(o gateOutside, featureID string) error {
	if r == nil || featureID != "" && featureID == r.f.Binding.FeatureID {
		return nil
	}
	if featureID == "" {
		o.Why = "which is in no feature and so in no Space"
		r.outside = append(r.outside, o)
		return nil
	}
	feature, err := r.l.feature(featureID)
	switch {
	case err != nil && unreachable(err):
		o.Feature, o.Why = featureID, fmt.Sprintf("in feature %s, which this credential cannot read, so nothing can say which Space it is in", featureID)
	case err != nil:
		return fmt.Errorf("check which Space feature %s is in: %w — nothing was written", featureID, err)
	case feature.SpaceID == r.spaceID:
		return nil
	default:
		o.Feature, o.Why = featureID, fmt.Sprintf("in feature %s, which is in %s", featureID, r.l.describeSpace(feature.SpaceID))
	}
	r.outside = append(r.outside, o)
	return nil
}

// placeRow places a row by its own read, never by what another folder's stack
// says about it.
func (r *spaceRule) placeRow(o gateOutside, kind, id string) error {
	if r == nil {
		return nil
	}
	_, featureID, _, ok, err := r.l.rowSpace(kind, id)
	if !ok {
		o.Why = fmt.Sprintf("a %s this ronja cannot place in a Space", kind)
		r.outside = append(r.outside, o)
		return nil
	}
	return r.placeRead(o, featureID, err)
}

// placeFolder places a folder write scope. A run writes files as a machine
// principal, whose paths resolve in the organization's default Space whatever
// its feature's Space is (BL-c4d8), so only a stack in the default Space may
// write one.
func (r *spaceRule) placeFolder(o gateOutside) error {
	if r == nil {
		return nil
	}
	space, err := r.l.space(r.spaceID)
	if err != nil {
		return fmt.Errorf("check whether stack %q's Space %s is the organization's default Space, where a run's file writes land: %w — nothing was written", r.f.Stack, r.spaceID, err)
	}
	if space.IsDefault {
		return nil
	}
	o.Why = fmt.Sprintf("a run's file writes land in the organization's default Space, not in %s — write a table in the stack's own feature instead", r.l.describeSpace(r.spaceID))
	r.outside = append(r.outside, o)
	return nil
}

// refuseUnplaced adds a target the rule cannot place at all.
func (r *spaceRule) refuseUnplaced(o gateOutside, why string) {
	if r == nil {
		return
	}
	o.Why = why
	r.outside = append(r.outside, o)
}

// refusal is the Space rule's one refusal, nil when every target was inside.
func (r *spaceRule) refusal() error {
	if r == nil || len(r.outside) == 0 {
		return nil
	}
	sort.SliceStable(r.outside, func(i, j int) bool { return r.outside[i].Where < r.outside[j].Where })
	lines := make([]string, 0, len(r.outside))
	for _, o := range r.outside {
		lines = append(lines, "    "+o.String())
	}
	here := r.l.describeSpace(r.spaceID)
	return &managedRefusal{Reason: syncReasonBindSpaceMismatch, Detail: fmt.Sprintf(
		"stack %q is declared for %s, and this folder would write or run outside it:\n%s\n  A stack reads other Spaces, but writes and runs only in its own: bind each target for this stack to a row in %s, or write into the stack's own feature. Nothing was written",
		r.f.Stack, here, strings.Join(lines, "\n"), here)}
}

// printStackLine is a single-folder push report's first line, as a tree
// apply's names it: the stack the folder deployed through and, when it
// declares one, its Space.
func printStackLine(f *folder) {
	if f == nil || f.Stack == "" || f.Manifest == nil {
		return
	}
	if st := f.Manifest.Stacks[f.Stack]; st.SpaceID != "" {
		fmt.Fprintf(os.Stdout, "  Stack:      %s · Space %s\n", f.Stack, spaceLabel(st))
		return
	}
	fmt.Fprintf(os.Stdout, "  Stack:      %s\n", f.Stack)
}
