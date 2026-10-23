package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// runtimeInfo names a runtime: the short parenthetical label, and the one line a
// report says about it when it is stamped. Both empty for a runtime this CLI has
// nothing true to say about — 1, and 4, whose meaning is the instance's — so it
// degrades to silence rather than to a lie. Every place the CLI puts a runtime's
// NAME on screen reads from here.
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
		return "", ""
	}
}

// runtimeFrozenCode is the wire code an instance's refusal carries when a
// folder's runtime no longer takes code changes. Keyed on the code, never on the
// sentence: the server rewords prose, and a hint matched on prose would point a
// different refusal at the wrong way out.
const runtimeFrozenCode = "runtime_frozen"

// runtimeRefusalHint appends the CLI's way out (runtimeHintFor) to an
// instance's refusal of a workflow runtime, and returns every other error
// unchanged.
//
// Applied once, at the root (run), so every single-folder door a refusal can
// come through — push, test, publish — gets the same words. `sync apply` does
// not reach it with a folder's error: it reports each folder in its own line,
// so runPlannedFolder puts the same hint in that line's Notes instead.
func runtimeRefusalHint(err error) error {
	if hint := runtimeHintFor(err); hint != "" {
		return fmt.Errorf("%w\n  %s", err, hint)
	}
	return err
}

// runtimeHintFor is the way out of an instance's refusal of a workflow
// runtime, or "" for any other error.
//
// Two refusals get one. A DRIFT refusal — the instance does not take the runtime
// this CLI sent ("invalid runtimeVersion"), or the row has moved past the
// folder ("runtimeVersion is a one-way upgrade") — means the CLI and the
// instance disagree about runtimes, and which of the two is behind decides the
// fix, so the hint names both. Matched on the server's sentence because those
// refusals carry no wire code; that is safe only because the match decides
// nothing but a hint, and a reworded refusal loses its hint and still says what
// it said. A FROZEN refusal (wire code runtime_frozen) — the folder's runtime no
// longer takes code changes — says how a FOLDER raises its runtime instead,
// which is the manifest key a push sends as an upgrade, not a flag.
func runtimeHintFor(err error) string {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status < 400 || apiErr.Status > 499 {
		return ""
	}
	if api.WireCodeOf(err) == runtimeFrozenCode {
		return fmt.Sprintf("to raise this folder, set \"runtime\" in %s to a runtime the instance creates on (GET /api/v2/workflow/defaults names the default) and push again — the push upgrades the workflow, and the instance says what the code must change first. If that runtime is newer than this CLI knows (%s), run `ronja update` first",
			wfdir.ManifestName, wfdir.JoinInts(wfdir.Runtimes))
	}
	message := strings.ToLower(apiErr.Code + " " + apiErr.Body)
	if !strings.Contains(message, "invalid runtimeversion") &&
		!strings.Contains(message, "runtimeversion is a one-way upgrade") {
		return ""
	}
	return fmt.Sprintf("this CLI knows workflow runtimes %s; if the instance names a newer one, run `ronja update`; if it names an older one, set \"runtime\" in %s to one it lists",
		wfdir.JoinInts(wfdir.Runtimes), wfdir.ManifestName)
}

// createRuntimeFor is the runtime a CREATE of f happens on: the one the folder
// names, else the one the instance creates new workflows on, read from GET
// /workflow/defaults.
//
// Every door that rehearses or makes a create asks here — push, which pins the
// answer, and `wf validate` and `sync check`, which only validate against it — so
// the runtime a folder is checked against is the runtime it is created on.
// Asking the instance rather than assuming a compiled-in default is the point:
// that default moves, and a folder validated against one runtime and created on
// another is checked against rules its workflow does not run under.
//
// Every failure is loud: an instance that predates the route, a read that
// failed (wrapped, so a 5xx stays an unanswered request), and a default this
// build does not know — pinning that would write a folder LoadManifest refuses.
func createRuntimeFor(ctx context.Context, client *api.Client, f *folder) (int, error) {
	if f.Manifest.Runtime != 0 {
		return f.Manifest.Runtime, nil
	}
	var defaults *api.WorkflowDefaults
	var err error
	if f.Manifest.IsJS() {
		defaults, err = jsWorkflowDefaults(ctx, client, f.Resolved, 0)
	} else {
		defaults, err = workflowDefaults(ctx, client, f.Resolved, 0, "")
	}
	if err != nil {
		return 0, err
	}
	if !wfdir.CreatableRuntime(defaults.RuntimeVersion) {
		return 0, unknownDefaultRuntime(f.Resolved, defaults.RuntimeVersion,
			fmt.Sprintf("set \"runtime\" in %s to one it knows", wfdir.ManifestPath(f.Root)))
	}
	return defaults.RuntimeVersion, nil
}

