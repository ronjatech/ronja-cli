package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/spf13/cobra"
)

// `ronja secret create` is a CREDENTIAL HANDSHAKE, the third kind of command
// beside the sync loops and the transport (see cli/README.md → the doctrine).
//
// POST /api/v2/secret already takes the value inline; what plain HTTP cannot do
// is take it from a person without the value landing in an argument list, a
// shell history or a transcript. That interactive half — an echo-off prompt, a
// pipe, a file — is the whole of what this command owns. It creates one secret,
// reports its id, and never prints a value. There is no list, show, rotate or
// delete: those stay on `ronja api` and in Ronja.
//
// The rule every line here keeps: no value reaches stdout, stderr, an error
// message or argv. A value lives in memory from where it was read until the one
// request that carries it, and a server error is scrubbed before it is reported
// (redactValues) — of every value of an API key, and of every sensitive field of
// a database login (sensitiveFieldName).

const valuesAreNeverFlags = "values are never flags — they end up in your shell history. Run without a value to be prompted, or pipe it on stdin."

func newSecretCmd() *cobra.Command {
	secret := &cobra.Command{
		Use:   "secret",
		Short: "Store a credential in Ronja without it ever being printed",
		Long: `Store a credential in Ronja without it ever being printed.

  ronja secret create    store an API key or a database login
  ronja secret connect   connect a service like Gmail by signing in to it

With create, the value is typed at an echo-off prompt, piped on stdin, or read
from a file — never passed as a flag, so it never lands in a shell history or an
agent's transcript. With connect, you sign in on the service's own page and the
token goes straight to Ronja. Nothing either command prints contains a value.

Listing, inspecting and deleting secrets stay on plain HTTP and in Ronja:

  ronja api /api/v2/secret/query
  ronja api -X DELETE /api/v2/secret/<id>`,
	}
	secret.AddCommand(newSecretCreateCmd())
	secret.AddCommand(newSecretConnectCmd())
	return secret
}

