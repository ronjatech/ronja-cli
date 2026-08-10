package commands

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/spf13/cobra"
)

// `ronja profile` manages the local credential store.
//
// This is not a breach of the "sync verbs yes, resource verbs no" doctrine that
// keeps the CLI from growing a subcommand per endpoint: a profile is local
// machine state with no server-side counterpart, so there is no API surface for
// these to drift from.
//
// There is deliberately no `profile add`. A profile is created by authenticating
// — a profile without a working credential in it is a trap that reports "signed
// in" and then 401s.
func newProfileCmd() *cobra.Command {
	profile := &cobra.Command{
		Use:   "profile",
		Short: "Manage stored logins",
		Long: `Manage stored logins.

Each profile is one instance and one organization, with the access token that
reaches it. A token belongs to a single organization, so being a member of two
means two profiles — created by signing in to each.

  ronja profile list
  ronja profile use acme-retail
  ronja profile rename app-2 northwind`,
	}
	profile.AddCommand(newProfileListCmd(), newProfileUseCmd(), newProfileRenameCmd())
	return profile
}

// profileRow is the --json shape. The token is never included: this command
// exists to describe credentials, not to hand them out (see `ronja env`).
type profileRow struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	TenantID   string `json:"tenantID,omitempty"`
	TenantName string `json:"tenantName,omitempty"`
	UserEmail  string `json:"userEmail,omitempty"`
	Current    bool   `json:"current"`
	ExpiresAt  string `json:"tokenExpiresAt,omitempty"`
	// Expired is derived from ExpiresAt rather than left to the caller, because
	// it is the one thing the store can diagnose about itself without asking
	// the server — and an expired credential explains a 401 that otherwise
	// looks like a permissions problem.
	Expired bool `json:"expired,omitempty"`
}

func newProfileListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List stored profiles",
		Long: `List stored profiles.

Reports what is on disk without contacting any instance, so it works offline and
never blocks. It cannot tell you whether a token has been revoked — that needs
'ronja whoami', which asks the server.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := config.Load()
			if err != nil {
				return err
			}
			rows := make([]profileRow, 0, len(f.Profiles))
			for _, name := range f.Names() {
				p := f.Profiles[name]
				if p == nil {
					continue
				}
				rows = append(rows, profileRow{
					Name:       name,
					URL:        p.URL,
					TenantID:   p.TenantID,
					TenantName: p.TenantName,
					UserEmail:  p.UserEmail,
					Current:    name == f.Current,
					ExpiresAt:  p.TokenExpiry,
					Expired:    isExpired(p.TokenExpiry),
				})
			}

			if flagJSON {
				return emitJSON(map[string]any{"profiles": rows, "current": f.Current})
			}
			if len(rows) == 0 {
				fmt.Fprintln(os.Stderr, "  No profiles yet — run `ronja login`.")
				warnLegacyStore()
				return nil
			}
			printProfiles(rows)
			warnLegacyStore()
			return nil
		},
	}
}

func printProfiles(rows []profileRow) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\tNAME\tINSTANCE\tORGANIZATION\t")
	for _, r := range rows {
		marker := " "
		if r.Current {
			marker = "*"
		}
		org := r.TenantName
		if org == "" {
			// Distinguishes "no organization" from "we never learned its name",
			// which are different situations with different fixes.
			org = "—"
		}
		if r.Expired {
			org += "  (token expired — run `ronja login`)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t\n", marker, r.Name, r.URL, org)
	}
	_ = w.Flush()
}

// isExpired reports whether a recorded expiry has passed. An unparseable or
// absent stamp is NOT expired: a token supplied with --with-token has no
// recorded lifetime, and guessing "expired" for it would be a lie that sends
// someone to re-run a login they do not need.
func isExpired(raw string) bool {
	if raw == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false
	}
	return time.Now().After(at)
}

func newProfileUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use [name]",
		Short: "Make a profile the default for later commands",
		Long: `Make a profile the default for later commands.

Affects every command that does not name a profile or a URL of its own. It does
not override $RONJA_TOKEN, which always wins.

With no name, and at a terminal, this opens a picker starting on the current
profile — arrow keys to move, enter to choose, q or Escape to cancel. Anywhere
else (a script, a pipe, an agent) the name is required, because a prompt nobody
can answer is a hang.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			if name == "" {
				chosen, err := pickProfile()
				if err != nil {
					if errors.Is(err, errPickerCancelled) {
						// Backing out is a choice, not a failure. Exit zero and
						// say nothing changed.
						fmt.Fprintln(os.Stderr, "  Cancelled — still using the profile you were on.")
						return nil
					}
					return err
				}
				name = chosen
			}

			err := config.Update(func(f *config.File) error {
				if _, ok := f.Profiles[name]; !ok {
					return unknownProfile(f, name)
				}
				f.Current = name
				return nil
			})
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(map[string]any{"current": name})
			}
			fmt.Fprintf(os.Stderr, "  Now using %s\n", name)
			return nil
		},
	}
}

// pickProfile runs the interactive chooser, returning the selected name.
func pickProfile() (string, error) {
	// --json means a machine is reading, and a machine cannot answer a picker.
	// Refused with the flag named, rather than drawing escape sequences into
	// something parsing our stdout.
	if flagJSON {
		return "", errors.New("--json cannot open a picker — name the profile you want")
	}
	f, err := config.Load()
	if err != nil {
		return "", err
	}
	names := f.Names()
	if len(names) == 0 {
		return "", errors.New("no profiles yet — run `ronja login`")
	}

	items := make([]pickerItem, 0, len(names))
	start := 0
	for i, name := range names {
		p := f.Profiles[name]
		if name == f.Current {
			// Start on the current profile, so enter alone changes nothing.
			start = i
		}
		org := p.TenantName
		if org == "" {
			org = "—"
		}
		note := ""
		if isExpired(p.TokenExpiry) {
			note = "(token expired)"
		}
		items = append(items, pickerItem{Label: name, Detail: org, Note: strings.TrimSpace(p.URL + "  " + note)})
	}

	i, err := pick("Select a profile:", items, start)
	if err != nil {
		return "", err
	}
	return names[i], nil
}

func newProfileRenameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <old> <new>",
		Short: "Rename a stored profile",
		Long: `Rename a stored profile.

A profile's name is a local handle — nothing on the server refers to it, and
renaming does not touch the credential. Names are lowercase letters, digits and
hyphens.

A workflow folder's binding is keyed by instance and organization, not by
profile name, so renaming cannot orphan one.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			from, to := args[0], args[1]
			if err := config.ValidateName(to); err != nil {
				return err
			}
			err := config.Update(func(f *config.File) error {
				if _, ok := f.Profiles[from]; !ok {
					return unknownProfile(f, from)
				}
				return f.Rename(from, to)
			})
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(map[string]any{"renamed": from, "to": to})
			}
			fmt.Fprintf(os.Stderr, "  Renamed %s to %s\n", from, to)
			return nil
		},
	}
}

// unknownProfile names what DOES exist. "No such profile" alone leaves the
// reader to run a second command to find out what they should have typed.
func unknownProfile(f *config.File, name string) error {
	names := f.Names()
	if len(names) == 0 {
		return fmt.Errorf("no such profile %q — there are none stored yet, run `ronja login`", name)
	}
	return fmt.Errorf("no such profile %q — stored profiles are: %s", name, strings.Join(names, ", "))
}
