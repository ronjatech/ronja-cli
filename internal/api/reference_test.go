package api

import (
	"context"
	"net/http"
	"testing"
)

// GetNote is the arm `ronja sync check` was missing.
//
// `note` is a MARKER-LESS dependency kind — nothing in a folder's source names
// one — so its edges come from a declared reference set rather than from a
// scanned marker. Without a read for it every note-declaring folder scored
// `not_checked`, which is `unknown` for the folder and exit 2 for the tree: a
// verdict nothing the reader does can clear.
func TestGetNoteReadsEitherPrefix(t *testing.T) {
	// A note rides `note-` and the legacy `skill-`, so the id is passed through
	// as written rather than normalised — the server's own
	// ValidateReferenceKind accepts both.
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

// A 404 and a 400 both mean "not available to you" on this surface, and the
// STATUS has to survive the error so the edge verifier can collapse them itself
// rather than guess from the message.
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
