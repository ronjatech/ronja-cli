package api

// The raw BODY is asserted, not a decoded struct: a *string collapsing to a
// string makes "no precondition" and "must not exist yet" the same bytes, and
// every legacy write would 409 on its own file.

import (
	"context"
	"io"
	"net/http"
	"testing"
)

// The precondition's THREE states have to survive the encoder distinctly.
func TestPutDataAppFileEncodesThePreconditionsThreeStates(t *testing.T) {
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	empty := ""
	for _, tc := range []struct {
		name string
		want string
		base *string
	}{
		{"absent", `{"content":"export default App"}`, nil},
		{"must not exist", `{"content":"export default App","baseSha256":""}`, &empty},
		{"digest", `{"content":"export default App","baseSha256":"` + digest + `"}`, &digest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			client := serve(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				got = string(body)
				_, _ = w.Write([]byte(`{"id":"dataappfile-1","dataAppID":"data_app-1","path":"App.tsx","content":"export default App"}`))
			})
			if _, err := client.PutDataAppFile(context.Background(), "data_app-1", "App.tsx", "export default App", tc.base); err != nil {
				t.Fatalf("put: %v", err)
			}
			if got != tc.want {
				t.Errorf("body = %s, want %s", got, tc.want)
			}
		})
	}
}

// An unconditional DELETE sends no body at all, never `{}`, so it does not
// depend on how gt decodes an empty object.
func TestDeleteDataAppFileOmitsTheBodyEntirelyWithoutAPrecondition(t *testing.T) {
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	empty := ""
	for _, tc := range []struct {
		name string
		base *string
		want string
	}{
		{"without a precondition", nil, ""},
		{"with a digest", &digest, `{"baseSha256":"` + digest + `"}`},
		// Never useful, but one precondition vocabulary covers both verbs, so
		// the empty string must still reach the wire.
		{"with the must-not-exist assertion", &empty, `{"baseSha256":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			client := serve(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				got = string(body)
				_, _ = w.Write([]byte(`{"dataAppID":"data_app-1"}`))
			})
			if _, err := client.DeleteDataAppFile(context.Background(), "data_app-1", "old.tsx", tc.base); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if got != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
		})
	}
}
