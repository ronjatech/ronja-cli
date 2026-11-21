package commands

import (
	"fmt"
	"mime"
	"net/http"
	urlpath "path"
	"strings"
)

// refuseWebAppPage refuses an HTML page outside the API as an answer: the web
// app's own page, or on an error status whatever served the page. --out is an
// explicit request for the file, so it is saved whatever it is, and HEAD has no
// body to print.
func (o apiOutput) refuseWebAppPage(status int, header http.Header, method, path string) error {
	if o.out != "" || method == http.MethodHead {
		return nil
	}
	page, ok := webAppPage(header, path)
	switch {
	case !ok:
		return nil
	case status >= 400:
		return fmt.Errorf("HTTP %d %s %s answered with an HTML page (not printed) — %s", status, method, path, apiPathsHint)
	}
	return fmt.Errorf("%s is a page of the web app, not an API endpoint — %s", page, apiPathsHint)
}

const apiPathsHint = "API paths start with /api/v2/; find the one you need in /docs/api/endpoints.md"

// htmlAnswerPrefixes are the paths whose HTML answer is a real one. Everything
// else that answers HTML is the web app's page, served 200 for any path the app
// router owns.
var htmlAnswerPrefixes = []string{"/api", "/service-api", "/docs"}

// webAppPage reports whether an answer is the web app's HTML page rather than
// an API response, returning the path as the server routed it: query string
// and fragment dropped, dot segments and doubled slashes cleaned.
func webAppPage(header http.Header, path string) (string, bool) {
	mediaType, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || mediaType != "text/html" {
		return "", false
	}
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	path = urlpath.Clean("/" + path)
	if path == "/llms.txt" {
		return "", false
	}
	for _, prefix := range htmlAnswerPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return "", false
		}
	}
	return path, true
}
