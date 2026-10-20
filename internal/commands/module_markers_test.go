package commands

import "testing"

// The port's fidelity test: the cases are the server's own
// (backend/engine/pymarkers/module_test.go).
func TestFirstRonjaMarkerVerb(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{"clean pure-Python module file",
			"def total(rows):\n    return sum(r['qty'] for r in rows)\n", ""},
		{"ref", `rows = tools.query("{{ ref('table-x') }}")`, "ref"},
		{"write", `tools.appendToTable(buf, {{ write('table-x') }})`, "write"},
		{"secret two-arg", `k = {{ secret('secret-x', 'token') }}`, "secret"},
		{"secret one-arg", `tools.queryExternal({{ secret('secret-x') }}, "postgres", q)`, "secret"},
		{"agent", `tools.run_agent({{ agent('agent-x') }})`, "agent"},
		{"file", `p = {{ file('reports/', 'write') }}`, "file"},
		{"codex", `tools.searchCodex({{ codex('cdx-x') }}, q)`, "codex"},
		{"mailbox", `tools.sendEmail(mailbox={{ mailbox('mailbox-x') }})`, "mailbox"},
		{"workflow", `tools.runWorkflow({{ workflow('workflow-x') }})`, "workflow"},
		// Modules are LEAVES in V1 — no transitive imports.
		{"module marker in a module file", `from {{ module('module-x') }} import y`, "module"},
		{"marker inside a docstring is not a violation",
			"\"\"\"\nCall it from a workflow that does {{ ref('table-x') }}.\n\"\"\"\ndef f():\n    pass\n", ""},
		{"marker inside a line comment is not a violation",
			"# the consumer passes the handle from {{ secret('secret-x', 'token') }}\ndef f():\n    pass\n", ""},
		{"marker inside a live string literal is a violation", `q = "{{ ref('table-x') }}"`, "ref"},
		{"an unknown verb is not a Ronja marker", `x = {{ format('a') }}`, ""},
		// No quoted argument, no binding, no match, as on the server.
		{"a marker-shaped construct that binds nothing passes", `x = {{ ref() }}`, ""},
		{"first violation wins",
			"a = {{ write('table-x') }}\nb = {{ agent('agent-x') }}\n", "write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstRonjaMarkerVerb(tc.content); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
