package commands

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
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

// formBody is a multipart body as an ordered list of segments.
//
// A segment is either a slice of bytes the CLI generated — part headers, plain
// fields, a piped part that had to be buffered, the closing boundary — or a
// reference to a file on disk. Open concatenates them; Len is the sum of the
// literal lengths and the stat'ed file sizes, which is exact, so the request
// carries a real Content-Length and the file bytes never enter memory.
//
// The boundary is generated ONCE, when the segments are built, and is baked
// into the literals. Every Open therefore produces byte-for-byte the same body
// — which is what a retry, and the Content-Type header the command already
// sent, both require.
type formBody struct {
	segments []formSegment
	size     int64
}

// formSegment is one piece of the body: literal bytes, or a file to stream.
type formSegment struct {
	literal []byte
	path    string    // empty for a literal segment
	size    int64     // the file's size at build time
	modTime time.Time // the file's modification time when size was measured
}

func (f formBody) Len() int64 { return f.size }

// Open returns a reader over the whole body, opening each file part LAZILY.
//
// Eagerly opening every part held one descriptor per file for the whole
// request, which is a real ceiling on a form assembled in a loop: a hundred
// parts is a hundred handles held while the slowest of them is uploaded, and
// nothing about the failure when they run out names the form. One at a time
// costs nothing — the parts are read strictly in order anyway — and the handle
// is closed at EOF, before the next one is opened.
//
// The cost of the laziness is WHEN an unopenable part is reported: at read
// time, mid-request, rather than before the request is shaped. That is why
// checkFormPaths exists and runs first, so the ordinary case (a path that is
// wrong, or gone) is still caught up front and costs nothing.
func (f formBody) Open() (io.ReadCloser, error) {
	return &formReader{segments: f.segments}, nil
}

// formReader walks a form body's segments, holding at most ONE open file.
type formReader struct {
	segments []formSegment
	next     int
	// current is the segment being read; open is the file behind it, or nil
	// when the segment is literal bytes.
	current io.Reader
	open    *os.File
	closed  bool
}

func (r *formReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, os.ErrClosed
	}
	for {
		if r.current == nil {
			if r.next >= len(r.segments) {
				return 0, io.EOF
			}
			seg := r.segments[r.next]
			r.next++
			if seg.path == "" {
				r.current = bytes.NewReader(seg.literal)
				continue
			}
			file, err := os.Open(seg.path)
			if err != nil {
				return 0, err
			}
			r.open, r.current = file, file
			continue
		}
		n, err := r.current.Read(p)
		if err == io.EOF {
			// The segment is done: its descriptor is released here rather than
			// at Close, which is the whole point of opening lazily. A close
			// error on a file that has just been read to the end says nothing
			// about the bytes already handed over, so it is not allowed to fail
			// an upload that is otherwise complete.
			r.closeCurrent()
			if n > 0 {
				return n, nil
			}
			continue
		}
		if n > 0 || err != nil {
			return n, err
		}
		// A reader may legally return (0, nil); ask it again rather than
		// reporting an EOF it did not give.
	}
}

// Close releases the one descriptor that may still be open — the part being
// read when the request was abandoned. Everything before it was closed at its
// own EOF.
func (r *formReader) Close() error {
	r.closed = true
	if r.open == nil {
		return nil
	}
	err := r.open.Close()
	r.open, r.current = nil, nil
	return err
}

func (r *formReader) closeCurrent() {
	if r.open != nil {
		_ = r.open.Close()
		r.open = nil
	}
	r.current = nil
}

func (f formBody) paths() []sizedPath {
	var paths []sizedPath
	for _, seg := range f.segments {
		if seg.path != "" {
			paths = append(paths, sizedPath{path: seg.path, size: seg.size, modTime: seg.modTime})
		}
	}
	return paths
}

