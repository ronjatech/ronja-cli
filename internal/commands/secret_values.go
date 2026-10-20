package commands

// The value half of `ronja secret create`. Every line here keeps one rule: no
// value reaches stdout, stderr, an error message or argv.

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

// readPassword is a package var so a test can pin that a prompted value goes
// into the body and nowhere else.
var readPassword = func(fd int) ([]byte, error) { return term.ReadPassword(fd) }

// maxSecretInput is far past any real credential and small enough that a
// misdirected pipe fails rather than being held whole and sent.
const maxSecretInput = 1 << 20

// minRedactLen is the shortest value redactValues scrubs: a shorter one would
// match inside ordinary words of the server's message.
const minRedactLen = 4

type secretValues struct {
	body    json.RawMessage
	lengths []fieldLength
	redact  []string
}

// fieldLength is the one thing about a value that is safe to print.
type fieldLength struct {
	Name   string `json:"name"`
	Length int    `json:"length"`
}

// readSecretInput's ok is false when stdin is a terminal and no file was named:
// the prompting case.
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
	// Not readStdinLimit: its "write it to a file" remedy is wrong here, since
	// --from-file has the same cap.
	body, over, err := readLimit(os.Stdin, maxSecretInput)
	if err != nil {
		return nil, false, fmt.Errorf("read standard input: %w", err)
	}
	if over {
		return nil, false, fmt.Errorf("standard input carried more than %d MiB, which no credential is — is that the right input?", maxSecretInput>>20)
	}
	return body, true, nil
}

// emptyInputError is a refusal, never a prompt: an agent with an empty stdin
// must hear that it supplied nothing rather than wait on a question.
func emptyInputError(fromFile string) error {
	if fromFile != "" {
		return fmt.Errorf("%s is empty — there is no value to store", fromFile)
	}
	return errors.New("no value on stdin — pipe one in, or pass --in-browser to enter it yourself in Ronja")
}

var errPromptCancelled = errors.New("cancelled; nothing was stored")

// readHidden is readPassword that gives way to ctx. signalContext turns Ctrl-C
// into a cancelled context, which term.ReadPassword ignores, so the read runs on
// a goroutine left blocked on stdin until exit, and the terminal's echo is
// restored here. A done context wins even over a read that also finished.
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

func readAPIKeyValues(ctx context.Context, fieldNames []string, fromFile string) (secretValues, error) {
	content, ok, err := readSecretInput(fromFile)
	if err != nil {
		return secretValues{}, err
	}

	values := map[string]string{}
	switch {
	case !ok:
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
			// A warning, not a refusal: a service account's key file is a
			// legitimate JSON value. It never quotes a byte of the content.
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

// readDialectValues checks only that a database login is a non-empty JSON
// object; the server, which holds each database's field list, checks the fields.
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

		// Only a sensitive field is scrubbed: a username of "postgres" would
		// otherwise redact the words of the server's own refusal.
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

// sensitiveFieldName decides which database-login fields redactValues scrubs.
// It must equal rsecret.SensitiveNamePattern character for character; the
// backend's TestCLISensitiveNamePatternMatchesTheBackend fails when they differ.
// A non-matching name ("pw", "pin") is not scrubbed: this is the second line of
// defence, since the server never quotes a value in an error.
var sensitiveFieldName = regexp.MustCompile(`(?i)password|pwd|pass|key|secret|token|credential|auth|bearer`)

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

// decodeValueObject's error never quotes the input: encoding/json's syntax
// errors name the offending byte, which is part of a credential.
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

// duplicateKey exists because encoding/json keeps the LAST value of a repeated
// key while a database login is sent as raw bytes, so the first value would go
// out unseen by the redaction list. content must already decode as an object.
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

func looksLikeFieldObject(content []byte, field string) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(content, &obj) != nil {
		return false
	}
	_, ok := obj[field]
	return ok
}

// stripOneNewline removes exactly one trailing line ending: any other
// whitespace may be part of the value.
func stripOneNewline(s string) string {
	if strings.HasSuffix(s, "\r\n") {
		return s[:len(s)-2]
	}
	return strings.TrimSuffix(s, "\n")
}

// redactValues is the CLI's half of the guarantee that no credential reaches a
// terminal, should the server ever quote a refused body. The JSON-escaped
// spelling is scrubbed too, because a non-API error body is reported raw.
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
