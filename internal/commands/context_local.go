package commands

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The note `ronja context` prints when the caller stands in a folder a CLI loop
// drives, so the loop is found instead of hand-rolled over raw HTTP.

// localWorkflow describes what the current directory looks like it holds.
type localWorkflow struct {
	// Root is the folder holding ronja.json, when there is one.
	Root  string
	Title string
	// Bound reports whether the manifest records any instance binding yet.
	Bound bool
	// LoosePython is set when there is no manifest but the directory holds
	// Python: the `wf init` case.
	LoosePython string
	// Kind is the folder's wfdir kind name, or "" when ronja.json reads as none
	// of them. The kinds are indistinguishable by shape, so this decides which
	// loop's commands the note names.
	Kind string
}

// localWork inspects the working directory. Nil, the common case, prints
// nothing.
func localWork() *localWorkflow {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	if root, err := wfdir.FindRoot(cwd); err == nil {
		found := &localWorkflow{Root: root}
		// Each kind from wfdir's registry is tried through LoadManifest, never a
		// literal list or a local parse, so a new kind cannot be misrouted to
		// `ronja wf`.
		for _, k := range wfdir.AllKinds() {
			if m, err := wfdir.LoadManifest(root, k); err == nil {
				found.Kind = k.Name
				found.Title = m.Title
				// Either shape counts: stacks, or legacy instances.
				found.Bound = len(m.Instances) > 0 || m.UsesStacks()
				break
			}
		}
		return found
	}
	if name := firstPythonFile(cwd); name != "" {
		return &localWorkflow{LoosePython: name}
	}
	return nil
}

// firstPythonFile returns one .py file in dir, preferring main.py, or an empty
// string. Deliberately shallow: no recursion, no content heuristics.
func firstPythonFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var fallback string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".py" {
			continue
		}
		if e.Name() == "main.py" {
			return e.Name()
		}
		if fallback == "" {
			fallback = e.Name()
		}
	}
	return fallback
}

func (l *localWorkflow) payload() map[string]any {
	out := map[string]any{}
	if l.Root != "" {
		out["root"] = l.Root
		out["bound"] = l.Bound
		if l.Kind != "" {
			out["kind"] = l.Kind
		}
		if l.Title != "" {
			out["title"] = l.Title
		}
	}
	if l.LoosePython != "" {
		out["pythonFile"] = l.LoosePython
	}
	return out
}

