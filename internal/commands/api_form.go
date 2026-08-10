package commands

import (
	"bytes"
	"fmt"
	"mime"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
)

// Multipart bodies for `ronja api -F`.
//
// This is the one hole the CLI left in its own reason for existing. `-d` sends
// a JSON blob, which covers nearly every endpoint — but not the upload ones,
// which are multipart/form-data only. Anyone who needed to upload a file was
// therefore sent back to exactly the pattern the CLI was built to eliminate:
//
//	eval "$(ronja env)" && curl -F file=@report.pdf "$RONJA_URL/api/v2/file/upload/..."
//
// which puts a live 90-day credential into a shell variable and an argument
// vector. Closing that is worth a flag.
//
// It does not breach the no-wrapper doctrine, because a content-type is not an
// endpoint. Nothing here knows what /file/upload does, what fields it wants, or
// that it exists; -F is a way of ENCODING a body, in the same category as -d,
// and it needs no maintenance when a route is added.

// buildForm turns repeated -F specs into a multipart body and its content type.
//
// Two spec forms, curl's:
//
//	name=value    a literal field
//	name=@path    a file part, read from disk ("-" reads standard input)
//
// The body is assembled in MEMORY rather than streamed from disk, which is a
// deliberate trade. A streamed body is read once and cannot be replayed, and
// --retry and --wait-until both re-issue the same request — a request whose
// body evaporated after the first attempt would retry as a zero-byte upload,
// which the server would accept. Holding the bytes is what makes those flags
// safe to combine, and maxRequestBody keeps it bounded.
func buildForm(specs []string) ([]byte, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	for _, spec := range specs {
		// FIRST = only: a literal value may perfectly well contain one (a URL,
		// a query fragment), and splitting on all of them would corrupt it.
		name, value, ok := strings.Cut(spec, "=")
		if !ok || name == "" {
			return nil, "", fmt.Errorf("-F takes name=value or name=@path, got %q", spec)
		}

		path, isFile := strings.CutPrefix(value, "@")
		if !isFile {
			if err := writer.WriteField(name, value); err != nil {
				return nil, "", fmt.Errorf("build form field %s: %w", name, err)
			}
			continue
		}
		if err := writeFilePart(writer, name, path); err != nil {
			return nil, "", err
		}
		// Checked per part rather than once at the end so an oversized upload
		// fails on the file that caused it, and before the rest are read.
		if buf.Len() > maxRequestBody {
			return nil, "", fmt.Errorf("the form body is larger than the %d MiB limit — upload it in pieces, or send the bytes directly with -d @%s",
				maxRequestBody>>20, path)
		}
	}

	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("finish form body: %w", err)
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}

// writeFilePart adds one file part, with a real content type on it.
//
// multipart's own CreateFormFile hardcodes application/octet-stream, which is
// not a cosmetic difference here: Ronja stores the part's declared type on the
// file row, and a PDF or PNG recorded as octet-stream is a file the product
// then cannot preview or extract text from. The type is guessed from the
// extension — the same thing every browser does for a file input — and falls
// back to octet-stream only when there is genuinely nothing to go on.
func writeFilePart(writer *multipart.Writer, name, path string) error {
	var (
		body     []byte
		err      error
		filename string
	)
	if path == "-" {
		// Named after the field, because a part with no filename is a plain
		// field to most servers and would be silently dropped by an upload
		// handler looking for a file.
		filename = name
		body, err = readStdinLimit("-F "+name+"=@-", maxRequestBody)
	} else {
		filename = filepath.Base(path)
		body, err = readBodyFile(path)
	}
	if err != nil {
		return err
	}

	contentType := mime.TypeByExtension(filepath.Ext(filename))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
		escapeFormValue(name), escapeFormValue(filename)))
	header.Set("Content-Type", contentType)

	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("build form file %s: %w", name, err)
	}
	if _, err := part.Write(body); err != nil {
		return fmt.Errorf("build form file %s: %w", name, err)
	}
	return nil
}

// escapeFormValue quotes a name or filename for a Content-Disposition header.
//
// Same escaping mime/multipart applies internally, and applied for the same
// reason: a filename containing a quote or a newline would otherwise terminate
// the header early and let the rest of the name be read as further headers.
// Local filenames are attacker-controlled often enough (anything downloaded,
// anything from a shared drive) that this is not theoretical.
var formValueEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"", "\r", "", "\n", "")

func escapeFormValue(s string) string { return formValueEscaper.Replace(s) }

// checkStdinSources refuses more than one reader of standard input.
//
// A pipe can be drained once. Two things reading it — `-d @-` alongside a
// `-F x=@-`, or two `-F`s both naming `@-` — means the first gets the content
// and every one after it gets EOF, so the request goes out carrying a
// zero-byte part. Nothing about that fails: the server accepts an empty file,
// and the caller finds out much later that they uploaded nothing. Refusing up
// front is the only point at which this is diagnosable.
//
// data is the raw -d value so the message can name both halves of the clash.
func checkStdinSources(data string, specs []string) error {
	var sources []string
	if data == "@-" {
		sources = append(sources, "-d @-")
	}
	for _, spec := range specs {
		name, value, ok := strings.Cut(spec, "=")
		if ok && value == "@-" {
			sources = append(sources, "-F "+name+"=@-")
		}
	}
	if len(sources) > 1 {
		return fmt.Errorf("%s all read standard input, and only the first would get anything — a pipe can be drained once, so the rest would be sent as empty",
			strings.Join(sources, ", "))
	}
	return nil
}

// formContentType decides what Content-Type a multipart request goes out with.
//
// The boundary is GENERATED here, so a caller-supplied one is a guarantee of
// failure rather than an override: the header would name a boundary the body
// does not use, and a server parsing that finds zero parts and reports
// success. Silent, and indistinguishable from an endpoint that ignored the
// upload.
//
// So a supplied multipart type keeps its SUBTYPE (multipart/related and
// friends are legitimate) and every other parameter, with the boundary
// replaced by the real one. A supplied NON-multipart type is refused outright:
// the body demonstrably is multipart, and honouring the header would have the
// server parse it as something it is not.
func formContentType(supplied, generated string) (string, error) {
	if supplied == "" {
		return generated, nil
	}
	mediaType, params, err := mime.ParseMediaType(supplied)
	if err != nil {
		return "", fmt.Errorf("cannot read the Content-Type given with -H (%q): %w", supplied, err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return "", fmt.Errorf("-F builds a multipart body, but -H declared Content-Type %q — drop the header, or send the bytes yourself with -d",
			mediaType)
	}
	_, generatedParams, err := mime.ParseMediaType(generated)
	if err != nil {
		// Cannot happen: `generated` is mime/multipart's own output. Falling
		// back to it whole is still correct, just less faithful to the -H.
		return generated, nil //nolint:nilerr // the generated type is always usable
	}
	if params == nil {
		params = map[string]string{}
	}
	params["boundary"] = generatedParams["boundary"]
	return mime.FormatMediaType(mediaType, params), nil
}

// checkFormPaths reports a -F file that does not exist BEFORE anything is sent.
//
// Not merely a nicer message: without it the first file is read, the body is
// assembled, and the failure arrives after the request has already been shaped
// — and with --retry set, after the retry budget has been spent on a request
// that could never have succeeded.
func checkFormPaths(specs []string) error {
	for _, spec := range specs {
		_, value, ok := strings.Cut(spec, "=")
		if !ok {
			continue
		}
		path, isFile := strings.CutPrefix(value, "@")
		if !isFile || path == "-" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("-F %s: %w", spec, err)
		}
	}
	return nil
}
