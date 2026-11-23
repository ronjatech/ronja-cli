package commands

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// proposeSetup is what propose did to a lone folder before proposing it.
type proposeSetup struct {
	Folder    string `json:"folder"`
	Stack     string `json:"stack"`
	FeatureID string `json:"featureID"`
	// StackAdded reports that ronja.json gained the stack: the folder's
	// binding on this instance, named.
	StackAdded bool   `json:"stackAdded,omitempty"`
	FolderID   string `json:"folderID"`
	Claimed    int    `json:"claimed"`
	// TookOverFrom names each folder --take-over released the row from.
	TookOverFrom []string `json:"tookOverFrom,omitempty"`
	// adopted: the folder had its id already, so only --take-over claimed.
	adopted bool
	// lockChanged: naming a cloned folder's binding as a stack moves its ids
	// into ronja.lock.json, which is committed too.
	lockChanged bool
}

// setUpLoneFolder adopts the one folder under base when Ronja keeps no state
// for it and its ronja.json names the feature it deploys to here: it names
// that binding as a stack where it has none (--stack, or the default name),
// then adopts that folder alone, as `ronja sync adopt` does. Nothing marks a
// cloned folder apart from an initialized one, so the rule reads only the
// file. nil when there is nothing to set up; a folder it cannot set up is
// refused naming the one command that would.
//
// flagStack names the stack only while it sets up: the rest of propose plans
// as a later run does, with the folder's stack read from ronja.json, so a
// migrations folder bound without --stack is in the first list too.
//
// With --take-over it also runs on an adopted lone folder, so a propose that
// was refused claimed_elsewhere takes the row over when run again with the
// flag; the claim step then carries the takeover (syncAdoptFolder).
func setUpLoneFolder(ctx context.Context, base string, found []discoveredFolder, resolved *config.Resolved) (*proposeSetup, error) {
	if flagTakeOver && len(found) == 1 && found[0].notChecked() {
		return nil, fmt.Errorf("nothing was proposed: --take-over takes over the row of one folder, and %s could not be read (%s): %s",
			filepath.Join(base, found[0].Rel), found[0].Reason, found[0].Detail)
	}
	if flagTakeOver && len(found) != 1 {
		return nil, fmt.Errorf("nothing was proposed: --take-over takes over the row of one folder, and %s holds %d. Propose that folder on its own with --take-over", base, len(found))
	}
	if len(found) != 1 || found[0].notChecked() || (found[0].Manifest.Adopted() && !flagTakeOver) {
		return nil, nil
	}
	savedStack := flagStack
	defer func() { flagStack = savedStack }()
	folder := found[0]
	adopted := folder.Manifest.Adopted()
	lockBefore, _ := os.ReadFile(wfdir.LockPath(folder.Root))
	refuse := func(format string, args ...any) error {
		if adopted {
			return fmt.Errorf("nothing was proposed: %s", fmt.Sprintf(format, args...))
		}
		return fmt.Errorf("nothing was proposed: %s is not adopted, so Ronja does not keep its state and a deploy of a copy could create its resources twice; %s", folder.Rel, fmt.Sprintf(format, args...))
	}
	f, err := openFolderToPlanAt(ctx, folder.Root, resolved, folder.Kind)
	if err != nil {
		return nil, refuse("propose adopts such a folder first, and could not read its binding here: %v", err)
	}
	featureID := f.Binding.FeatureID
	if !f.Bound || featureID == "" {
		name := f.Stack
		if name != "" {
			name += " "
		}
		return nil, refuse("propose adopts such a folder first, and its %s names no feature on %s to adopt it in. Run `ronja sync stack add %s--feature <feature-id>` there (`ronja api /api/v2/feature/query` lists them), then propose again",
			wfdir.ManifestName, describeTarget(resolved), name)
	}
	l := newSpaceLookup(ctx, resolved)
	if f.Stack == "" {
		name, err := defaultStackName(l, featureID, f.Manifest.Stacks)
		if err != nil {
			return nil, refuse("propose adopts such a folder first, and could not name its stack: %v", err)
		}
		flagStack = name
		if f, err = openFolderToPlanAt(ctx, folder.Root, resolved, folder.Kind); err != nil {
			return nil, refuse("propose adopts such a folder first, and could not name its stack %q: %v", name, err)
		}
	}
	flagStack = f.Stack
	if flagTakeOver {
		if err := takeOverPrecheck(f); err != nil {
			return nil, refuse("%v", err)
		}
	}
	setup := &proposeSetup{Folder: folder.Rel, Stack: f.Stack, FeatureID: featureID, adopted: adopted}
	if f.adoptPending {
		space, err := featureSpace(l, featureID)
		if err != nil {
			return nil, refuse("%v", declareSpaceFailed(featureID, f.Stack, err))
		}
		stampSpace(f.Manifest, f.Stack, recordedSpace(space))
		if err := f.saveFolder(); err != nil {
			return nil, refuse("record stack %q in %s: %v", f.Stack, wfdir.ManifestName, err)
		}
		f.adoptPending = false
		setup.StackAdded = true
	}

	folder.Manifest = f.Manifest
	key := wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID}
	if reason, detail := resolveStackForFolder(folder.Manifest, f.Lock, key, f.Stack); reason != "" {
		return nil, refuse("propose could not adopt it on stack %q (%s): %s", f.Stack, reason, detail)
	}
	line := syncManagedFolderReport{Path: folder.Rel, Root: folder.Root, Kind: folder.Kind.Name, Units: []syncManagedUnitView{}}
	syncAdoptFolder(withAddressWalkRoot(ctx, base), folder, resolved, &line)
	if line.Verdict != verdictApplied {
		added := ""
		switch {
		case line.Minted && setup.StackAdded:
			added = fmt.Sprintf(" (%s now names stack %q and carries a folder id: commit it)", wfdir.ManifestName, f.Stack)
		case line.Minted:
			added = fmt.Sprintf(" (%s now carries a folder id: commit it)", wfdir.ManifestName)
		case setup.StackAdded:
			added = fmt.Sprintf(" (%s now names stack %q: commit it)", wfdir.ManifestName, f.Stack)
		}
		next := fmt.Sprintf("Once that is fixed, `ronja sync adopt --stack %s` there adopts it, or propose again", f.Stack)
		switch {
		case flagTakeOver:
			next = "Nothing was taken over"
		case line.Reason == syncReasonClaimedElsewhere && takesOver(f.Kind):
			next = fmt.Sprintf("If this folder is to continue that %s (a fresh clone of it, its old folder gone or abandoned), propose again with --take-over: it releases that folder's claim and claims the %s for this one, when that folder is not in a git repository and this one is based on the live version. That folder's next deploy is then refused", f.Kind.Label, f.Kind.Label)
		}
		return nil, refuse("propose could not adopt it on stack %q (%s): %s%s. %s",
			f.Stack, line.Reason, line.Detail, added, next)
	}
	setup.FolderID, setup.Claimed = line.FolderID, len(line.Units)
	for _, u := range line.Units {
		if u.TakenFrom != "" {
			setup.TookOverFrom = append(setup.TookOverFrom, u.TakenFrom)
		}
	}
	if adopted {
		if len(setup.TookOverFrom) == 0 {
			return nil, nil
		}
		setup.Claimed = len(setup.TookOverFrom)
	}
	lockAfter, _ := os.ReadFile(wfdir.LockPath(folder.Root))
	setup.lockChanged = !bytes.Equal(lockBefore, lockAfter)
	return setup, nil
}

