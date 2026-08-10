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
	// Kind is which folder loop this is — wfdir.KindWorkflow or KindDataApp,
	// or "" when there is a ronja.json we could not read as either.
	//
	// Load-bearing rather than cosmetic: the two folder types are
	// indistinguishable by shape, and pointing a data-app folder at `wf` would
	// name commands that LoadManifest's kind check then refuses.
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
		// is the manifest's `kind`, and LoadManifest refuses the other one. Ask
		// for each in turn rather than parsing the file ourselves, so this
		// stays honest if the refusal rules change.
		for _, k := range []wfdir.Kind{wfdir.WorkflowKind, wfdir.DataAppKind} {
			if m, err := wfdir.LoadManifest(root, k); err == nil {
				found.Kind = k.Name
				found.Title = m.Title
				found.Bound = len(m.Instances) > 0
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

	noun := "workflow folder"
	if l.Kind == wfdir.KindDataApp {
		noun = "data-app folder"
	}

	switch {
	case l.Root != "" && l.Title != "":
		fmt.Fprintf(out, "This is the %s for %q (%s).\n\n", noun, l.Title, l.Root)
	case l.Root != "":
		fmt.Fprintf(out, "This is a %s (%s).\n\n", noun, l.Root)
	default:
		fmt.Fprintf(out, "There is Python here (%s) but no workflow folder.\n\n", l.LoosePython)
		fmt.Fprintf(out, "    ronja wf init --feature <feature id>    turn it into one\n\n")
		fmt.Fprintf(out, "A workflow folder syncs your local files into a draft, so you never have to\n")
		fmt.Fprintf(out, "escape source into a JSON body to save it.\n")
		return
	}

	if l.Kind == wfdir.KindDataApp {
		fmt.Fprintf(out, "    ronja app status     local changes, remote state, and drift\n")
		fmt.Fprintf(out, "    ronja app validate   compile the draft on the server\n")
		fmt.Fprintf(out, "    ronja app push       sync the folder into your draft\n")
		fmt.Fprintf(out, "    ronja app publish    take the draft live\n\n")
		// The data-app equivalents of the two hand-rolled steps below: the
		// allowlist is the one nobody expects, because nothing infers it from
		// the code and an empty one publishes green.
		fmt.Fprintf(out, "`app push` syncs the files AND the access block in ronja.json — the tables and\n")
		fmt.Fprintf(out, "secrets the app may read, which nothing derives from your source. There is no\n")
		fmt.Fprintf(out, "`app test`: a data app has nothing to run headlessly, so `status` prints its\n")
		fmt.Fprintf(out, "URL instead.\n")
		if !l.Bound {
			fmt.Fprintf(out, "\nNot pushed to this instance yet; the first `app push` creates the app as an\n")
			fmt.Fprintf(out, "unpublished draft only you can see.\n")
		}
		return
	}

	fmt.Fprintf(out, "    ronja wf status      local changes, remote state, and drift\n")
	fmt.Fprintf(out, "    ronja wf validate    check it server-side, save nothing\n")
	fmt.Fprintf(out, "    ronja wf push        sync the folder into your draft\n")
	fmt.Fprintf(out, "    ronja wf test        run the draft and report what happened\n")
	fmt.Fprintf(out, "    ronja wf publish     take the draft live\n\n")
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