// unknownDefaultRuntime refuses an instance default this build cannot create
// on. advice is the caller's other way out.
func unknownDefaultRuntime(resolved *config.Resolved, runtime int, advice string) error {
	return fmt.Errorf("%s creates new workflows on runtime %d, which this CLI does not know — run `ronja update`, or %s",
		describeTarget(resolved), runtime, advice)
}

// requireLanguageSupport refuses to create — or rehearse the create of — a js
// folder on an instance that does not create JavaScript workflows. Such an
// instance ignores the create body's language and would make a Python workflow
// out of JavaScript source, reporting success.
func requireLanguageSupport(ctx context.Context, client *api.Client, f *folder) error {
	if !f.Manifest.IsJS() {
		return nil
	}
	_, err := jsWorkflowDefaults(ctx, client, f.Resolved, f.Manifest.Runtime)
	return err
}

// jsWorkflowDefaults is workflowDefaults for a JavaScript workflow, refusing an
// answer that does not say "language": "js": an instance that predates
// JavaScript workflows ignores ?language and answers its Python defaults.
func jsWorkflowDefaults(ctx context.Context, client *api.Client, resolved *config.Resolved, runtime int) (*api.WorkflowDefaults, error) {
	defaults, err := workflowDefaults(ctx, client, resolved, runtime, wfdir.LanguageJS)
	if err != nil {
		return nil, err
	}
	if defaults.Language != wfdir.LanguageJS {
		return nil, fmt.Errorf("%s does not create JavaScript workflows — asked for one, it answered without \"language\": %q. Update the instance, or write the workflow in Python",
			describeTarget(resolved), wfdir.LanguageJS)
	}
	return defaults, nil
}

// workflowDefaults reads GET /workflow/defaults for runtime (0: the instance's
// default) and language ("": python) once per invocation, instance and
// credential: a tree of folders asks once, not once per folder. Only an answer
// is kept; a failure is asked again.
func workflowDefaults(ctx context.Context, client *api.Client, resolved *config.Resolved, runtime int, language string) (*api.WorkflowDefaults, error) {
	key := defaultsKey{url: resolved.URL, token: resolved.Token, runtime: runtime, language: language}
	defaultsCache.Lock()
	cached, ok := defaultsCache.answers[key]
	defaultsCache.Unlock()
	if ok {
		return cached, nil
	}
	defaults, err := client.GetWorkflowDefaults(ctx, runtime, language)
	if err != nil {
		if predatesDefaultsRoute(err) {
			return nil, fmt.Errorf("%s has no GET /api/v2/workflow/defaults, so it predates this CLI — update the instance or use an older ronja (%w)",
				describeTarget(resolved), err)
		}
		if runtime != 0 {
			return nil, fmt.Errorf("ask %s for a new runtime-%d workflow: %w", describeTarget(resolved), runtime, err)
		}
		return nil, fmt.Errorf("ask %s which runtime a new workflow starts on: %w", describeTarget(resolved), err)
	}
	defaultsCache.Lock()
	if defaultsCache.answers == nil {
		defaultsCache.answers = map[defaultsKey]*api.WorkflowDefaults{}
	}
	defaultsCache.answers[key] = defaults
	defaultsCache.Unlock()
	return defaults, nil
}

// predatesDefaultsRoute reports an instance without GET /workflow/defaults,
// where the path reaches GET /workflow/:id with the id "defaults": a 404, or on
// older backends a 400 "no rows".
func predatesDefaultsRoute(err error) bool {
	switch api.StatusOf(err) {
	case http.StatusNotFound:
		return true
	case http.StatusBadRequest:
		return strings.EqualFold(strings.TrimSpace(api.CodeOf(err)), "no rows")
	}
	return false
}

type defaultsKey struct {
	url, token, language string
	runtime              int
}

// defaultsCache is workflowDefaults' memory. Package-level because the folders
// of one tree each build their own client; emptied by newRootCmd, so it lives
// exactly as long as one invocation (and one test's runCLI).
var defaultsCache struct {
	sync.Mutex
	answers map[defaultsKey]*api.WorkflowDefaults
}

// forgetWorkflowDefaults empties defaultsCache. See there.
func forgetWorkflowDefaults() {
	defaultsCache.Lock()
	defaultsCache.answers = nil
	defaultsCache.Unlock()
}

// runtimePinnedNote is what a push says when it pinned a folder that named no
// runtime: the folder now says something its author never wrote.
func runtimePinnedNote(runtime int) string {
	return fmt.Sprintf("this folder named no runtime; pinned to the instance's default %d", runtime)
}
