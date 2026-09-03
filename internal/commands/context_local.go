package commands

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// Noticing that the caller is standing in a workflow folder, and saying so.
//
// This exists because of a failure mode that is entirely ours. `ronja wf`
// already does the loop people hand-roll over raw HTTP — it edits from a
// folder, syncs a draft, runs it, polls to completion and prints the log — and
// it manages the parameter declaration too. But nothing pointed at it. Someone
// who read the HTTP guide first ended up escaping Python source into a JSON PUT
// body on every edit, and hand-writing a poll loop four times, with the command
// that does both sitting one word away.
//
// A capability nobody finds is a capability that does not exist, and the cost
// of the fix is one line of output at the moment it is relevant: `ronja
// context` is the handoff command, so it is where a reader is deciding what to
// do next.

// localWorkflow describes what the current directory looks like it holds.
type localWorkflow struct {
	// Root is the folder holding ronja.json, when there is one.
	Root string
	// Title comes from the manifest, so the line can name the workflow.
	Title string
	// Bound reports whether the manifest records any instance binding yet — the
	// difference between "cloned, keep editing" and "initialised, never pushed".
	Bound bool
	// LoosePython is set when there is no manifest but the directory holds
	// Python. That is the `wf init` case, and the one where somebody is about
	// to start doing it by hand.
	LoosePython string
	// Kind is which folder loop this is — wfdir.KindWorkflow, KindDataApp or
	// KindPipeline — or "" when there is a ronja.json we could not read as any
	// of them.
	//
	// Load-bearing rather than cosmetic: the folder types are indistinguishable
	// by shape, and pointing a pipeline folder at `wf` would name commands that
	// LoadManifest's kind check then refuses.
	Kind string
}

// localWork inspects the working directory. Nil means there is nothing worth
// saying, which is the common case and must stay silent — a context command
// that editorialises about every directory is one people stop reading.
func localWork() *localWorkflow {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	if root, err := wfdir.FindRoot(cwd); err == nil {
		found := &localWorkflow{Root: root}
		// FindRoot only knows there is a ronja.json; which loop it belongs to
		// is the manifest's `kind`, and LoadManifest refuses every other one. Ask
		// for each in turn rather than parsing the file ourselves, so this
		// stays honest if the refusal rules change.
		//
		// The set comes from wfdir's REGISTRY, never a literal list. It was a
		// literal list of three, and the fourth kind was added without it: an
		// automation folder then left Kind empty, and printLocalWork's fallback
		// announced "this is a workflow folder" and recommended `ronja wf` —
		// exactly the misrouting Kind.Command exists to prevent, in the one
		// message a lost caller reads.
		for _, k := range wfdir.AllKinds() {
			if m, err := wfdir.LoadManifest(root, k); err == nil {
				found.Kind = k.Name
				found.Title = m.Title
				// Either shape counts: a stack folder's bindings live in
				// "stacks" and a legacy one's in "instances", and a folder that
				// has migrated is no less bound for it.
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

// firstPythonFile returns one .py file in dir, or "".
//
// Deliberately shallow — no recursion, no heuristics about what the file
// contains. The signal is "somebody is writing Python here", and a directory
// listing is enough to establish it. Guessing harder would produce the one
// outcome worse than staying quiet: confidently telling a data scientist that
// their unrelated notebook scratch folder is a Ronja workflow.
func firstPythonFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	// main.py first if it is there, since that is the entrypoint `wf init`
	// would pick and naming it makes the suggestion concrete.
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
		// Which loop this folder belongs to, which the payload used to omit
		// entirely — so a program reading this could see that there was a folder
		// but not which commands drive it, the one thing the note exists to say.
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

// printLocalWork appends the folder note to `ronja context`.
//
// Placed at the very end, after the inlined /llms.txt, on purpose: llms.txt is
// long, and the last thing on the page is the thing a reader acts on.
func printLocalWork(l *localWorkflow) {
	if l == nil {
		return
	}
	out := os.Stdout
	fmt.Fprintf(out, "\n---\n\n## In this directory\n\n")

	// The noun comes from the registry's Label, so a kind cannot be added and
	// then described as a workflow. An UNREADABLE manifest (Kind "") keeps the
	// old fallback: there is nothing better to call it, and the verb list below
	// is the same one `wf init` would have offered.
	noun := "workflow folder"
	if kind, ok := wfdir.KindByName(l.Kind); ok {
		noun = kind.Label + " folder"
	}

	switch {
	case l.Root != "" && l.Title != "":
		fmt.Fprintf(out, "This is the %s for %q (%s).\n\n", noun, l.Title, l.Root)
	case l.Root != "":
		// "a automation folder" — the article has to follow the Label now that
		// it comes from the registry rather than from a literal beside it.
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
		// The two things the raw-HTTP path makes people do by hand: the draft-id
		// bookkeeping per edit cycle, and the poll-plus-truth-table that decides
		// whether a build actually worked.
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
		// The two things worth knowing before the first edit, and neither is
		// obvious from the verb list: there is no publish (nothing to commit),
		// and a field a file leaves out is a field this folder does not manage.
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
		// The data-app equivalents of the two hand-rolled steps below: the
		// allowlist is the one nobody expects, because nothing infers it from
		// the code and an empty one publishes green.
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
		// A kind the registry knows and this note has no verb list for yet.
		// Saying nothing more is the honest answer, and it is the whole reason
		// this branch exists: falling through to the `ronja wf` list below would
		// hand the reader the commands for a DIFFERENT loop, which LoadManifest
		// then refuses — the misrouting Kind.Command was introduced to stop,
		// delivered in the one message a lost caller reads.
		return
	}

	fmt.Fprintf(out, "    ronja wf status      local changes, remote state, and drift\n")
	fmt.Fprintf(out, "    ronja wf validate    check it server-side, save nothing\n")
	fmt.Fprintf(out, "    ronja wf push        sync the folder into your draft\n")
	fmt.Fprintf(out, "    ronja wf test        run the draft and report what happened\n")
	fmt.Fprintf(out, "    ronja wf publish     take the draft live\n")
	fmt.Fprintf(out, "    ronja wf run         run the live workflow and report what happened\n\n")
	// The two things the raw-HTTP path makes people do by hand, named
	// explicitly — they are the reason to prefer these commands, and neither is
	// obvious from the verb list.
	fmt.Fprintf(out, "`wf push` syncs the files AND the parameter declaration in ronja.json, and\n")
	fmt.Fprintf(out, "`wf test` polls the run to completion and prints the log — so neither needs\n")
	fmt.Fprintf(out, "a hand-written JSON body or a poll loop.\n")
	if !l.Bound {
		fmt.Fprintf(out, "\nNot pushed to this instance yet; the first `wf push` records the binding.\n")
	}
}
