package api

// Wire-shape coverage for the data-app file write's optimistic-concurrency
// precondition, mirroring workflow_write_test.go's.
//
// The raw BODY is what is asserted rather than a decoded struct, because the
// whole hazard is a *string collapsing to a string: that makes "no
// precondition" and "assert the file does not exist yet" the same bytes, and no
// decode of the sender's own type can see it. On this route that collapse is
// not merely a wrong request — every legacy caller and every in-app agent write
// would start asserting "must not exist" and 409 on its own file.

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

// The DELETE's body is OPTIONAL and sends genuinely nothing when it has nothing
// to say. That is not cosmetic: the previous shape of the route was bodyless,
// and `{}` is a different request for a server that distinguishes an absent
// field from a zero one — `{}` would decode to a nil pointer today, but the
// route's contract is that an unconditional delete looks exactly like it always
// did, and nothing about that should depend on how gt happens to decode an
// empty object.
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
		// Legal but never useful (asserting that what you are deleting does not
		// exist), and it still has to reach the wire as an explicit empty
		// string rather than being dropped by omitempty — one precondition
		// vocabulary covers both verbs, and a client that builds the field the
		// same way for either must not silently mean something different here.
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
