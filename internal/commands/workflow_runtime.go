package commands

import "github.com/ronjatech/ronja-cli/internal/wfdir"

// runtimeInfo names a runtime: the short parenthetical label, and the one line a
// report says about it when it is stamped. Both empty for a runtime with nothing
// to say, so a future 4 degrades to silence rather than to a lie.
//
// One function rather than one per print site, because five sites each spelling
// a runtime's meaning inline is what let a runtime-3 push print runtime 2's
// sentence and call runtime 3 "(Durable)" full stop. Every place the CLI puts a
// runtime's NAME on screen reads from here.
//
// Note that runtime 3's label still says "Durable": v3 IS durable, and an author
// upgrading 1 -> 3 has to learn that their steps just became journaled. Tests on
// these strings therefore assert the exact rendered line — asserting that the
// word "Durable" is ABSENT would assert nothing.
func runtimeInfo(n int) (label, sentence string) {
	switch n {
	case wfdir.RuntimeDurable:
		return "Durable", "steps are journaled; a failed run resumes with `ronja wf test --resume`"
	case wfdir.RuntimeQuery:
		return "Durable, tables via tools.query",
			"steps are journaled; tables are read only through `tools.query` — the container holds no table credential"
	default:
		// Runtime 1 has nothing to say (it is what a workflow is unless somebody
		// chose otherwise), and a runtime this CLI has never heard of has nothing
		// TRUE to say. Both answer with silence.
		return "", ""
	}
}
