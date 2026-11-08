package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// asFreshFolder makes one folder of the imported tree a classic folder that
// has never deployed: no folder id, no lock, no local record. Its next apply
// creates its rows.
func asFreshFolder(t *testing.T, root string) {
	t.Helper()
	m := loadAnyManifest(t, root)
	m.ID = ""
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, wfdir.LockName), filepath.Join(root, ".ronja")} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
}

// --allow-create beside a live app, module or automation of the same name in
// the target feature is refused name_taken, as it is for a workflow.
func TestAllowCreateRefusesToTwinEveryKind(t *testing.T) {
	for _, tc := range []struct{ folder, want string }{
		{"apps/sales-dashboard", "Sales already has an app called Sales dashboard (data_app-dash)"},
		{"modules/sales_helpers", "Sales already has a module called sales_helpers (module-helpers)"},
		{"automations/sales", "Sales already has an automation called"},
	} {
		t.Run(tc.folder, func(t *testing.T) {
			f, dir := gatedTree(t)
			asFreshFolder(t, filepath.Join(dir, tc.folder))
			f.resetRequests()
			report, err := applyStack(t, dir, "main", "--allow-create")
			line := applyFolderLine(t, &report, tc.folder)
			if line.Verdict != verdictRefused || line.Reason != syncReasonNameTaken || !strings.Contains(line.Detail, tc.want) {
				t.Fatalf("apply: %v\n%s / %s — %s", err, line.Verdict, line.Reason, line.Detail)
			}
		})
	}
}

// An inventory that may not list the kind being created, or that cannot be
// read at all, says nothing about twins: the folder is unknown (exit 2), and
// nothing is created.
func TestNameTakenIsUnknownWhenTheInventoryCannotAnswer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(f *fakeImportInstance)
		want  string
	}{
		{"kind omitted", func(f *fakeImportInstance) {
			f.inv.omitted = []api.OmittedRefKind{{Kind: api.RefKindDataApp, Reason: "scope"}}
		}, "may not list the dataapp rows of Sales (scope)"},
		{"read fails", func(f *fakeImportInstance) {
			f.fail["GET /api/v2/feature/"+importFeature+"/references"] = 500
		}, "GET /feature/:id/references"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, dir := gatedTree(t)
			asFreshFolder(t, filepath.Join(dir, "apps/sales-dashboard"))
			tc.stage(f)
			f.resetRequests()
			report, err := applyStack(t, dir, "main", "--allow-create")
			line := applyFolderLine(t, &report, "apps/sales-dashboard")
			if line.Verdict != verdictUnknown || line.Reason != syncReasonLookupFailed || !strings.Contains(line.Detail, tc.want) {
				t.Fatalf("apply: %s / %s — %s", line.Verdict, line.Reason, line.Detail)
			}
			if code := exitCodeOf(err); code != syncExitUnknown {
				t.Fatalf("exit %d, want %d: %v", code, syncExitUnknown, err)
			}
			for _, w := range f.writes() {
				if strings.HasPrefix(w, "POST /api/v2/dataapp") {
					t.Fatalf("created an app anyway: %v", f.writes())
				}
			}
		})
	}
}
