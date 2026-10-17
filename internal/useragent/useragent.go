// Package useragent is the CLI's one User-Agent format, shared by the Ronja API
// client (internal/api) and the release fetch (internal/update).
//
// A leaf on purpose: internal/update must never depend on internal/api, the
// credential-attaching client (see update's imports test), so the format both
// send lives below both.
package useragent

import "strings"

// Product is the bare product token. Sent alone it means "no version known":
// what every CLI built before the version was added sends to an instance, and
// what the release download sends.
const Product = "ronja-cli"

// For is the User-Agent of a CLI at version: "ronja-cli/v0.26.2", or
// "ronja-cli/dev" for an unstamped build.
//
// The format is a contract with the server, which parses `ronja-cli/<version>`
// to count CLI adoption before it changes a default an older CLI cannot follow,
// and reads a bare Product as older than every versioned build. A version
// carrying anything a header product token cannot hold (it only ever comes from
// the build, but it is still text) degrades to the bare Product rather than to
// a malformed header.
func For(version string) string {
	return withVersion(Product, version)
}

// APIProduct is the product token of a request `ronja api` passes through — a
// request somebody wrote by hand rather than one a CLI command built.
const APIProduct = "ronja-cli-api"

// ForAPI is the User-Agent of `ronja api` at version: "ronja-cli-api/v0.26.2".
//
// Its own product token, not For's, because the server tells the two apart: a
// workflow created by `wf push` was created on the runtime the CLI pinned, and
// one created by `ronja api POST /api/v2/workflow` was created on whatever the
// author wrote. Same version rule as For.
func ForAPI(version string) string {
	return withVersion(APIProduct, version)
}

func withVersion(product, version string) string {
	if version == "" || strings.ContainsAny(version, " \t\r\n/()") {
		return product
	}
	return product + "/" + version
}
