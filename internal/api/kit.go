package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// The read-only data-app component kit, as the docs surface publishes it.
//
// Every data app compiles against an embedded kit (shadcn/ui primitives plus
// Ronja's operator components) as a virtual base layer, and an app file at the
// SAME path shadows the kit's for the whole bundle. So customising a kit
// component is not a mode or a flag: it is having a file of your own at
// "components/ui/button.tsx", after which every existing import resolves to it
// unchanged.
//
// The bytes to start from are not reachable through the app surface —
// GET /dataapp/:id/files lists an app's OWN files, and a kit file is not one of
// them — so they are served from the agent-readable docs routes instead, out of
// the compiler's own embed. Unauthenticated, like /llms.txt and
// /docs/api/endpoints.md, and served verbatim as text/plain.

// KitIndexPath is the index listing every kit file with a one-line description.
const KitIndexPath = "/docs/api/kit.md"

// kitPathPrefix is the route family one kit file's source is served on.
const kitPathPrefix = "/docs/api/kit/"

// KitDocsPath is the docs route serving one kit file, by its kit-relative path
// ("components/ui/button.tsx").
func KitDocsPath(kitPath string) string {
	return kitPathPrefix + strings.TrimPrefix(kitPath, "/")
}

// GetKitFile returns the verbatim source of one component-kit file.
//
// Unauthenticated (GetText attaches no credential), because the docs surface is
// and because the kit is identical for every tenant — it is code shipped in the
// binary, not customer data.
//
// A 404 is translated rather than passed on: the instance answers an unknown
// kit path with the generic /docs/api breadcrumb, which reads as "wrong host"
// to someone who mistyped a filename. Any kit paths the breadcrumb's near-miss
// suggestions name are carried into the message, since a one-letter typo is by
// far the likeliest way to get here.
func (c *Client) GetKitFile(ctx context.Context, kitPath string) (string, error) {
	body, err := c.GetText(ctx, KitDocsPath(kitPath))
	if err != nil {
		if StatusOf(err) != http.StatusNotFound {
			return "", err
		}
		message := fmt.Sprintf("no such file in the built-in component kit: %s", kitPath)
		if near := kitSuggestions(err); len(near) > 0 {
			message += fmt.Sprintf("\n  Did you mean %s?", strings.Join(near, ", "))
		}
		return "", fmt.Errorf("%s\n  The whole kit is listed at %s%s", message, c.BaseURL, KitIndexPath)
	}
	return body, nil
}

// kitSuggestions pulls kit paths out of a 404 breadcrumb's near-miss list.
//
// Best-effort and deliberately dumb: it scans for whitespace-separated tokens
// under the kit route prefix and reports what it finds. The breadcrumb is prose
// meant for humans and agents rather than a contract, so a body it cannot read
// yields nothing at all and the caller still gets a clear message.
func kitSuggestions(err error) []string {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		return nil
	}
	seen := map[string]bool{}
	for _, field := range strings.Fields(apiErr.Body) {
		token := strings.Trim(field, "`\"'(),.;:?!<>[]")
		rest, ok := strings.CutPrefix(token, kitPathPrefix)
		if !ok || rest == "" {
			continue
		}
		seen[rest] = true
	}
	out := make([]string, 0, len(seen))
	for path := range seen {
		out = append(out, path)
	}
	sort.Strings(out)
	// Three is a hint; a longer list is a worse copy of the index, which the
	// message points at on the next line anyway.
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}