func newSecretCreateCmd() *cobra.Command {
	var (
		featureID   string
		name        string
		hosts       []string
		fields      []string
		dialect     string
		description string
		fromFile    string
		inBrowser   bool
		noBrowser   bool
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Store an API key or database login as a secret",
		Long: `Store an API key or database login as a secret.

An API key — every --host is a host the value may be sent to, and every --field
is a value a Workflow may read (default: one field named apiKey):

  ronja secret create --feature <feature-id> --name Stripe --host api.stripe.com

A database login — the server applies the database's own list of fields and
refuses a login missing one it needs to connect:

  ronja secret create --feature <feature-id> --name Warehouse --dialect postgres \
    --from-file warehouse.json

Where the value comes from:

  --from-file <path>   read it from a file
  a pipe on stdin      e.g. pbpaste | ronja secret create ...
  a terminal           each field is prompted for with echo off

One field reads the content as the value, with one trailing newline removed.
Several fields, and every database login, read a JSON object of field → value:

  {"host": "db.example.com", "database": "sales", "username": "reader",
   "password": "..."}

When the value must not pass through whatever runs this — an AI agent, a
script — pass --in-browser: nothing is read here. Ronja opens the new secret's
page, you enter the value there, and the command waits (up to 15 minutes) for it
to be saved:

  ronja secret create --feature <feature-id> --name Stripe --host api.stripe.com \
    --in-browser

--no-browser prints that page's link instead of opening it. The form for entering
the value opens only for you, signed in to Ronja. The value never passes through
this command, its arguments, a shell history or a transcript. Whoever can edit
the secret or the feature's workflows can still use it and change where it goes. A host other
people control (*.com, *.github.io) is refused for a value entered this way, and
so is a host with characters other than a-z, 0-9, '-' and '.' (give an
internationalised name in its xn-- form).

Prints the new secret's id, its fields and each value's length — never a value.
--json prints the same as metadata.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			featureID = strings.TrimSpace(featureID)
			name = strings.TrimSpace(name)
			dialect = strings.TrimSpace(dialect)

			// A value smuggled into a flag is refused FIRST, before anything
			// else is looked at, so the refusal is the one message somebody
			// who tried it sees.
			for _, f := range fields {
				if strings.Contains(f, "=") {
					return errors.New(valuesAreNeverFlags)
				}
			}
			if featureID == "" {
				return errors.New("--feature is required: the Feature the secret lives in")
			}
			if name == "" {
				return errors.New("--name is required")
			}
			if dialect != "" && (len(hosts) > 0 || len(fields) > 0) {
				return errors.New("--dialect makes a database login, whose fields and hosts come from the database itself — drop --host and --field")
			}
			if dialect == "" && len(hosts) == 0 {
				if len(fields) > 0 {
					return errors.New("--field needs at least one --host: the hosts the value may be sent to")
				}
				return errors.New("pass --host <host> for an API key (the hosts it may be sent to), or --dialect <database> for a database login")
			}

			fieldNames, err := secretFieldNames(fields)
			if err != nil {
				return err
			}
			hosts, err = secretHosts(hosts)
			if err != nil {
				return err
			}
			if noBrowser && !inBrowser {
				return errors.New("--no-browser goes with --in-browser")
			}
			if inBrowser && fromFile != "" {
				return errors.New("--in-browser reads no value here — the value is entered in Ronja; drop --from-file")
			}

			// Signed in BEFORE any value is read: a value piped into a command
			// that then refuses for want of a login is a value the person has
			// to find and pipe again, and a prompt answered for nothing.
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}

			if inBrowser {
				// No value is read from stdin, a prompt or a file on this
				// path: the person enters it on the secret's page.
				pending := api.CreatePendingSecretInput{
					FeatureID:   featureID,
					Name:        name,
					Description: description,
				}
				if dialect != "" {
					pending.SecretType = "database"
					pending.Dialect = dialect
				} else {
					pending.SecretType = "api_key"
					pending.AllowedURLs = hosts
					pending.AgentFields = fieldNames
					pending.FieldSchema = apiKeyFieldSchema(fieldNames)
				}
				return runSecretInBrowser(cmd.Context(), newClient(resolved.URL, resolved.Token), pending, fieldNames, noBrowser)
			}

			var creds secretValues
			if dialect != "" {
				creds, err = readDialectValues(fromFile)
			} else {
				creds, err = readAPIKeyValues(cmd.Context(), fieldNames, fromFile)
			}
			if err != nil {
				return err
			}

			in := api.CreateSecretInput{
				FeatureID:   featureID,
				Name:        name,
				Description: description,
				Credentials: creds.body,
			}
			if dialect != "" {
				in.SecretType = "database"
				in.Dialect = dialect
			} else {
				in.SecretType = "api_key"
				in.AllowedURLs = hosts
				// Every field is an agentField: a Workflow can read only the
				// fields listed there, so a field left off is one nothing can
				// ever use.
				in.AgentFields = fieldNames
				in.FieldSchema = apiKeyFieldSchema(fieldNames)
			}

			client := newClient(resolved.URL, resolved.Token)
			created, err := client.CreateSecret(cmd.Context(), in)
			if err != nil {
				if api.IsTimeout(err) {
					return redactValues(fmt.Errorf("%w — the secret may have been created anyway; look for %q in Ronja before running this again", err, name), creds.redact)
				}
				return redactValues(err, creds.redact)
			}

			return reportCreatedSecret(created, featureID, creds.lengths)
		},
	}

	cmd.Flags().StringVar(&featureID, "feature", "", "the Feature the secret lives in (required)")
	cmd.Flags().StringVar(&name, "name", "", "the secret's name (required)")
	cmd.Flags().StringArrayVar(&hosts, "host", nil, "a host the value may be sent to, e.g. api.stripe.com or *.example.com (repeatable; makes an API key)")
	cmd.Flags().StringArrayVar(&fields, "field", nil, "a field name to store and let Workflows read (repeatable; default apiKey). Never a value")
	cmd.Flags().StringVar(&dialect, "dialect", "", "make a database login for this database, e.g. postgres, mysql, sqlserver, snowflake, bigquery")
	cmd.Flags().StringVar(&description, "description", "", "a description shown in Ronja")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "read the value from this file instead of stdin or a prompt")
	cmd.Flags().BoolVar(&inBrowser, "in-browser", false, "read no value here: open the new secret in Ronja, where the value is entered, and wait for it")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "with --in-browser, print the link instead of opening it")
	return cmd
}

// secretFieldNames validates the --field list, defaulting to one apiKey field.
// Names are kept in the order given (that order is the prompt order and the
// form's order in Ronja), and a repeat is refused rather than silently merged.
func secretFieldNames(fields []string) ([]string, error) {
	if len(fields) == 0 {
		return []string{"apiKey"}, nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			return nil, errors.New("--field cannot be empty")
		}
		if seen[f] {
			return nil, fmt.Errorf("--field %s is given twice", f)
		}
		seen[f] = true
		out = append(out, f)
	}
	return out, nil
}

// secretHosts validates the --host list. Each one is a HOSTNAME, matched
// against the bare hostname of the URL a request goes to — so a URL, a path, a
// port or a user would be stored and then never match anything, and the secret
// would be unusable with no error anywhere. `*.` is allowed as a prefix only,
// which is the one wildcard the server matches.
func secretHosts(hosts []string) ([]string, error) {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			return nil, errors.New("--host cannot be empty")
		}
		rest := strings.TrimPrefix(h, "*.")
		if rest == "" || strings.ContainsAny(rest, "/:@?#*") || strings.IndexFunc(rest, unicode.IsSpace) >= 0 {
			return nil, fmt.Errorf("--host %q is not a host: pass the host only, e.g. api.stripe.com or *.example.com", h)
		}
		out = append(out, h)
	}
	return out, nil
}

// apiKeyFieldSchema describes the fields so Ronja's form shows them as secret
// inputs when the value is rotated there later.
func apiKeyFieldSchema(fieldNames []string) json.RawMessage {
	schema := map[string]any{}
	for i, f := range fieldNames {
		schema[f] = map[string]any{"label": f, "type": "secret", "secret": true, "required": true, "order": i}
	}
	body, _ := json.Marshal(schema)
	return body
}

// reportCreatedSecret prints the one result line, or --json metadata. Field
// names and lengths only.
func reportCreatedSecret(created *api.CreatedSecret, featureID string, lengths []fieldLength) error {
	if created.FeatureID != nil && *created.FeatureID != "" {
		featureID = *created.FeatureID
	}
	if flagJSON {
		return emitJSON(map[string]any{
			"id":         created.ID,
			"name":       created.Name,
			"secretType": created.SecretType,
			"featureID":  featureID,
			"fields":     lengths,
		})
	}

	noun := "fields"
	if len(lengths) == 1 {
		noun = "field"
	}
	parts := make([]string, 0, len(lengths))
	for _, l := range lengths {
		unit := "chars"
		if l.Length == 1 {
			unit = "char"
		}
		parts = append(parts, fmt.Sprintf("%s — %d %s", l.Name, l.Length, unit))
	}
	fmt.Fprintf(os.Stdout, "%s  %s (%s, feature %s, %d %s: %s)\n",
		created.ID, created.Name, created.SecretType, featureID, len(lengths), noun, strings.Join(parts, ", "))
	return nil
}
