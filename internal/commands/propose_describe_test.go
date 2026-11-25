package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// The rows of proposeFixture's list as the fake keys them.
const fixtureChanges = `  weekly_copy   automation  create
  sales-report  workflow    update
  mdb-1         database    migrate
`

// TestProposePrintsTheTableAndNextStep: a new list is proposed unsent and
// says so, then lists each change under its key and spells out describing it
// as two commands to paste; --json carries the status, the skeleton and the
// server's next step.
func TestProposePrintsTheTableAndNextStep(t *testing.T) {
	f, dir := proposeFixture(t)
	f.changeLists.dropped = []string{"old-report"}
	out, err := runPropose_(t, dir, "--stack", "dev")
	if err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	want := "Proposed change list changelist-1: 1 modified, 1 created, 1 database migration. Not sent yet: nobody has been notified.\n" +
		"  " + fakeListURL + "\n" +
		fixtureChanges +
		"Notes dropped, since their change is no longer in this list: old-report\n" +
		"Describe it: print it to a file, write the title, the description and a note per change, then save it:\n" +
		"ronja propose describe changelist-1 > .change.json\n" +
		"ronja propose describe changelist-1 .change.json\n"
	if i := strings.Index(out, "Proposed change list"); i < 0 || out[i:] != want {
		t.Errorf("propose printed:\n%s\nwant it to end:\n%s", out, want)
	}
	if !f.changeLists.proposals[0].unsent {
		t.Error("propose did not ask for the list unsent")
	}

	out, err = runPropose_(t, dir, "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("propose --json: %v\n%s", err, out)
	}
	var report proposeReport
	decodeJSONInto(t, out, &report)
	if report.Status != changeListUnsent || report.Next != "ronja propose describe changelist-1" || report.Skeleton == nil ||
		len(report.Skeleton.Notes) != 3 || len(report.Changes) != 3 {
		t.Errorf("--json report %+v, want unsent with the skeleton's three keys and the describe step", report)
	}
}

