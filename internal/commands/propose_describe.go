package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/spf13/cobra"
)

// changeListID is the Args of a `ronja propose <verb> <id>` subcommand: a
// bare verb is more likely a folder of that name, so its refusal names the
// propose that takes one. extra is how many more arguments it takes.
func changeListID(verb string, extra int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("ronja propose %[1]s needs the change list's id, which `ronja propose` printed. To propose a directory named %[1]s, run: ronja propose ./%[1]s", verb)
		}
		if len(args) > 1+extra {
			return fmt.Errorf("ronja propose %s takes at most %d %s, not %d: see ronja propose %s --help", verb, 1+extra, agree(1+extra, "argument", "arguments"), len(args), verb)
		}
		return nil
	}
}

func newProposeDescribeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "describe <id> [file|-]",
		Short: "Print or save what an unsent change list tells the person approving it",
		Long: `Print or save what a change list tells the person approving it: its title,
its description and a note per change, in words they understand.

  ronja propose describe <id> > .change.json  # every key, "" where nothing is written
  ronja propose describe <id> .change.json    # save it back
  ronja propose describe <id> -               # save it from stdin

With no file it prints the list's JSON: {"title", "description", "notes"},
with one note per change the person sees as a row (an app, a workflow, an
automation, a table or metric, a database), keyed by the name they see. Fill it
in and save it back. A save merges: a field left out keeps its value, notes
merge per key, and "" clears one. A key the list does not have saves nothing
and names the keys it has. A title is one line of at most 120 characters, a
description at most 4 KiB, a note at most 1 KiB, plain text.

Only an unsent list takes a description: once sent, it is what the person was
told, so propose again to change it. The answer names the next step:
` + "`ronja propose send <id>`" + `, or on a chat's computer, requestPublish.`,
		Args: changeListID("describe", 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			var description json.RawMessage
			if len(args) == 2 {
				var err error
				if description, err = readChangeListDescription(args[1], cmd.InOrStdin()); err != nil {
					return err
				}
			}
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			client := api.New(resolved.URL, resolved.Token)
			if description == nil {
				list, _, err := client.ChangeList(cmd.Context(), id)
				if err != nil {
					return fmt.Errorf("change list %s: %s", id, serverMessage(err))
				}
				if list.Skeleton == nil {
					return fmt.Errorf("%s takes no description: it sends a change list as it is proposed", resolved.URL)
				}
				// Unescaped, so the agent reads "&" and "<" as the person will.
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				enc.SetEscapeHTML(false)
				return enc.Encode(list.Skeleton)
			}
			described, err := client.DescribeChangeList(cmd.Context(), id, description)
			if err != nil {
				return errors.New(serverMessage(err))
			}
			if flagJSON {
				return emitJSON(described)
			}
			out := os.Stdout
			fmt.Fprintf(out, "Saved the description of change list %s.\n", id)
			if s := described.Skeleton; s != nil && !s.Described() {
				var missing []string
				if s.Title == "" {
					missing = append(missing, "a title")
				}
				if s.Description == "" {
					missing = append(missing, "a description")
				}
				fmt.Fprintf(out, "  It still needs %s before it can be sent.\n", strings.Join(missing, " and "))
			}
			printNoNote(out, described.Skeleton)
			printNextStep(out, id, described.Skeleton, described.Next)
			return nil
		},
	}
}

func newProposeSendCmd() *cobra.Command {
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "send <id>",
		Short: "Send a described change list to the person you work for",
		Long: `Send a change list to the person you work for: Ronja tells them in their
inbox, and they have a day to approve it. On a terminal it opens the link.

  ronja propose send <id>

It is refused until the list has a title and a description, naming the
` + "`ronja propose describe`" + ` commands that write them, and it names each change
that has no note: the person sees only its name. A list already sent is
answered as it is. Then run ` + "`ronja propose wait <id>`" + ` in the background.

On a chat's computer the list is sent by requestPublish, not by this command.`,
		Args: changeListID("send", 0),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			client := api.New(resolved.URL, resolved.Token)
			sent, err := client.SendChangeList(cmd.Context(), id)
			if err != nil {
				return errors.New(serverMessage(err))
			}
			if flagJSON {
				return emitJSON(sent)
			}
			approver := "the person this login belongs to"
			if me, err := client.Me(cmd.Context()); err == nil {
				approver = personName(me)
			}
			out := os.Stdout
			fmt.Fprintf(out, "Sent change list %s: %s was notified in Ronja and has until %s to approve it.\n", id, approver, waitTime(sent.ExpiresAt))
			if sent.URL != "" {
				fmt.Fprintf(out, "  %s\n", sent.URL)
			}
			printNoNote(out, sent.Skeleton)
			if strings.HasPrefix(sent.Next, "ronja propose wait ") {
				fmt.Fprintf(out, "Next: %s   (agents: in the background)\n", carryCredential(sent.Next))
			} else {
				printNextStep(out, id, nil, sent.Next)
			}
			if sent.URL != "" && !noBrowser && isTerminal(os.Stdout) {
				_ = openBrowser(sent.URL)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the link without opening it")
	return cmd
}

func newProposeDiscardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "discard <id>",
		Short: "End a change list you no longer mean to send; nothing from it deploys",
		Long: `End a change list: an unsent one is discarded, and a sent one is rejected, as
the person's Reject does. Nothing from it deploys.

  ronja propose discard <id>

On a chat's computer only an unsent list can be discarded: a sent one is the
person's to reject on its card, or propose again to replace it.`,
		Args: changeListID("discard", 0),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			ended, err := api.New(resolved.URL, resolved.Token).RejectChangeList(cmd.Context(), id)
			if err != nil {
				return errors.New(serverMessage(err))
			}
			if flagJSON {
				return emitJSON(ended)
			}
			if ended.SentAt != nil {
				fmt.Fprintf(os.Stdout, "Rejected change list %s: nothing from it deploys.\n", id)
			} else {
				fmt.Fprintf(os.Stdout, "Discarded change list %s.\n", id)
			}
			return nil
		},
	}
}

