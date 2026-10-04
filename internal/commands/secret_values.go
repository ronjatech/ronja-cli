package commands

// The value half of `ronja secret create`: reading a credential from a hidden
// prompt, a pipe or a file, and scrubbing it from any error before it is
// reported. Split from secret.go (the command and its flags) so the rule every
// line here keeps is in one place: no value reaches stdout, stderr, an error
// message or argv.
//
// sensitiveFieldName below mirrors rsecret.SensitiveNamePattern; a backend test
// (resource/rsecret's TestCLISensitiveNamePatternMatchesTheBackend) reads this
// file and fails when the two differ.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// readPassword reads one line from a terminal with echo off.
//
// A package var for the same reason isTerminal is one: the prompted path is
// the one a person uses, and a test that cannot reach it cannot pin that the
// prompted value goes into the body and nowhere else.
var readPassword = func(fd int) ([]byte, error) { return term.ReadPassword(fd) }

// maxSecretInput bounds a piped or file-read value. A credential is bytes to
// kilobytes; a service-account JSON is a few KB. A megabyte is far past any real
// one and still small enough that a misdirected pipe fails rather than being
// held whole and sent.
const maxSecretInput = 1 << 20

// minRedactLen is the shortest value redactValues scrubs from an error. A value
// shorter than this would match inside ordinary words of the server's message
// and turn it into noise, and is not a credential worth the damage.
const minRedactLen = 4

// secretValues is a credentials blob ready to send, plus what may be said
// about it: each field's length, and the values to scrub from any error.
type secretValues struct {
	body    json.RawMessage
	lengths []fieldLength
	redact  []string
}

// fieldLength is a field name and its value's length in characters — the one
// thing about a value that is safe to print, and what catches a truncated paste.
type fieldLength struct {
	Name   string `json:"name"`
	Length int    `json:"length"`
}

// readSecretInput returns the content of --from-file, or of a piped stdin.
// ok is false when neither applies — stdin is a terminal and no file was named
// — which is the prompting case.
func readSecretInput(fromFile string) (content []byte, ok bool, err error) {
	if fromFile != "" {
		f, err := os.Open(fromFile)
		if err != nil {
			return nil, false, fmt.Errorf("read --from-file: %w", err)
		}
		defer f.Close()
		body, over, err := readLimit(f, maxSecretInput)
		if err != nil {
			return nil, false, fmt.Errorf("read --from-file: %w", err)
		}
		if over {
			return nil, false, fmt.Errorf("%s is larger than %d MiB, which no credential is — is that the right file?", fromFile, maxSecretInput>>20)
		}
		return body, true, nil
	}
	if isTerminal(os.Stdin) {
		return nil, false, nil
	}
	// Not readStdinLimit: its over-the-cap remedy is "write it to a file and
	// pass the path", and --from-file has the same cap — no credential is a
	// megabyte, so the answer for both inputs is that this is the wrong input.
	body, over, err := readLimit(os.Stdin, maxSecretInput)
	if err != nil {
		return nil, false, fmt.Errorf("read standard input: %w", err)
	}
	if over {
		return nil, false, fmt.Errorf("standard input carried more than %d MiB, which no credential is — is that the right input?", maxSecretInput>>20)
	}
	return body, true, nil
}

// emptyInputError is the refusal for nothing on stdin. It is a refusal, not a
// prompt and not a fallback: an agent running with an empty stdin must hear
// that it supplied nothing rather than wait on a question nobody will answer.
func emptyInputError(fromFile string) error {
	if fromFile != "" {
		return fmt.Errorf("%s is empty — there is no value to store", fromFile)
	}
	return errors.New("no value on stdin — pipe one in, or pass --in-browser to enter it yourself in Ronja")
}

// errPromptCancelled is the answer to Ctrl-C at a hidden prompt.
var errPromptCancelled = errors.New("cancelled; nothing was stored")

// readHidden is readPassword that gives way to ctx.
//
// ronja's root context owns SIGINT (signalContext), so Ctrl-C does not kill the
// process: it cancels the context. term.ReadPassword knows nothing of that and
// goes on waiting for Enter, which reads as a prompt that ignores Ctrl-C. So
// the read runs on its own goroutine and the prompt returns the moment ctx is
// done — restoring the terminal first, since the read that would have restored
// its echo never returns. That goroutine stays blocked on stdin; the process is
// about to exit, so it costs nothing.
//
// A context that is done wins even over a read that also finished: a person
// who pressed Ctrl-C and then Enter asked to stop, not to be told the value
// was empty.
func readHidden(ctx context.Context, fd int) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, errPromptCancelled
	}
	read := readPassword // read here, not in the goroutine, so a test's restore does not race it
	state, stateErr := term.GetState(fd)
	type result struct {
		raw []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		raw, err := read(fd)
		done <- result{raw, err}
	}()
	select {
	case r := <-done:
		if ctx.Err() != nil {
			return nil, errPromptCancelled
		}
		return r.raw, r.err
	case <-ctx.Done():
		if stateErr == nil {
			_ = term.Restore(fd, state)
		}
		return nil, errPromptCancelled
	}
}

