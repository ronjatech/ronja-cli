package commands

import (
	"fmt"
	"os"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Its output is for `eval`, which consumes stdout, so the token moves into the
// environment without being displayed; it must never become a printing command.
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

			// A terminal on stdout means the credential is about to be
			// displayed: warn on stderr rather than refuse.
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

// shellQuote single-quotes a value for the eval that consumes it; no token or
// URL contains a quote today, but a credential must not be one metacharacter
// from injection.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
