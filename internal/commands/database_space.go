package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// checkDatabaseSpace refuses a database outside the Space that --stack's
// stack declares, on this instance and organization (key), in the
// repository's folders (bind_space_mismatch), so a Dev migration never runs on
// Main's database. A stack no folder declares a Space for is not checked; a
// repository that cannot be read, a migrations folder in none, and a stack two
// folders declare for two Spaces refuse. verb ("applied" or "proposed") is
// what the refusals say did not happen; offerUnchecked adds dropping --stack
// as a way out, which only push can offer: propose without --stack would also
// plan its folders and pick its bindings for another stack.
func checkDatabaseSpace(ctx context.Context, client *api.Client, resolved *config.Resolved, key wfdir.InstanceKey, dir, databaseID, verb string, offerUnchecked bool) error {
	if flagStack == "" {
		return nil
	}
	unchecked := ""
	if offerUnchecked {
		unchecked = ", or pass --database <id> without --stack, which applies with no Space check"
	}
	repo, described := addressBoundary(ctx, dir)
	if _, err := os.Lstat(filepath.Join(repo, ".git")); err != nil {
		return withExitCode(syncExitUnknown, fmt.Errorf("the migrations folder is not inside a git work tree, so no stack's Space could be read; run it from the repository%s. Nothing was %s", unchecked, verb))
	}
	found, err := discoverFolders(repo)
	if err != nil {
		return err
	}
	type declaring struct {
		stack wfdir.Stack
		root  string
	}
	var stacks []declaring
	for _, folder := range found {
		m, err := manifestForSpaceScan(folder, unchecked, verb)
		if err != nil {
			return err
		}
		if st := m.Stacks[flagStack]; st.SpaceID != "" {
			stacks = append(stacks, declaring{stack: st, root: folder.Root})
		}
	}
	if len(stacks) == 0 {
		return nil
	}
	if !key.Known() {
		if err := ensureTenant(ctx, resolved); err != nil {
			return withExitCode(syncExitUnknown, fmt.Errorf("stack %q declares a Space in %s, and which organization this credential is in could not be read to match it: %w. Nothing was %s", flagStack, described, err, verb))
		}
		key.TenantID = resolved.TenantID
	}
	declared := map[string]string{}
	folders := map[string][]string{}
	var spaces []string
	for _, d := range stacks {
		st := d.stack
		if !wfdir.Matches(st.Key(), key) {
			continue
		}
		if _, seen := declared[st.SpaceID]; !seen {
			spaces = append(spaces, st.SpaceID)
		}
		declared[st.SpaceID] = spaceLabel(st)
		rel, err := filepath.Rel(repo, d.root)
		if err != nil {
			rel = d.root
		}
		folders[st.SpaceID] = append(folders[st.SpaceID], filepath.ToSlash(rel))
	}
	if len(declared) == 0 {
		return nil
	}
	db, err := client.GetDatabase(ctx, databaseID)
	if err != nil {
		return withExitCode(syncExitUnknown, fmt.Errorf("check which Space database %s is in, since stack %q declares a Space in %s: %w — nothing was %s", databaseID, flagStack, described, err, verb))
	}
	if db.SpaceID == "" {
		fmt.Fprintf(os.Stderr, "  Note: %s does not say which Space database %s is in, so it was not checked against stack %q's Space.\n", client.BaseURL, databaseID, flagStack)
		return nil
	}
	if _, ok := declared[db.SpaceID]; ok && len(declared) == 1 {
		return nil
	}
	l := newSpaceLookupOn(ctx, client)
	labels := make([]string, 0, len(spaces))
	for _, id := range spaces {
		label := "Space " + declared[id]
		if len(spaces) > 1 {
			label += " (" + strings.Join(folders[id], ", ") + ")"
		}
		labels = append(labels, label)
	}
	refusal := &managedRefusal{Reason: syncReasonBindSpaceMismatch, Detail: fmt.Sprintf(
		"stack %q is declared for %s in %s, and database %s (%s) is in %s. Pass --database with a database in the stack's Space; nothing was %s",
		flagStack, strings.Join(labels, " and "), described, orUnknown(db.Name), databaseID, l.describeSpace(db.SpaceID), verb)}
	return fmt.Errorf("refused (%s): %w", refusal.Reason, refusal)
}

// manifestForSpaceScan is a discovered folder's manifest, read here when
// discovery set it aside unread (a nested root, a copied folder id). A
// manifest that cannot be read refuses: it could declare the stack's Space.
func manifestForSpaceScan(folder discoveredFolder, unchecked, verb string) (*wfdir.Manifest, error) {
	if folder.Manifest != nil {
		return folder.Manifest, nil
	}
	unreadable := func(detail string) error {
		return withExitCode(syncExitUnknown, fmt.Errorf("cannot read %s, so whether it declares stack %q's Space is unknown: %s. Fix it%s. Nothing was %s",
			wfdir.ManifestPath(folder.Root), flagStack, detail, unchecked, verb))
	}
	if folder.Reason == syncReasonUnreadable {
		return nil, unreadable(folder.Detail)
	}
	kind, known, err := folderKindAt(folder.Root)
	if err != nil {
		return nil, unreadable(err.Error())
	}
	if !known {
		return nil, unreadable("it declares no kind this build understands")
	}
	m, err := wfdir.LoadManifest(folder.Root, kind)
	if err != nil {
		return nil, unreadable(err.Error())
	}
	return m, nil
}
