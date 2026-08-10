package commands

import (
	"fmt"
	"os"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// `ronja env` emits shell assignments for RONJA_URL and RONJA_TOKEN.
//
// Why this exists alongside the credential FILE:
//
//   - `eval "$(ronja env)"` sets the variables without the token ever being
//     displayed — eval consumes the command's stdout, so the value moves from
//     the credential file into the environment and nothing prints it. Reading
//     a credential and echoing it are different things; this is the reading
//     kind.
//   - It needs no `jq`. The documented file-substitution form
//     (`$(jq -r '.profiles[…].token' …)`) is fine but silently unavailable on a
//     machine without jq, which is common enough to matter.
//   - It carries the base URL too, so a caller stops hardcoding the instance.
//
// IMPORTANT for agents: shell state does not survive between tool invocations —
// each call is typically a fresh shell. `eval "$(ronja env)"` in one call and
// `$RONJA_TOKEN` in the next will find an empty variable. Combine them in a
// single command:
//
//	eval "$(ronja env)" && curl -H "Authorization: Bearer $RONJA_TOKEN" "$RONJA_URL/api/v2/..."
func newEnvCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "env",
		Short: "Emit shell assignments for RONJA_URL and RONJA_TOKEN",
		Long: `Emit shell assignments for RONJA_URL and RONJA_TOKEN.

Meant to be evaluated, not read:

  eval "$(ronja env)"

Used that way the token moves from the credential file into your environment
without ever being displayed — eval consumes this command's output. Running
'ronja env' bare prints a live credential to your terminal.

Shell state does not persist between separate agent tool calls, so combine the
eval with the work in one command:

  eval "$(ronja env)" && curl -H "Authorization: Bearer $RONJA_TOKEN" \
    "$RONJA_URL/api/v2/authentication/me"

With --json, emits {"RONJA_URL": ..., "RONJA_TOKEN": ...} for callers that are
not a POSIX shell.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := config.Resolve(flagURL, flagProfile)
			if err != nil {
				return err
			}
			if resolved.Token == "" {
				return fmt.Errorf("not signed in to %s — run `ronja login --url %s`",
					resolved.URL, resolved.URL)
			}

			// A terminal on stdout means nothing is capturing this, so the
			// credential is about to be displayed. Warn on stderr (which eval
			// does not consume) rather than refusing — a human inspecting the
			// output is a legitimate, if unwise, thing to do.
			if term.IsTerminal(int(os.Stdout.Fd())) {
				fmt.Fprintln(os.Stderr,
					"warning: this prints a live credential. Use: eval \"$(ronja env)\"")
			}

			if flagJSON {
				return emitJSON(map[string]string{
					config.EnvURL:   resolved.URL,
					config.EnvToken: resolved.Token,
				})
			}
			fmt.Printf("export %s=%s\n", config.EnvURL, shellQuote(resolved.URL))
			fmt.Printf("export %s=%s\n", config.EnvToken, shellQuote(resolved.Token))
			return nil
		},
	}
}

// shellQuote wraps a value in single quotes so the shell takes it literally.
//
// Neither a base64url token nor an http(s) URL contains a quote today, but a
// credential is exactly the wrong thing to leave one shell-metacharacter away
// from being mis-parsed — or from injecting into the eval that consumes this.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