// readAPIKeyValues collects an API key's fields from a file, a pipe or prompts.
func readAPIKeyValues(ctx context.Context, fieldNames []string, fromFile string) (secretValues, error) {
	content, ok, err := readSecretInput(fromFile)
	if err != nil {
		return secretValues{}, err
	}

	values := map[string]string{}
	switch {
	case !ok:
		// A terminal: one echo-off prompt per field, on stderr.
		for _, field := range fieldNames {
			fmt.Fprintf(os.Stderr, "%s (input hidden): ", field)
			raw, err := readHidden(ctx, int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if errors.Is(err, errPromptCancelled) {
				return secretValues{}, err
			}
			if err != nil {
				return secretValues{}, fmt.Errorf("read %s: %w", field, err)
			}
			v := strings.TrimRight(string(raw), "\r\n")
			if strings.TrimSpace(v) == "" {
				return secretValues{}, fmt.Errorf("no value entered for %s — nothing was stored", field)
			}
			values[field] = v
		}
	case len(fieldNames) == 1:
		if len(bytes.TrimSpace(content)) == 0 {
			return secretValues{}, emptyInputError(fromFile)
		}
		if looksLikeFieldObject(content, fieldNames[0]) {
			// A warning, not a refusal: a JSON value is legitimate (a service
			// account's key file is one). It names the field and never a
			// byte of the content.
			fmt.Fprintf(os.Stderr, "warning: this looks like a JSON object with a %q key; with one --field the whole input is stored as the value of %s\n", fieldNames[0], fieldNames[0])
		}
		values[fieldNames[0]] = stripOneNewline(string(content))
	default:
		if len(bytes.TrimSpace(content)) == 0 {
			return secretValues{}, emptyInputError(fromFile)
		}
		obj, err := decodeValueObject(content, fromFile)
		if err != nil {
			return secretValues{}, err
		}
		expected := strings.Join(fieldNames, ", ")
		var unknown, missing []string
		for k := range obj {
			if !contains(fieldNames, k) {
				unknown = append(unknown, k)
			}
		}
		for _, f := range fieldNames {
			if _, ok := obj[f]; !ok {
				missing = append(missing, f)
			}
		}
		sort.Strings(unknown)
		if len(unknown) > 0 {
			return secretValues{}, fmt.Errorf("unexpected field %s in the input — the fields are: %s", strings.Join(unknown, ", "), expected)
		}
		if len(missing) > 0 {
			return secretValues{}, fmt.Errorf("the input has no %s — the fields are: %s", strings.Join(missing, ", "), expected)
		}
		for _, f := range fieldNames {
			var s string
			if err := json.Unmarshal(obj[f], &s); err != nil {
				return secretValues{}, fmt.Errorf("the value for %s must be a JSON string", f)
			}
			if strings.TrimSpace(s) == "" {
				return secretValues{}, fmt.Errorf("the value for %s is empty — nothing was stored", f)
			}
			values[f] = s
		}
	}

	body, err := json.Marshal(values)
	if err != nil {
		return secretValues{}, fmt.Errorf("encode the value: %w", err)
	}
	out := secretValues{body: body}
	for _, f := range fieldNames {
		out.lengths = append(out.lengths, fieldLength{Name: f, Length: utf8.RuneCountInString(values[f])})
		// Every field of an API key is a secret, whatever it is called.
		out.redact = append(out.redact, values[f])
	}
	return out, nil
}

// readDialectValues reads a database login, which is always a JSON object. The
// fields are the database's, and the server — which holds that list — checks
// them; the CLI checks only that there is an object with something in it.
func readDialectValues(fromFile string) (secretValues, error) {
	content, ok, err := readSecretInput(fromFile)
	if err != nil {
		return secretValues{}, err
	}
	if !ok {
		return secretValues{}, errors.New("database credentials take a JSON object: use --from-file, pipe it on stdin, or pass --in-browser to enter it in Ronja")
	}
	if len(bytes.TrimSpace(content)) == 0 {
		return secretValues{}, emptyInputError(fromFile)
	}
	obj, err := decodeValueObject(content, fromFile)
	if err != nil {
		return secretValues{}, err
	}
	if len(obj) == 0 {
		return secretValues{}, errors.New("the database login is an empty JSON object — nothing was stored")
	}

	names := make([]string, 0, len(obj))
	for k := range obj {
		names = append(names, k)
	}
	sort.Strings(names)

	out := secretValues{body: bytes.TrimSpace(content)}
	for _, k := range names {
		raw := bytes.TrimSpace(obj[k])
		length := len(raw)
		var s string
		isString := json.Unmarshal(raw, &s) == nil
		if isString {
			length = utf8.RuneCountInString(s)
		}
		out.lengths = append(out.lengths, fieldLength{Name: k, Length: length})

		// Only a sensitive field's value is scrubbed from an error. A login's
		// other fields are the very words a server's refusal is made of — a
		// username of "postgres" would otherwise turn "postgres credentials
		// missing required fields: database" into "[redacted] credentials…".
		if !sensitiveFieldName.MatchString(k) {
			continue
		}
		if isString {
			out.redact = append(out.redact, s)
			continue
		}
		// A number or an object under a sensitive name: its JSON text, and
		// every string inside it, since a server may quote either.
		out.redact = append(out.redact, string(raw))
		out.redact = append(out.redact, stringLeaves(raw)...)
	}
	return out, nil
}

// sensitiveFieldName decides which of a database login's fields redactValues
// scrubs from an error. It mirrors rsecret.SensitiveNamePattern
// (backend/resource/rsecret/sensitive.go) character for character — that file
// names this copy, and the two change together.
//
// The gap, stated: a field whose name does not match (a "pw" or a "pin") is not
// scrubbed. Every secret field a database declares today matches it (password,
// privateKey, sshKey, secretAccessKey), and the server does not quote a value in
// an error at all; this is the second line, not the first. An API key's fields
// are every one scrubbed, whatever their names — see readAPIKeyValues.
var sensitiveFieldName = regexp.MustCompile(`(?i)password|pwd|pass|key|secret|token|credential|auth|bearer`)

// stringLeaves returns every string inside a JSON value, at any depth.
func stringLeaves(raw []byte) []string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case map[string]any:
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(v)
	return out
}