// buildForm turns repeated -F specs into a multipart body and its content type.
//
// Two spec forms, curl's:
//
//	name=value    a literal field
//	name=@path    a file part, read from disk ("-" reads standard input)
//
// File parts are STREAMED: the multipart framing around them is generated here,
// the bytes are not. That is what lets an upload be as large as the instance
// accepts rather than as large as this process can hold. Replay still works,
// because a segmented body can be re-opened as often as --retry and
// --wait-until need it — see formBody. The exceptions are the parts that cannot
// be re-opened to the same bytes at all: a piped part (`@-`), and a path that
// is readable but not a regular file. Both are buffered — see
// classifyBodyPath.
func buildForm(specs []string) (api.BodySource, string, error) {
	var (
		buf    bytes.Buffer
		body   formBody
		writer = multipart.NewWriter(&buf)
	)
	// flush turns everything the multipart writer has produced since the last
	// call into one literal segment. Called immediately after a file part's
	// headers are written, so the file's bytes can be spliced in behind them.
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		literal := bytes.Clone(buf.Bytes())
		body.segments = append(body.segments, formSegment{literal: literal})
		body.size += int64(len(literal))
		buf.Reset()
	}

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
		seg, err := writeFilePart(writer, name, path)
		if err != nil {
			return nil, "", err
		}
		if seg != nil {
			flush()
			body.segments = append(body.segments, *seg)
			body.size += seg.size
		}
	}

	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("finish form body: %w", err)
	}
	flush()
	return body, writer.FormDataContentType(), nil
}

// writeFilePart adds one file part, with a real content type on it.
//
// multipart's own CreateFormFile hardcodes application/octet-stream, which is
// not a cosmetic difference here: Ronja stores the part's declared type on the
// file row, and a PDF or PNG recorded as octet-stream is a file the product
// then cannot preview or extract text from. The type is guessed from the
// extension — the same thing every browser does for a file input — and falls
// back to octet-stream only when there is genuinely nothing to go on.
//
// It returns the segment the caller must splice in after the part's headers, or
// nil when the content was buffered — a piped part, or a path that is readable
// but not a regular file — and has already been written through the writer.
func writeFilePart(writer *multipart.Writer, name, path string) (*formSegment, error) {
	var (
		buffered []byte // the content, when it had to be read rather than streamed
		streamed bool
		size     int64
		modTime  time.Time
		filename string
	)
	switch {
	case path == "-":
		// Named after the field, because a part with no filename is a plain
		// field to most servers and would be silently dropped by an upload
		// handler looking for a file.
		filename = name
		content, err := readStdinLimit("-F "+name+"=@-", "-F", maxStdinBody)
		if err != nil {
			return nil, err
		}
		buffered = content
	default:
		filename = filepath.Base(path)
		what := "-F " + name + "=@" + path
		kind, fi, err := classifyBodyPath(what, path)
		if err != nil {
			return nil, err
		}
		if kind == pathRegular {
			streamed, size, modTime = true, fi.Size(), fi.ModTime()
			break
		}
		// Not a regular file, but readable: buffered like a pipe, for the
		// reasons classifyBodyPath sets out.
		content, err := readUnseekablePath(what, "-F", path)
		if err != nil {
			return nil, err
		}
		buffered = content
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
		return nil, fmt.Errorf("build form file %s: %w", name, err)
	}
	if streamed {
		return &formSegment{path: path, size: size, modTime: modTime}, nil
	}
	if _, err := part.Write(buffered); err != nil {
		return nil, fmt.Errorf("build form file %s: %w", name, err)
	}
	return nil, nil
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

// checkFormPaths reports a -F file that does not exist, or is a directory,
// BEFORE anything is sent.
//
// Not merely a nicer message: without it the first file is read, the body is
// assembled, and the failure arrives after the request has already been shaped
// — and with --retry set, after the retry budget has been spent on a request
// that could never have succeeded. Since formBody opens its parts lazily, it is
// also the only thing that catches a bad path before the request is in flight.
//
// It stats and nothing more: a non-regular path is legitimate here (it is
// buffered when the body is built), and reading it to find that out would drain
// the very thing the build is about to read.
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
		if _, _, err := classifyBodyPath("-F "+spec, path); err != nil {
			return err
		}
	}
	return nil
}