// printNoNote names each change the skeleton holds no note for.
func printNoNote(out io.Writer, s *api.ChangeListSkeleton) {
	if s == nil {
		return
	}
	var bare []string
	for key, note := range s.Notes {
		if note == "" {
			bare = append(bare, key)
		}
	}
	if len(bare) == 0 {
		return
	}
	slices.Sort(bare)
	fmt.Fprintf(out, "  No note for %s: the person sees only %s.\n", terminalSafe(strings.Join(bare, ", ")), agree(len(bare), "its name", "their names"))
}

// readChangeListDescription reads a description to save, from the file at
// from, or stdin for "-", and refuses anything but a JSON object before it is
// sent.
func readChangeListDescription(from string, stdin io.Reader) (json.RawMessage, error) {
	var raw []byte
	var err error
	if from == "-" {
		raw, err = io.ReadAll(stdin)
	} else {
		raw, err = os.ReadFile(from)
	}
	if err != nil {
		return nil, fmt.Errorf("read the description from %s: %w", describedFrom(from), err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("%s is not a JSON object of title, description and notes, as `ronja propose describe <id>` prints it: %v", describedFrom(from), err)
	}
	return raw, nil
}

func describedFrom(from string) string {
	if from == "-" {
		return "stdin"
	}
	return from
}

// describeProposed saves description, read from from, on the list report
// names, when the server took the list unsent. The list is proposed either
// way, so a refusal says that.
func describeProposed(ctx context.Context, client *api.Client, report *proposeReport, from string, description json.RawMessage) error {
	if description == nil || report.ID == "" {
		return nil
	}
	if report.Status != changeListUnsent {
		report.Notes = append(report.Notes, fmt.Sprintf("this Ronja takes no description, so %s was not saved", describedFrom(from)))
		return nil
	}
	described, err := client.DescribeChangeList(ctx, report.ID, description)
	if err != nil {
		return fmt.Errorf("change list %s was proposed, but its description was not saved: %s", report.ID, serverMessage(err))
	}
	report.Title, report.Changes, report.Skeleton, report.Next = described.Title, described.Changes, described.Skeleton, described.Next
	return nil
}

// printChanges is the list's rows: the key each note is written under, the
// kind and the action.
func printChanges(out io.Writer, changes []api.ChangeListChange) {
	rows := make([][3]string, len(changes))
	keyWidth, kindWidth := 0, 0
	for i, c := range changes {
		rows[i] = [3]string{terminalSafe(c.Key), terminalSafe(c.Kind), terminalSafe(c.Action)}
		keyWidth, kindWidth = max(keyWidth, utf8.RuneCountInString(rows[i][0])), max(kindWidth, utf8.RuneCountInString(rows[i][1]))
	}
	for _, r := range rows {
		fmt.Fprintf(out, "  %-*s  %-*s  %s\n", keyWidth, r[0], kindWidth, r[1], r[2])
	}
}

// printNextStep prints the next step: while the list still lacks its title
// or description, the two commands that print it to a file and save it back,
// and otherwise the server's next.
func printNextStep(out io.Writer, id string, skeleton *api.ChangeListSkeleton, next string) {
	if skeleton != nil && !skeleton.Described() {
		describe := carryCredential("ronja propose describe " + id)
		fmt.Fprintln(out, "Describe it: print it to a file, write the title, the description and a note per change, then save it:")
		fmt.Fprintf(out, "%s > .change.json\n", describe)
		fmt.Fprintf(out, "%s .change.json\n", describe)
		return
	}
	if next != "" {
		fmt.Fprintf(out, "Next: %s\n", carryCredential(next))
	}
}

// carryCredential appends the --profile and --url this run used to a ronja
// command it prints, and to each `ronja …` command a sentence quotes, so the
// command reaches the same organization; any other step, such as
// requestPublish, is returned as it is.
func carryCredential(step string) string {
	var flags string
	if flagProfile != "" {
		flags += " --profile " + shellArg(flagProfile)
	}
	if flagURL != "" {
		flags += " --url " + shellArg(flagURL)
	}
	if flags == "" {
		return step
	}
	if strings.HasPrefix(step, "ronja ") {
		return step + flags
	}
	parts := strings.Split(step, "`")
	for i := 1; i < len(parts); i += 2 {
		if strings.HasPrefix(parts[i], "ronja ") {
			parts[i] += flags
		}
	}
	return strings.Join(parts, "`")
}
