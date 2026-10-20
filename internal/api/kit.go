package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// The data-app component kit's sources are served by the docs routes, not the
// app surface: GET /dataapp/:id/files lists only an app's OWN files, and an app
// file at a kit path shadows the kit's.

// KitIndexPath is the index listing every kit file with a one-line description.
const KitIndexPath = "/docs/api/kit.md"

const kitPathPrefix = "/docs/api/kit/"

// KitDocsPath is the docs route serving one kit file, by its kit-relative path
// ("components/ui/button.tsx").
func KitDocsPath(kitPath string) string {
	return kitPathPrefix + strings.TrimPrefix(kitPath, "/")
}

// GetKitFile returns the verbatim source of one component-kit file,
// unauthenticated: the kit is code shipped in the binary, identical for every
// tenant. A 404 is translated, since the generic /docs/api breadcrumb reads as
// "wrong host" to someone who mistyped a filename.
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

// kitSuggestions is best-effort: the 404 breadcrumb is prose, not a contract,
// so a body it cannot read yields nothing.
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
	// A longer list is a worse copy of the index the message points at.
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}
