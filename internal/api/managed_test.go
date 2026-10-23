package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The server renders an error's details at the TOP LEVEL of the body
// (gt.ErrorHandler), so the holder of a claimed address is `claim`, beside
// `error` and `code` — not inside a `details` object.
func TestAsAddressClaimedReadsTheTopLevelClaim(t *testing.T) {
	err := &Error{Status: 409, Body: `{"error":"taken","code":"address_claimed","featureID":"f","folderID":"x","address":"","claim":{"id":"managed-1","state":"ok","resourceID":"wf-1","readable":true},"resourceID":"wf-1"}`}
	c, ok := AsAddressClaimed(err)
	if !ok || c == nil || c.ResourceID != "wf-1" || c.State != ManagedStateOK {
		t.Fatalf("claim = %+v, ok = %v", c, ok)
	}
	if _, ok := AsAddressClaimed(&Error{Status: 409, Body: `{"error":"x","code":"name_taken"}`}); ok {
		t.Fatal("a name_taken read as address_claimed")
	}
	redacted, ok := AsAddressClaimed(&Error{Status: 409, Body: `{"error":"x","code":"address_claimed","claim":{"id":"managed-1","state":"not_readable"}}`})
	if !ok || redacted.ResourceID != "" || redacted.State != ManagedStateNotReadable {
		t.Fatalf("redacted = %+v", redacted)
	}
}

func TestListManagedSendsFeatureAndFolder(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Path + "?" + r.URL.RawQuery
		_, _ = w.Write([]byte(`{"claims":[{"id":"managed-1","address":"orders.sql","state":"lost","resourceID":"table-1"}]}`))
	}))
	defer server.Close()
	claims, err := New(server.URL, "t").ListManaged(context.Background(), "feat-1", "0b6f7c2e-3a41-4f0e-9d4c-7a5e2b9c1d08")
	if err != nil {
		t.Fatal(err)
	}
	if query != "/api/v2/managed?feature=feat-1&folder=0b6f7c2e-3a41-4f0e-9d4c-7a5e2b9c1d08" {
		t.Fatalf("query = %s", query)
	}
	if len(claims) != 1 || claims[0].State != ManagedStateLost || claims[0].ResourceID != "table-1" {
		t.Fatalf("claims = %+v", claims)
	}
}

// A tombstone holding an address arrives as the top-level claim with state
// moved_out and departedAt; no resourceID when its row is not readable.
func TestAsAddressClaimedReadsATombstone(t *testing.T) {
	c, ok := AsAddressClaimed(&Error{Status: 409, Body: `{"error":"x","code":"address_claimed","claim":{"id":"managed-1","state":"moved_out","departedAt":"2026-10-04T08:00:00Z","readable":false}}`})
	if !ok || !c.Departed() || c.State != ManagedStateMovedOut || c.ResourceID != "" {
		t.Fatalf("claim = %+v", c)
	}
	var live *ManagedClaim
	if live.Departed() {
		t.Fatal("a nil claim read as a tombstone")
	}
}
