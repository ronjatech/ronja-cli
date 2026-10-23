package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// resolveInitLanguage checks --language against the other flags and answers
// the manifest's value: "" for Python, the absent key, so a Python manifest
// stays what it was.
func resolveInitLanguage(language, fromPath string, runtimeNamed bool, runtime int) (string, error) {
	switch language {
	case "", wfdir.LanguagePython:
		if fromPath != "" && wfdir.IsJSSource(fromPath) {
			return "", fmt.Errorf("--from %s is JavaScript — pass --language js to make a JavaScript workflow of it", fromPath)
		}
		return "", nil
	case wfdir.LanguageJS:
		if runtimeNamed && runtime != wfdir.RuntimeMain {
			return "", fmt.Errorf("--language js --runtime %d: a JavaScript workflow runs on runtime %d only — omit --runtime",
				runtime, wfdir.RuntimeMain)
		}
		if fromPath != "" && !wfdir.IsJSSource(fromPath) {
			return "", fmt.Errorf("--language js copies --from in as the entrypoint, and %s is not a .js or .mjs file", fromPath)
		}
		return language, nil
	default:
		return "", fmt.Errorf("--language must be %q or %q (got %q)", wfdir.LanguagePython, wfdir.LanguageJS, language)
	}
}

// initScaffold is what `wf init` writes for a new folder, as the instance
// answered it.
type initScaffold struct {
	runtime     int
	entrypoint  string
	starter     string
	packageJSON string
}

// packageJSONName is the file a JavaScript workflow declares its npm packages in.
const packageJSONName = "package.json"

// initRuntime decides the runtime a new folder is pinned to and the starter
// written for it, by asking the instance: GET /workflow/defaults for the
// runtime --runtime names, else for the instance's default. The instance is the
// one place both are true — a runtime it offers, and an entrypoint written for
// it — so the CLI keeps no copy of either to go stale.
//
// Every failure is init's, before a byte is written (see workflowDefaults): an
// instance that predates the route, a runtime the instance does not create on
// (its 400 names the ones it does, and is shown as it is), a failed read, and
// an answer this build cannot pin — LoadManifest refuses a runtime it does not
// know, so pinning one would write a folder no later command could open. For
// js, also an instance that does not create JavaScript workflows
// (jsWorkflowDefaults), and an entrypoint that is not JavaScript.
func initRuntime(ctx context.Context, resolved *config.Resolved, named int, isNamed bool, language string) (*initScaffold, error) {
	ask := 0
	if isNamed {
		ask = named
	}
	client := newClient(resolved.URL, resolved.Token)
	js := language == wfdir.LanguageJS
	var defaults *api.WorkflowDefaults
	var err error
	if js {
		defaults, err = jsWorkflowDefaults(ctx, client, resolved, ask)
	} else {
		defaults, err = workflowDefaults(ctx, client, resolved, ask, "")
	}
	if err != nil {
		return nil, err
	}
	// Python keeps the CLI's own entrypoint, which is what every instance
	// answered before it named one.
	entrypoint := wfdir.DefaultEntrypoint
	if js {
		entrypoint = wfdir.WorkflowJSKind.DefaultEntrypoint
		if defaults.Entrypoint != "" {
			entrypoint = defaults.Entrypoint
		}
	}
	switch {
	case len(defaults.EntrypointStarter) > maxStarterBytes:
		return nil, fmt.Errorf("%s sent a %d-byte starter %s, over the %d KiB this CLI writes — refusing it; pass --from with the code to start from, or report the instance",
			describeTarget(resolved), len(defaults.EntrypointStarter), entrypoint, maxStarterBytes/1024)
	case len(defaults.PackageJSON) > maxStarterBytes:
		return nil, fmt.Errorf("%s sent a %d-byte starter %s, over the %d KiB this CLI writes — refusing it; report the instance",
			describeTarget(resolved), len(defaults.PackageJSON), packageJSONName, maxStarterBytes/1024)
	case isNamed && defaults.RuntimeVersion != named:
		// An instance that ignored the parameter answers its default, and
		// pinning that would pin a runtime the author did not ask for.
		return nil, fmt.Errorf("asked %s for runtime %d and it answered runtime %d — update the instance, or omit --runtime to take its default",
			describeTarget(resolved), named, defaults.RuntimeVersion)
	case !wfdir.CreatableRuntime(defaults.RuntimeVersion):
		return nil, unknownDefaultRuntime(resolved, defaults.RuntimeVersion, "pass --runtime with one it knows")
	case js && defaults.RuntimeVersion != wfdir.RuntimeMain:
		// LoadManifest refuses a js folder on any other runtime.
		return nil, fmt.Errorf("%s answered runtime %d for a new JavaScript workflow, and this CLI knows JavaScript only on runtime %d — run `ronja update`",
			describeTarget(resolved), defaults.RuntimeVersion, wfdir.RuntimeMain)
	case js && (!wfdir.IsJSSource(entrypoint) || wfdir.ValidatePath(entrypoint) != nil):
		return nil, fmt.Errorf("%s named %q as a new JavaScript workflow's entrypoint, which is not a .js or .mjs path this CLI can write — report the instance",
			describeTarget(resolved), entrypoint)
	}
	scaffold := &initScaffold{
		runtime:    defaults.RuntimeVersion,
		entrypoint: entrypoint,
		starter:    defaults.EntrypointStarter,
	}
	if js {
		scaffold.packageJSON = defaults.PackageJSON
	}
	return scaffold, nil
}

// maxStarterBytes caps the starter an instance may have `wf init` write: a
// starter is a page of code, and the instance's answer is written to disk as is.
const maxStarterBytes = 256 << 10

// errEmptyStarter is writeStarter's refusal to write an empty entrypoint; the
// caller words it.
var errEmptyStarter = errors.New("empty starter")

// writeStarter writes the starter into the folder, reporting whether it wrote
// anything. An empty starter is refused (errEmptyStarter) — but only when it
// would be written: a folder adopting an existing entrypoint never needed one.
//
// An existing entrypoint is left ALONE rather than refused: `wf init` in a
// directory that already holds main.py is the documented way to adopt a script
// you have, and the one thing worse than an init that scaffolds nothing is one
// that ate the file it found. Nothing else in init depends on the scaffold, so
// there is no failure to report — only a note.
func writeStarter(root, entrypoint, starter string) (bool, error) {
	dest := filepath.Join(root, entrypoint)
	if _, err := os.Stat(dest); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("read %s: %w", dest, err)
	}
	if strings.TrimSpace(starter) == "" {
		return false, errEmptyStarter
	}
	if err := wfdir.WriteFile(root, entrypoint, starter); err != nil {
		return false, err
	}
	return true, nil
}
