package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

func draftNoLongerFolder(noun, draftID string) error {
	return fmt.Errorf("%s draft %s no longer holds exactly this folder: it was changed after this run pushed it, so nothing was published", noun, draftID)
}

// draftChangedError reports the server's refusal of a publish whose draft moved
// after the check, or nil for any other error. A publish carries the draft
// version read before the check that the draft holds this folder; a later
// write moves it and the server refuses (api.DraftChangedCode).
func draftChangedError(draftID string, err error) error {
	if api.WireCodeOf(err) != api.DraftChangedCode {
		return nil
	}
	return fmt.Errorf("commit %s: %w", draftID, err)
}

// warnUncheckedDraftVersion warns when a publish sent a draft version and the
// answer does not say it was checked: an older Ronja ignores the field, so the
// publish went through without the check rather than being refused.
func warnUncheckedDraftVersion(noun, draftID, sent string, echo api.DraftVersionEcho) {
	if sent != "" && !echo.DraftVersionChecked {
		fmt.Fprintf(os.Stderr, "  Warning: this Ronja does not check draft versions yet; the %s draft %s was published without that check.\n", noun, draftID)
	}
}

func checkWorkflowDraft(ctx context.Context, client *api.Client, f *folder, draftID string) (string, error) {
	row, err := client.GetWorkflow(ctx, draftID)
	if err != nil {
		return "", fmt.Errorf("read draft %s: %w", draftID, err)
	}
	files, err := client.ListWorkflowFiles(ctx, draftID)
	if err != nil {
		return "", fmt.Errorf("read files of %s: %w", draftID, err)
	}
	local, _, err := readLocalFiles(f.Root, f.Kind)
	if err != nil {
		return "", err
	}
	if !workflowRowHoldsFolder(f, row, contentByPath(f.Codec, files), local) {
		return "", draftNoLongerFolder("workflow", draftID)
	}
	return api.DraftVersion(row.UpdatedAt), nil
}

func checkAppDraft(ctx context.Context, client *api.Client, f *folder, draftID string) (string, error) {
	row, err := client.GetDataApp(ctx, draftID)
	if err != nil {
		return "", fmt.Errorf("read draft %s: %w", draftID, err)
	}
	files, err := client.ListDataAppFiles(ctx, draftID)
	if err != nil {
		return "", fmt.Errorf("read files of %s: %w", draftID, err)
	}
	local, _, err := readLocalFiles(f.Root, wfdir.DataAppKind)
	if err != nil {
		return "", err
	}
	if !appRowHoldsFolder(f, row, appContentByPath(f.Codec, files), local) {
		return "", draftNoLongerFolder("app", draftID)
	}
	return api.DraftVersion(row.UpdatedAt), nil
}

func checkModuleDraft(ctx context.Context, client *api.Client, f *folder, draftID string) (string, error) {
	row, err := client.GetModule(ctx, draftID)
	if err != nil {
		return "", fmt.Errorf("read draft %s: %w", draftID, err)
	}
	files, err := client.ListModuleFiles(ctx, draftID)
	if err != nil {
		return "", fmt.Errorf("read files of %s: %w", draftID, err)
	}
	local, _, err := readLocalFiles(f.Root, wfdir.ModuleKind)
	if err != nil {
		return "", err
	}
	if !moduleRowHoldsFolder(f, row, moduleContentByPath(files), local) {
		return "", draftNoLongerFolder("module", draftID)
	}
	return api.DraftVersion(row.UpdatedAt), nil
}