// decodeValueObject decodes a JSON object of field → value. The error never
// quotes the input: encoding/json's syntax errors name the offending byte, and
// that byte is part of a credential.
func decodeValueObject(content []byte, fromFile string) (map[string]json.RawMessage, error) {
	where := "stdin"
	if fromFile != "" {
		where = fromFile
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(content, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("%s is not a JSON object of field → value", where)
	}
	if key, ok := duplicateKey(content); ok {
		return nil, fmt.Errorf("%s names the field %q twice — give each field once", where, key)
	}
	return obj, nil
}

// duplicateKey reports a key that appears twice at the top of a JSON object.
//
// encoding/json keeps the LAST value of a repeated key, so every check here
// would see that one — while a database login is sent as the raw bytes, first
// value and all, and the first value would go out never having been seen by
// the redaction list. Refused on both paths rather than reasoned about per
// path. content must already have decoded as an object.
func duplicateKey(content []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(content))
	if _, err := dec.Token(); err != nil { // the opening {
		return "", false
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		key, _ := tok.(string)
		if seen[key] {
			return key, true
		}
		seen[key] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return "", false
		}
	}
	return "", false
}

// looksLikeFieldObject reports content that is a JSON object with field as one
// of its keys — the shape of a multi-field input given to a one-field secret.
func looksLikeFieldObject(content []byte, field string) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(content, &obj) != nil {
		return false
	}
	_, ok := obj[field]
	return ok
}

// stripOneNewline removes exactly one trailing line ending — what `echo` and an
// editor add — and nothing else: leading or inner whitespace may be part of
// the value.
func stripOneNewline(s string) string {
	if strings.HasSuffix(s, "\r\n") {
		return s[:len(s)-2]
	}
	return strings.TrimSuffix(s, "\n")
}

// redactValues scrubs each of values from an error before it is reported.
//
// The server does not echo a credential in an error today. This is the CLI's
// half of that guarantee, so a server that one day quotes the body it refused
// still cannot put a value in a terminal through this command. Both the value
// and its JSON-escaped spelling are scrubbed, because an error body that is not
// the API's {"error": …} shape is reported raw.
func redactValues(err error, values []string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	scrubbed := msg
	for _, v := range values {
		if len(v) < minRedactLen {
			continue
		}
		scrubbed = strings.ReplaceAll(scrubbed, v, "[redacted]")
		if escaped, e := json.Marshal(v); e == nil {
			inner := string(escaped[1 : len(escaped)-1])
			if inner != v {
				scrubbed = strings.ReplaceAll(scrubbed, inner, "[redacted]")
			}
		}
	}
	if scrubbed == msg {
		return err
	}
	return errors.New(scrubbed)
}