// takesOver reports a kind whose folder holds one row, which --take-over can
// carry.
func takesOver(kind wfdir.Kind) bool {
	switch kind.Name {
	case wfdir.KindWorkflow, wfdir.KindDataApp, wfdir.KindModule:
		return true
	}
	return false
}

// takeOverPrecheck refuses a --take-over the server would refuse for want of
// what only the folder knows, before anything is written: the kind, and the
// live version the folder is based on, which the server compares with the
// row's.
func takeOverPrecheck(f *folder) error {
	if !takesOver(f.Kind) {
		return fmt.Errorf("--take-over is for an app, workflow or module folder: a %s folder holds other rows a takeover cannot carry", f.Kind.Label)
	}
	if f.headVersion() != "" {
		return nil
	}
	id, _ := f.Binding.ResourceID(f.Kind)
	return fmt.Errorf("--take-over needs the version of the live %s this folder is based on, and %s records none for stack %q (it was cloned without --stack, or from your open draft). Clone the live %s again with `%s clone %s <dir> --stack %s`, apply your change there and propose that folder with --take-over",
		f.Kind.Label, wfdir.LockName, f.Stack, f.Kind.Label, f.Kind.Command, orUnknown(id), f.Stack)
}

func printProposeSetup(out io.Writer, s *proposeSetup) {
	folder := s.Folder
	if folder == "." {
		folder = "this folder"
	}
	if s.adopted {
		for _, from := range s.TookOverFrom {
			fmt.Fprintf(out, "Took the row over for %s (folder id %s) from %s, whose next deploy is now refused.\n", folder, s.FolderID, from)
		}
		return
	}
	fmt.Fprintf(out, "Set up %s for change lists first, once:\n", folder)
	if s.StackAdded {
		fmt.Fprintf(out, "  1. added stack %q (feature %s) to %s\n", s.Stack, s.FeatureID, wfdir.ManifestName)
	} else {
		fmt.Fprintf(out, "  1. stack %q (feature %s) was already in %s\n", s.Stack, s.FeatureID, wfdir.ManifestName)
	}
	fmt.Fprintf(out, "  2. adopted it on stack %q: folder id %s written to %s, %d %s claimed in Ronja\n",
		s.Stack, s.FolderID, wfdir.ManifestName, s.Claimed, plural(s.Claimed, "row"))
	for _, from := range s.TookOverFrom {
		fmt.Fprintf(out, "     taken over from %s, whose next deploy is now refused\n", from)
	}
	if s.lockChanged {
		fmt.Fprintf(out, "%s and %s changed: commit them (until then they are uncommitted edits).\n", wfdir.ManifestName, wfdir.LockName)
		return
	}
	fmt.Fprintf(out, "%s changed: commit it (until then it is an uncommitted edit).\n", wfdir.ManifestName)
}