// printLocalWork appends the folder note to `ronja context`, last on purpose:
// after the long inlined /llms.txt, the last thing on the page is what a reader
// acts on.
func printLocalWork(l *localWorkflow) {
	if l == nil {
		return
	}
	out := os.Stdout
	fmt.Fprintf(out, "\n---\n\n## In this directory\n\n")

	// An unreadable manifest (Kind "") falls back to "workflow folder".
	noun := "workflow folder"
	if kind, ok := wfdir.KindByName(l.Kind); ok {
		noun = kind.Label + " folder"
	}

	switch {
	case l.Root != "" && l.Title != "":
		fmt.Fprintf(out, "This is the %s for %q (%s).\n\n", noun, l.Title, l.Root)
	case l.Root != "":
		article := "a"
		switch noun[0] {
		case 'a', 'e', 'i', 'o', 'u':
			article = "an"
		}
		fmt.Fprintf(out, "This is %s %s (%s).\n\n", article, noun, l.Root)
	default:
		fmt.Fprintf(out, "There is Python here (%s) but no workflow folder.\n\n", l.LoosePython)
		fmt.Fprintf(out, "    ronja wf init --feature <feature id>    turn it into one\n\n")
		fmt.Fprintf(out, "A workflow folder syncs your local files into a draft, so you never have to\n")
		fmt.Fprintf(out, "escape source into a JSON body to save it.\n")
		return
	}

	if l.Kind == wfdir.KindPipeline {
		fmt.Fprintf(out, "    ronja pipeline status    local changes, table health, and drift\n")
		fmt.Fprintf(out, "    ronja pipeline push      sync each changed .sql file and build it\n")
		fmt.Fprintf(out, "    ronja pipeline publish   commit your drafts\n\n")
		fmt.Fprintf(out, "One .sql file is one derived table; everything else in the folder is ignored.\n")
		fmt.Fprintf(out, "`pipeline push` opens the draft, writes the SQL and its derived inputs, builds\n")
		fmt.Fprintf(out, "it and reports the verdict with a schema diff and sample rows — so none of that\n")
		fmt.Fprintf(out, "needs a hand-written poll loop. Publishing cascades, so downstream tables\n")
		fmt.Fprintf(out, "rebuild on their own.\n")
		if !l.Bound {
			fmt.Fprintf(out, "\nNothing pushed to this instance yet; the first `pipeline push` creates each\n")
			fmt.Fprintf(out, "table and records the binding.\n")
		}
		return
	}

	if l.Kind == wfdir.KindAutomation {
		fmt.Fprintf(out, "    ronja automation status   what a push would change, and what moved\n")
		fmt.Fprintf(out, "    ronja automation push     make each automation match its file\n\n")
		fmt.Fprintf(out, "One .json file is one automation and its filename is the name; everything else\n")
		fmt.Fprintf(out, "in the folder is ignored. There is no publish step — an automation has no draft\n")
		fmt.Fprintf(out, "and no version history, so a push is the whole write. A field a file does not\n")
		fmt.Fprintf(out, "mention is not managed here and is never touched, which is how `enabled` stays\n")
		fmt.Fprintf(out, "under whoever paused it.\n")
		if !l.Bound {
			fmt.Fprintf(out, "\nNothing pushed to this instance yet; the first `automation push` creates each\n")
			fmt.Fprintf(out, "automation and records the binding.\n")
		}
		return
	}

	if l.Kind == wfdir.KindDataApp {
		fmt.Fprintf(out, "    ronja app status     local changes, remote state, and drift\n")
		fmt.Fprintf(out, "    ronja app validate   compile the draft on the server\n")
		fmt.Fprintf(out, "    ronja app push       sync the folder into your draft\n")
		fmt.Fprintf(out, "    ronja app test       render it headlessly and report what was seen\n")
		fmt.Fprintf(out, "    ronja app publish    take the draft live\n\n")
		fmt.Fprintf(out, "`app push` syncs the files AND the access block in ronja.json — the tables and\n")
		fmt.Fprintf(out, "secrets the app may read, which nothing derives from your source. `app test`\n")
		fmt.Fprintf(out, "renders the app in a headless browser and writes a screenshot plus a report of\n")
		fmt.Fprintf(out, "what it saw — perception, not a pass/fail gate, so it exits zero either way.\n")
		if !l.Bound {
			fmt.Fprintf(out, "\nNot pushed to this instance yet; the first `app push` creates the app as an\n")
			fmt.Fprintf(out, "unpublished draft only you can see.\n")
		}
		return
	}

	if l.Kind != "" && l.Kind != wfdir.KindWorkflow {
		// A known kind with no verb list here: say nothing rather than fall
		// through to `ronja wf` verbs that LoadManifest would refuse.
		return
	}

	fmt.Fprintf(out, "    ronja wf status      local changes, remote state, and drift\n")
	fmt.Fprintf(out, "    ronja wf validate    check it server-side, save nothing\n")
	fmt.Fprintf(out, "    ronja wf push        sync the folder into your draft\n")
	fmt.Fprintf(out, "    ronja wf test        run the draft and report what happened\n")
	fmt.Fprintf(out, "    ronja wf publish     take the draft live\n")
	fmt.Fprintf(out, "    ronja wf run         run the live workflow and report what happened\n\n")
	fmt.Fprintf(out, "`wf push` syncs the files AND the parameter declaration in ronja.json, and\n")
	fmt.Fprintf(out, "`wf test` polls the run to completion and prints the log — so neither needs\n")
	fmt.Fprintf(out, "a hand-written JSON body or a poll loop.\n")
	if !l.Bound {
		fmt.Fprintf(out, "\nNot pushed to this instance yet; the first `wf push` records the binding.\n")
	}
}
