package commands

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// featureSpace is the Space featureID is in, nil when the instance names none
// for it.
func featureSpace(l *spaceLookup, featureID string) (*api.Space, error) {
	feature, err := l.feature(featureID)
	if err != nil {
		return nil, err
	}
	if feature.SpaceID == "" {
		return nil, nil
	}
	return l.space(feature.SpaceID)
}

// recordedSpace is the Space a stack on a feature in space records ("spaceID",
// "space"): space itself outside the organization's default Space, nil in it.
// Only a recorded Space is held to the CLI's Space rule, so a folder whose
// stacks are all in the default Space records none and keeps the format
// version an older CLI reads.
func recordedSpace(space *api.Space) *api.Space {
	if space == nil || space.IsDefault {
		return nil
	}
	return space
}

// stampSpace records space on the stack a folder is being declared on.
func stampSpace(m *wfdir.Manifest, stack string, space *api.Space) {
	st, ok := m.Stacks[stack]
	if space == nil || !ok || st.SpaceID != "" {
		return
	}
	st.SpaceID, st.Space = space.ID, space.Name
	m.Stacks[stack] = st
}

// declareSpaceFailed is the refusal when the Space a stack is being declared
// for cannot be read.
func declareSpaceFailed(featureID, stack string, err error) error {
	return fmt.Errorf("could not read which Space feature %s is in (%w), and stack %q records its Space when it is outside the organization's default Space — %s was not changed; try again once the instance answers", featureID, err, stack, wfdir.ManifestName)
}

// defaultStackName names a stack declared with only a feature: "main" for a
// feature in the organization's default Space (whatever that Space is called)
// or on an instance that names no Space for it, and otherwise after its Space,
// lowercased ("dev"). A stack `existing` already declares on that feature is
// that stack. Any other name `existing` already declares is refused, since
// taking it would point that stack somewhere else; the person names one.
func defaultStackName(l *spaceLookup, featureID string, existing map[string]wfdir.Stack) (string, error) {
	for _, name := range sortedKeys(existing) {
		if existing[name].FeatureID == featureID {
			return name, nil
		}
	}
	space, err := featureSpace(l, featureID)
	if err != nil {
		return "", stackNameUnread(featureID, err)
	}
	name := "main"
	if space != nil && !space.IsDefault {
		name = strings.Trim(stackNameRun.ReplaceAllString(strings.ToLower(space.Name), "-"), "-._")
		if wfdir.ValidStackName(name) != nil {
			return "", fmt.Errorf("feature %s is in Space %q, which does not make a stack name — name the stack with --stack <name>", featureID, space.Name)
		}
	}
	for declared := range existing {
		if strings.EqualFold(declared, name) {
			return "", fmt.Errorf("this folder already has a stack %q, so a stack for feature %s cannot be named %q — name it with --stack <name>", declared, featureID, name)
		}
	}
	return name, nil
}

var stackNameRun = regexp.MustCompile(`[^a-z0-9._-]+`)

// stackNameUnread is the refusal when the Space a default stack name comes
// from cannot be read.
func stackNameUnread(featureID string, err error) error {
	return fmt.Errorf("name the stack with --stack <name>: which Space feature %s is in could not be read to name it after (%w)", featureID, err)
}

// settleStackSpace is what `init` and `clone` decide before they write
// anything: the Space the folder's stack records, and, with no --stack, the
// stack itself. A feature outside the organization's default Space declares a
// stack named after that Space (defaultStackName), so a Dev folder is born a
// stack checked against Dev. A feature in the default Space, or on an instance
// whose features name no Space, is written with no stack unless --stack names
// one, and records no Space. With no
// --stack, a Space that cannot be read keeps it too, with a note saying how to
// name a stack; with --stack it refuses, since that stack may be one the Space
// rule must hold. Sets flagStack, so it runs before anything reads it.
func settleStackSpace(ctx context.Context, resolved *config.Resolved, featureID string) (*api.Space, error) {
	if featureID == "" {
		return nil, nil
	}
	l := newSpaceLookup(ctx, resolved)
	space, err := featureSpace(l, featureID)
	if err != nil {
		if flagStack != "" {
			return nil, declareSpaceFailed(featureID, flagStack, err)
		}
		fmt.Fprintf(os.Stderr, "  Note: could not read which Space feature %s is in (%v), so this folder is written without a stack. If the feature is outside the organization's default Space, run it again with --stack <name> so its deploys are checked against that Space.\n", featureID, err)
		return nil, nil
	}
	space = recordedSpace(space)
	if space == nil || flagStack != "" {
		return space, nil
	}
	name, err := defaultStackName(l, featureID, nil)
	if err != nil {
		return nil, err
	}
	flagStack = name
	return space, nil
}

// bindDefaultStack names the stack `bind --feature` declares when no --stack
// was given (defaultStackName). It answers "" when the instance names no
// Space for the feature, and bind then refuses a feature with no stack.
func bindDefaultStack(ctx context.Context, resolved *config.Resolved, kind wfdir.Kind, featureID string) (string, error) {
	l := newSpaceLookup(ctx, resolved)
	feature, err := l.feature(featureID)
	if err != nil {
		return "", stackNameUnread(featureID, err)
	}
	if feature.SpaceID == "" {
		return "", nil
	}
	root, err := folderRootHere(kind)
	if err != nil {
		return "", err
	}
	m, err := wfdir.LoadManifest(root, kind)
	if err != nil {
		return "", err
	}
	return defaultStackName(l, featureID, m.Stacks)
}
