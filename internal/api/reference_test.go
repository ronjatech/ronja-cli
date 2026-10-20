package api

import (
	"context"
	"net/http"
	"testing"
)

func TestGetNoteReadsEitherPrefix(t *testing.T) {
	// The id is passed through as written: the server accepts both `note-` and
	// the legacy `skill-` prefix.
	for _, id := range []string{"note-policy", "skill-policy"} {
		var gotPath string
		client := serve(t, func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.Write([]byte(`{"id":"` + id + `","name":"Refund policy"}`))
		})
		note, err := client.GetNote(context.Background(), id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if gotPath != "/api/v2/note/"+id {
			t.Errorf("path = %s", gotPath)
		}
		if note.ID != id || note.Name != "Refund policy" {
			t.Errorf("note = %+v", note)
		}
	}
}

// A 404 and a 400 both mean "not available to you" here; the status must
// survive so the edge verifier can collapse them without reading the message.
func TestGetNoteSurfacesTheStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound} {
		client := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(`{"message":"not found"}`))
		})
		_, err := client.GetNote(context.Background(), "note-gone")
		if err == nil {
			t.Fatalf("a %d must be an error", status)
		}
		if got := StatusOf(err); got != status {
			t.Errorf("StatusOf = %d, want %d", got, status)
		}
	}
}
