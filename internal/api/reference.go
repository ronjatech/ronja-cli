package api

import (
	"context"
	"net/url"
)

// The by-id reads `ronja sync check` needs to answer one question — "does this
// reference still resolve?" — for the reference kinds nothing else in the CLI
// reads.
//
// ⚠️ None of these is a general client, and none should grow into one:
// discovery belongs on plain HTTP (`ronja api`), and the doctrine that keeps
// this CLI from becoming a permanently incomplete mirror of the API is that a
// COMMAND wraps a loop, never an endpoint. What justifies them is that the edge
// verifier has to ask about an agent, a secret and a note id, and there is no
// other way to ask.
//
// ONLY THE ERROR IS READ. The verifier branches on whether the call succeeded
// and, when it did not, on the status — so the payload types below are
// deliberately the two fields a diagnostic might one day want to quote, not a
// mirror of the row. See Client.GetTable's note: a read gate does not
// distinguish absent from invisible, so a 404 here means "not available to
// you", never "deleted".

// Agent is the sliver of a saved Agent the edge verifier reads.
type Agent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GetAgent reads one saved Agent. Scope: agents:read.
//
// ⚠️ Its two failure shapes DISAGREE about which status they use, and the
// handler says so: an Agent that exists but is not yours answers 404, an id
// matching nothing at all answers 400. Both mean "not available to you", which
// is exactly why the verifier treats the pair identically.
func (c *Client) GetAgent(ctx context.Context, id string) (*Agent, error) {
	var out Agent
	if err := c.Do(ctx, "GET", "agent/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Secret is the sliver of a secret the edge verifier reads.
//
// It carries NO credential material and never will: the read route serves the
// row's metadata, and the values live behind a separate surface this CLI has no
// business touching.
type Secret struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GetSecret reads one secret's metadata. Scope: secrets:read.
func (c *Client) GetSecret(ctx context.Context, id string) (*Secret, error) {
	var out Secret
	if err := c.Do(ctx, "GET", "secret/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Note is the sliver of a note the edge verifier reads.
//
// The BODY is deliberately not decoded. The verifier branches on the error and
// nothing else, and a note's content is the largest thing on this surface — a
// skill note is a document — so decoding it would move a megabyte per edge to
// answer a question the status line already has.
type Note struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GetNote reads one note. Scope: analytics:read — the note group declares it for
// every route, so this is NOT the `notes:read` a reader would guess from the
// path, and a token scoped by guesswork is refused before the row is looked at.
//
// The id may carry either prefix: a note rides `note-` and the legacy `skill-`,
// which is why markers.IsResourceID answers for both and why this takes the
// string as it was written rather than normalising it.
//
// Failure shapes collapse the way GetAgent's do — a note that exists but is not
// visible answers 404, an id matching nothing answers 400 — and the verifier
// treats the pair identically for the reason this file's opening note gives.
func (c *Client) GetNote(ctx context.Context, id string) (*Note, error) {
	var out Note
	if err := c.Do(ctx, "GET", "note/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
