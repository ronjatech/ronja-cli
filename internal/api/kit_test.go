package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGetKitFile_ReturnsTheSourceVerbatim: the whole value of the route is that
// what comes back is what the compiler sees, so nothing may be trimmed or
// re-encoded on the way through.
func TestGetKitFile_ReturnsTheSourceVerbatim(t *testing.T) {
	const source = "// Vendored from shadcn/ui.\nexport function Button() {\n\treturn null\n}\n"
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		// Unauthenticated, like the rest of the docs surface.
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("the kit read attached a credential (%q) to a public docs route", auth)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(source))
	}))
	defer server.Close()

	body, err := New(server.URL, "test-token").GetKitFile(context.Background(), "components/ui/button.tsx")
	if err != nil {
		t.Fatalf("GetKitFile: %v", err)
	}
	if body != source {
		t.Errorf("body = %q, want %q", body, source)
	}
	if asked != "/docs/api/kit/components/ui/button.tsx" {
		t.Errorf("asked for %q", asked)
	}
}

// A 404 answers with the API's plain-text breadcrumb, which says nothing about
// the kit. Passed through untranslated it reads as "wrong host"; the point of
// the wrapper is that it reads as "no such file", carrying the breadcrumb's own
// near-miss suggestion.
func TestGetKitFile_NotFoundNamesThePathAndTheIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 page not found\n\nStart at /llms.txt\n" +
			"Did you mean:\n  GET /docs/api/kit/components/ui/button.tsx\n"))
	}))
	defer server.Close()

	_, err := New(server.URL, "").GetKitFile(context.Background(), "components/ui/buton.tsx")
	if err == nil {
		t.Fatal("a kit path that does not exist must be an error")
	}
	for _, want := range []string{
		"component kit",
		"components/ui/buton.tsx",
		"components/ui/button.tsx",
		server.URL + "/docs/api/kit.md",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// The suggestion scan reads prose, not a contract: it must find kit paths in
// whatever punctuation surrounds them, ignore everything else, and stay a hint
// rather than becoming a worse copy of the index.
func TestKitSuggestions(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"nothing to find", "404 page not found\nStart at /llms.txt\n", nil},
		{"other routes are not kit paths", "try `/docs/api/endpoints.md` or /api/v2/dataapp", nil},
		{"punctuation is trimmed", "did you mean `/docs/api/kit/lib/utils.ts`, (/docs/api/kit/styles/theme.ts)?", []string{"lib/utils.ts", "styles/theme.ts"}},
		{"the prefix alone suggests nothing", "see /docs/api/kit/", nil},
		{"capped at three", "/docs/api/kit/a.ts /docs/api/kit/b.ts /docs/api/kit/c.ts /docs/api/kit/d.ts", []string{"a.ts", "b.ts", "c.ts"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := kitSuggestions(&Error{Status: http.StatusNotFound, Body: tc.body})
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("kitSuggestions = %v, want %v", got, tc.want)
			}
		})
	}
}