// TestProposeAgainstAnOlderServerSaysSo: a Ronja that predates unsent lists
// sends the list at once. propose prints today's report, says the description
// is not taken, and never claims nobody was notified; --describe sends nothing.
func TestProposeAgainstAnOlderServerSaysSo(t *testing.T) {
	f, dir := proposeFixture(t)
	f.changeLists.olderServer = true
	writeLocal(t, dir, ".change.json", `{"title":"Weekly copy","description":"Why."}`)
	out, err := runPropose_(t, dir, "--stack", "dev", "--describe", ".change.json")
	if err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	wantAll(t, "propose to an older Ronja", out,
		"Proposed change list changelist-1: 1 modified, 1 created, 1 database migration.\n",
		"Nothing changes until",
		"This Ronja takes no description: it sent the list as proposed.",
		"Note: this Ronja takes no description, so .change.json was not saved",
		"To continue once it is decided, run: ronja propose wait changelist-1")
	for _, never := range []string{"Not sent yet", "nobody has been notified", "ronja propose describe"} {
		if strings.Contains(out, never) {
			t.Errorf("propose to an older Ronja says %q:\n%s", never, out)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if strings.HasPrefix(r, "PUT /api/v2/change-list") {
			t.Errorf("propose sent the description to a Ronja that takes none: %s", r)
		}
	}
}

// TestDescribeRoundTrips: describe prints every key with "" to fill in, saves
// the file back and names the next step, and prints what was saved; a key
// the list does not have is refused in the server's words; --describe saves
// the same file in the propose itself.
func TestDescribeRoundTrips(t *testing.T) {
	f, dir := proposeFixture(t)
	if out, err := runPropose_(t, dir, "--stack", "dev"); err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	out, err := runCLI(t, dir, "propose", "describe", "changelist-1")
	if err != nil {
		t.Fatalf("describe: %v\n%s", err, out)
	}
	if want := `{
  "title": "",
  "description": "",
  "notes": {
    "mdb-1": "",
    "sales-report": "",
    "weekly_copy": ""
  }
}
`; out != want {
		t.Fatalf("describe printed:\n%s\nwant:\n%s", out, want)
	}

	var filled struct {
		Title       string            `json:"title"`
		Description string            `json:"description"`
		Notes       map[string]string `json:"notes"`
	}
	decodeJSONInto(t, out, &filled)
	filled.Title = "Weekly sales copy & report"
	filled.Description = "Sales gets the report every Monday.\n\nThe copy runs weekly."
	filled.Notes["sales-report"] = "Adds the week's totals."
	filled.Notes["weekly_copy"] = "Copies the report each Monday at 07:00."
	filled.Notes["mdb-1"] = "Adds a score column to leads."
	raw, _ := json.MarshalIndent(filled, "", "  ")
	writeLocal(t, dir, ".change.json", string(raw))
	out, err = runCLI(t, dir, "propose", "describe", "changelist-1", ".change.json")
	if err != nil {
		t.Fatalf("describe changelist-1 .change.json: %v\n%s", err, out)
	}
	if want := "Saved the description of change list changelist-1.\nNext: ronja propose send changelist-1\n"; out != want {
		t.Errorf("the save printed:\n%s\nwant:\n%s", out, want)
	}
	out, err = runCLI(t, dir, "propose", "describe", "changelist-1")
	if err != nil {
		t.Fatalf("describe again: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"Weekly sales copy & report"`) || !strings.Contains(out, `"Copies the report each Monday at 07:00."`) {
		t.Errorf("describe does not print what was saved, as written:\n%s", out)
	}

	writeLocal(t, dir, "wrong.json", `{"notes":{"no-such-change":"x"}}`)
	_, err = runCLI(t, dir, "propose", "describe", "changelist-1", "wrong.json")
	if err == nil || err.Error() != `this change list has no change called "no-such-change", so nothing was saved` {
		t.Errorf("an unknown key: %v, want the server's refusal as given", err)
	}
	writeLocal(t, dir, "broken.json", `{"title": `)
	if _, err := runCLI(t, dir, "propose", "describe", "changelist-1", "broken.json"); err == nil || !strings.Contains(err.Error(), "broken.json is not a JSON object") {
		t.Errorf("a file that is not JSON: %v, want it refused before it is sent", err)
	}

	out, err = runPropose_(t, dir, "--stack", "dev", "--describe", ".change.json")
	if err != nil {
		t.Fatalf("propose --describe: %v\n%s", err, out)
	}
	if !strings.HasSuffix(out, fixtureChanges+"Next: ronja propose send changelist-1\n") || f.changeLists.list.title != filled.Title {
		t.Errorf("propose --describe did not save the description and name the send:\n%s", out)
	}
	out, err = runPropose_(t, dir, "--stack", "dev", "--describe", ".change.json", "--json")
	if err != nil {
		t.Fatalf("propose --describe --json: %v\n%s", err, out)
	}
	var answered struct {
		Changes []api.ChangeListChange `json:"changes"`
	}
	decodeJSONInto(t, out, &answered)
	for _, c := range answered.Changes {
		if c.Note != filled.Notes[c.Key] {
			t.Errorf("propose --describe --json answers %q's note as %q, want %q, as saved", c.Key, c.Note, filled.Notes[c.Key])
		}
	}
}

// TestDescribeFileNeverShipsWithTheFolder: the printed describe commands,
// followed from inside an app folder and an automation folder, leave
// .change.json there; proposing again packs it in neither, and the automation
// folder does not read it as an automation.
func TestDescribeFileNeverShipsWithTheFolder(t *testing.T) {
	f, dir := proposeFixture(t)
	out, err := runPropose_(t, dir, "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	var first proposeReport
	decodeJSONInto(t, out, &first)
	out, err = runPropose_(t, dir, "--stack", "dev")
	if err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	var printed []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "ronja propose describe ") {
			printed = append(printed, line)
		}
	}
	if len(printed) != 2 || !strings.HasSuffix(printed[0], " > .change.json") {
		t.Fatalf("propose printed %q, want the two describe commands", printed)
	}
	for _, folder := range []string{"apps/sales-dashboard", "automations/sales"} {
		here := filepath.Join(dir, folder)
		skeleton, err := runCLI(t, here, strings.Fields(strings.TrimSuffix(printed[0], " > .change.json"))[1:]...)
		if err != nil {
			t.Fatalf("%s: %s: %v\n%s", folder, printed[0], err, skeleton)
		}
		writeLocal(t, here, ".change.json", skeleton)
		if out, err := runCLI(t, here, strings.Fields(printed[1])[1:]...); err != nil {
			t.Fatalf("%s: %s: %v\n%s", folder, printed[1], err, out)
		}
	}

	out, err = runPropose_(t, dir, "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("propose again: %v\n%s", err, out)
	}
	var again proposeReport
	decodeJSONInto(t, out, &again)
	if again.Counts != first.Counts {
		t.Errorf("proposing again with .change.json in the folders counts %+v, want %+v", again.Counts, first.Counts)
	}
	proposals := f.changeLists.proposals
	for name := range archiveFiles(t, proposals[len(proposals)-1].archive) {
		if strings.HasSuffix(name, ".change.json") {
			t.Errorf("the copy holds %s", name)
		}
	}
}

// TestDescribeReadsStdin: `-` saves the description piped in.
func TestDescribeReadsStdin(t *testing.T) {
	f, dir := proposeFixture(t)
	if out, err := runPropose_(t, dir, "--stack", "dev"); err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	stdinFrom(t, `{"title":"Weekly copy","description":"Copies the report each Monday."}`)
	out, err := runCLI(t, dir, "propose", "describe", "changelist-1", "-")
	if err != nil {
		t.Fatalf("describe -: %v\n%s", err, out)
	}
	if f.changeLists.list.title != "Weekly copy" || !strings.Contains(out, "Next: ronja propose send changelist-1") {
		t.Errorf("describe - saved title %q and printed:\n%s", f.changeLists.list.title, out)
	}
}

// TestSendNamesChangesWithoutANote: a send of an undescribed list prints the
// server's refusal as given, naming the describe commands; once described it
// sends, names the changes with no note without refusing, and names the wait.
func TestSendNamesChangesWithoutANote(t *testing.T) {
	_, dir := proposeFixture(t)
	if out, err := runPropose_(t, dir, "--stack", "dev"); err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	_, err := runCLI(t, dir, "propose", "send", "changelist-1", "--no-browser")
	if want := "change list changelist-1 has no title or no description yet, so it was not sent. " +
		"Print it with `ronja propose describe changelist-1`, write the title, the description and a note per change to `.change.json`, " +
		"save it with `ronja propose describe changelist-1 .change.json`, then send it."; err == nil || err.Error() != want {
		t.Fatalf("send undescribed: %v, want the server's refusal as given:\n%s", err, want)
	}

	writeLocal(t, dir, ".change.json", `{"title":"Weekly copy","description":"Why.","notes":{"sales-report":"Adds totals."}}`)
	if out, err := runCLI(t, dir, "propose", "describe", "changelist-1", ".change.json"); err != nil {
		t.Fatalf("describe: %v\n%s", err, out)
	}
	out, err := runCLI(t, dir, "propose", "send", "changelist-1", "--no-browser")
	if err != nil {
		t.Fatalf("send: %v\n%s", err, out)
	}
	wantAll(t, "send", out,
		"Sent change list changelist-1: dev@example.com was notified in Ronja and has until "+waitTime(time.Date(2026, 11, 25, 9, 30, 0, 0, time.UTC))+" to approve it.\n",
		"  "+fakeListURL+"\n",
		"  No note for mdb-1, weekly_copy: the person sees only their names.\n",
		"Next: ronja propose wait changelist-1   (agents: in the background)\n")
}

// TestWaitOnUnsentExitsAtOnce: an unsent list has nothing to wait for, so
// the wait ends on its first read with the server's next step, before it asks
// whether the list is past its expiry.
func TestWaitOnUnsentExitsAtOnce(t *testing.T) {
	quickWait(t)
	long := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	f := newFakeWaitedList(t, waitedList("unsent", map[string]any{"next": "ronja propose describe changelist-1", "expiresAt": long, "decidedAt": nil}))
	out, stderr, err := runWait(t, "changelist-1")
	wantExit1(t, err, out)
	if want := "Change list changelist-1 is not sent yet: nobody has been asked to approve it, so there is nothing to wait for. Next: ronja propose describe changelist-1\n"; out != want {
		t.Errorf("wait printed:\n%s\nwant:\n%s", out, want)
	}
	if n := len(f.reads()); n != 1 || stderr != "" {
		t.Errorf("%d reads and progress %q, want one read and none", n, stderr)
	}

	out, _, err = runWait(t, "changelist-1", "--json")
	wantExit1(t, err, out)
	var report proposeWaitReport
	decodeJSONInto(t, out, &report)
	if report.Status != changeListUnsent || report.Next != "ronja propose describe changelist-1" {
		t.Errorf("--json %+v, want unsent with the next step", report)
	}
}

// TestDiscardPrintsTheServersRefusal: discard prints the server's refusal
// word for word, with no fallback; otherwise it says an unsent list is
// discarded, and a sent one rejected.
func TestDiscardPrintsTheServersRefusal(t *testing.T) {
	f, dir := proposeFixture(t)
	if out, err := runPropose_(t, dir, "--stack", "dev"); err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	const sent = "this change list was sent; the person rejects it on its card. Propose again to replace it."
	f.changeLists.rejectRefusal = `{"error":"` + sent + `","code":"computer_publish_refused"}`
	if _, err := runCLI(t, dir, "propose", "discard", "changelist-1"); err == nil || err.Error() != sent {
		t.Fatalf("discard refused: %v, want the server's words %q", err, sent)
	}
	if f.changeLists.list.status != changeListUnsent {
		t.Fatalf("a refused discard ended the list: %s", f.changeLists.list.status)
	}
	f.changeLists.rejectRefusal = ""
	out, err := runCLI(t, dir, "propose", "discard", "changelist-1")
	if err != nil || out != "Discarded change list changelist-1.\n" || f.changeLists.list.status != changeListRejected {
		t.Errorf("discard: %v, list %s, printed:\n%s", err, f.changeLists.list.status, out)
	}

	if out, err := runPropose_(t, dir, "--stack", "dev"); err != nil {
		t.Fatalf("propose again: %v\n%s", err, out)
	}
	writeLocal(t, dir, ".change.json", `{"title":"Weekly copy","description":"Why."}`)
	for _, args := range [][]string{{"describe", "changelist-1", ".change.json"}, {"send", "changelist-1", "--no-browser"}} {
		if out, err := runCLI(t, dir, append([]string{"propose"}, args...)...); err != nil {
			t.Fatalf("propose %v: %v\n%s", args, err, out)
		}
	}
	out, err = runCLI(t, dir, "propose", "discard", "changelist-1")
	if err != nil || out != "Rejected change list changelist-1: nothing from it deploys.\n" {
		t.Errorf("discard of a sent list: %v, printed:\n%s", err, out)
	}
}

// TestPrintedStepsCarryTheProfile: when --stack switched propose to the
// profile stored for its organization, every step it prints names that
// profile, so following them reaches the same list; a typed --url is carried
// the same way, and requestPublish is left as the server words it.
func TestPrintedStepsCarryTheProfile(t *testing.T) {
	f, repo := setupFixture(t)
	if out, err := runCLI(t, repo, "app", "init", "new", "--feature", importFeature, "--title", "New"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	dir := filepath.Join(repo, "new")
	if out, err := runPropose_(t, dir); err != nil {
		t.Fatalf("the first propose, which adds stack main: %v\n%s", err, out)
	}
	writeProfiles(t, "other",
		testProfile{Name: "other", URL: "https://other.example.com", TenantID: "ten-other", Token: "tok-other"},
		testProfile{Name: "test", URL: f.URL(), TenantID: testTenantID, Token: "tok-test"})
	t.Setenv("RONJA_URL", "")
	t.Setenv("RONJA_TOKEN", "")

	var out string
	var err error
	stderr := captureStderr(t, func() { out, err = runPropose_(t, dir, "--stack", "main") })
	if err != nil || !strings.Contains(stderr, `Using profile "test", not "other"`) {
		t.Fatalf("propose --stack main under another organization's profile: %v\n%s\n%s", err, stderr, out)
	}
	wantAll(t, "propose", out,
		"ronja propose describe changelist-1 --profile test > .change.json\n",
		"ronja propose describe changelist-1 --profile test .change.json\n")
	skeleton, err := runCLI(t, dir, "propose", "describe", "changelist-1", "--profile", "test")
	if err != nil {
		t.Fatalf("the printed describe: %v\n%s", err, skeleton)
	}
	writeLocal(t, dir, ".change.json", strings.Replace(skeleton, `"title": ""`, `"title": "Weekly copy"`, 1))
	out, err = runCLI(t, dir, "propose", "describe", "changelist-1", ".change.json", "--profile", "test", "--url", f.URL())
	if err != nil {
		t.Fatalf("the printed save: %v\n%s", err, out)
	}
	wantAll(t, "the save", out, "  It still needs a description before it can be sent.\n",
		"ronja propose describe changelist-1 --profile test --url "+f.URL()+" > .change.json\n")

	if got := carryCredential("call requestPublish with changelist-1"); got != "call requestPublish with changelist-1" {
		t.Errorf("requestPublish carried the credential: %q", got)
	}
}

// TestProposeDescribeRefusedStillPrintsTheList: a --describe the server
// refuses after the list was proposed exits 1 with the server's words, and
// the list is still printed with its id, its link and the describe step.
func TestProposeDescribeRefusedStillPrintsTheList(t *testing.T) {
	f, dir := proposeFixture(t)
	writeLocal(t, dir, ".change.json", `{"title":"Weekly copy","notes":{"no-such-change":"x"}}`)
	out, err := runPropose_(t, dir, "--stack", "dev", "--describe", ".change.json")
	if want := `change list changelist-1 was proposed, but its description was not saved: this change list has no change called "no-such-change", so nothing was saved`; err == nil || err.Error() != want {
		t.Fatalf("propose --describe refused: %v, want %q", err, want)
	}
	wantAll(t, "propose", out,
		"Proposed change list changelist-1: 1 modified, 1 created, 1 database migration. Not sent yet: nobody has been notified.\n",
		"  "+fakeListURL+"\n",
		"ronja propose describe changelist-1 > .change.json\n")
	if f.changeLists.list == nil || f.changeLists.list.status != changeListUnsent || f.changeLists.list.title != "" {
		t.Errorf("the list after a refused description: %+v, want it proposed, unsent and undescribed", f.changeLists.list)
	}
}

// TestPrintedKeysAreTerminalSafe: a key, a title or a note the person or
// assistant wrote cannot repaint the terminal or reorder what it shows.
func TestPrintedKeysAreTerminalSafe(t *testing.T) {
	var out strings.Builder
	printChanges(&out, []api.ChangeListChange{{Key: "\x1b[2Jreport\u202e", Kind: "workflow", Action: "update"}})
	printNoNote(&out, &api.ChangeListSkeleton{Title: "t", Description: "d", Notes: map[string]string{"\x1b]0;x\x07copy": ""}})
	if want := "  [2Jreport  workflow  update\n  No note for ]0;xcopy: the person sees only its name.\n"; out.String() != want {
		t.Errorf("printed %q, want %q", out.String(), want)
	}
}

// TestChangeListIDCountsItsArguments: the refusal of extra arguments agrees
// with how many the subcommand takes.
func TestChangeListIDCountsItsArguments(t *testing.T) {
	for _, c := range []struct {
		verb  string
		extra int
		want  string
	}{
		{"send", 0, "ronja propose send takes at most 1 argument, not 2: see ronja propose send --help"},
		{"describe", 1, "ronja propose describe takes at most 2 arguments, not 3: see ronja propose describe --help"},
	} {
		args := []string{"changelist-1", "x", "y"}[:c.extra+2]
		if err := changeListID(c.verb, c.extra)(nil, args); err == nil || err.Error() != c.want {
			t.Errorf("%s with %d arguments: %v, want %q", c.verb, len(args), err, c.want)
		}
	}
}

// stdinFrom makes os.Stdin read body for the rest of the test.
func stdinFrom(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdin
	os.Stdin = file
	t.Cleanup(func() {
		os.Stdin = saved
		file.Close()
	})
}

// TestCarryCredentialReachesQuotedCommands: a next step that is a sentence,
// such as a replacement's that kept the replaced list's description, carries
// the profile into each ronja command it quotes, and leaves the rest alone.
func TestCarryCredentialReachesQuotedCommands(t *testing.T) {
	profile, url := flagProfile, flagURL
	t.Cleanup(func() { flagProfile, flagURL = profile, url })
	flagProfile, flagURL = "test", ""
	for step, want := range map[string]string{
		"ronja propose send changelist-1": "ronja propose send changelist-1 --profile test",
		"Kept the title and description from the replaced list; update them with `ronja propose describe changelist-1` if this change differs, then `ronja propose send changelist-1`": "Kept the title and description from the replaced list; update them with `ronja propose describe changelist-1 --profile test` if this change differs, then `ronja propose send changelist-1 --profile test`",
		"Kept the title; update them with `ronja propose describe changelist-1` if this change differs, then call requestPublish with changelist-1":                                    "Kept the title; update them with `ronja propose describe changelist-1 --profile test` if this change differs, then call requestPublish with changelist-1",
		"call requestPublish with changelist-1": "call requestPublish with changelist-1",
	} {
		if got := carryCredential(step); got != want {
			t.Errorf("carryCredential(%q) = %q, want %q", step, got, want)
		}
	}
}
