package commands

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// resolveTreeInstance is resolveInstance for a command over the tree at dir.
func resolveTreeInstance(dir string) (*config.Resolved, error) {
	return signedIn(resolveStackCredential(dir, false, false))
}

// resolveDeployInstance is resolveInstance for a command that changes the
// organization: it switches to the stack's stored profile only on a yes
// (confirmCredentialSwitch). yes is the command's own --yes, false for a
// command that has none.
func resolveDeployInstance(yes bool) (*config.Resolved, error) {
	return signedIn(resolveStackCredential("", true, yes))
}

// resolveDeployTreeInstance is resolveDeployInstance over the tree at dir.
func resolveDeployTreeInstance(dir string, yes bool) (*config.Resolved, error) {
	return signedIn(resolveStackCredential(dir, true, yes))
}

func signedIn(resolved *config.Resolved, err error) (*config.Resolved, error) {
	if err != nil {
		return nil, err
	}
	if resolved.Token == "" {
		return nil, notSignedIn(resolved)
	}
	return resolved, nil
}

// resolveStackCredential is config.Resolve, switched to the stored profile
// for the organization --stack names when nothing chose a credential. tree is
// the directory a tree command walks, "" for a command on the folder here. A
// read switches with a note; a deploy only on a yes, since the committed
// ronja.json is what names the organization.
func resolveStackCredential(tree string, deploy, yes bool) (*config.Resolved, error) {
	resolved, err := config.Resolve(flagURL, flagProfile)
	if err != nil {
		return nil, err
	}
	other, err := stackProfile(resolved, tree)
	if err != nil || other == "" {
		return resolved, err
	}
	if deploy && !yes {
		if err := confirmCredentialSwitch(resolved.Profile, other); err != nil {
			return nil, err
		}
	}
	current := resolved.Profile
	if resolved, err = config.Resolve(flagURL, other); err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "  Using profile %q, not %q: it is the one stored for stack %q's organization. Pass --profile to use another.\n", other, current, flagStack)
	return resolved, nil
}

// confirmCredentialSwitch asks before a deploy acts with another stored
// profile than the current one. With no terminal it refuses, naming the
// --profile that says the same thing on purpose.
func confirmCredentialSwitch(current, other string) error {
	if flagJSON || !isTerminal(os.Stdin) {
		return fmt.Errorf("stack %q deploys to the organization stored as profile %q, and the current profile %q reaches another one. A deploy switches profiles only when you say yes, and there is no terminal to ask on — pass --profile %s to deploy with it. Nothing was done",
			flagStack, other, current, other)
	}
	fmt.Fprintf(os.Stderr, "  Stack %q deploys to the organization stored as profile %q, not to profile %q's. Deploy with profile %q? [y/N] ", flagStack, other, current, other)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	return fmt.Errorf("nothing was done: stack %q deploys with profile %q — answer yes, or pass --profile %s", flagStack, other, other)
}

// stackProfile is the stored profile for the organization --stack names, when
// nothing chose a credential (no --profile, RONJA_PROFILE, --url, RONJA_URL or
// RONJA_TOKEN) and the current profile reaches another one: each organization
// of a repository has its own credential, and --stack names which. It reads
// the folder here, or every folder under tree; a stack that names several
// organizations there is refused, since no one profile fits them. Empty when
// there is nothing to switch to.
func stackProfile(resolved *config.Resolved, tree string) (string, error) {
	if flagStack == "" || flagProfile != "" || flagURL != "" {
		return "", nil
	}
	for _, env := range []string{config.EnvProfile, config.EnvURL, config.EnvToken} {
		if strings.TrimSpace(os.Getenv(env)) != "" {
			return "", nil
		}
	}
	targets := stackTargets(tree)
	if len(targets) == 0 {
		return "", nil
	}
	if len(targets) > 1 {
		var names []string
		for _, key := range targets {
			names = append(names, describeKeyForApply(key))
		}
		return "", fmt.Errorf("stack %q names %d organizations under %s: %s — no one stored profile fits them all; use --profile or RONJA_TOKEN to say which",
			flagStack, len(targets), tree, strings.Join(names, ", "))
	}
	key := targets[0]
	if key.TenantID == resolved.TenantID {
		return "", nil
	}
	url, err := config.NormalizeURL(key.URL)
	if err != nil {
		return "", nil
	}
	file, err := config.Load()
	if err != nil {
		return "", nil
	}
	name, _ := file.FindIdentity(url, key.TenantID)
	if name == resolved.Profile {
		return "", nil
	}
	return name, nil
}

// stackTargets are the distinct organizations --stack names in the folder
// here (tree "") or in the folders under tree, skipping a stack that names no
// organization.
func stackTargets(tree string) []wfdir.InstanceKey {
	var stacks []wfdir.Stack
	if tree == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil
		}
		root, err := wfdir.FindRoot(cwd)
		if err != nil {
			return nil
		}
		raw, err := os.ReadFile(wfdir.ManifestPath(root))
		if err != nil {
			return nil
		}
		var probe struct {
			Stacks map[string]wfdir.Stack `json:"stacks"`
		}
		if json.Unmarshal(raw, &probe) != nil {
			return nil
		}
		if st, ok := probe.Stacks[flagStack]; ok {
			stacks = append(stacks, st)
		}
	} else {
		found, err := discoverFolders(tree)
		if err != nil {
			return nil
		}
		for _, folder := range found {
			if folder.Manifest == nil {
				continue
			}
			if st, ok := folder.Manifest.Stacks[flagStack]; ok {
				stacks = append(stacks, st)
			}
		}
	}
	var out []wfdir.InstanceKey
	seen := map[wfdir.InstanceKey]bool{}
	for _, st := range stacks {
		key := st.Key()
		if url, err := config.NormalizeURL(key.URL); err == nil {
			key.URL = url
		}
		if key.TenantID == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}
