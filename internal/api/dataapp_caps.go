package api

// DeclarableCapabilities are the names an author declares under
// "access"."capabilities": the capabilities Ronja never hands an app on its own.
// The server refuses any name that is neither declarable nor automatic.
//
// The CLI is a separate module and cannot import the server's definition
// (rscripttoken.DataAppDeclarableCaps and DataAppAutoGrantedCaps), so the
// backend's TestTheCLIsCapabilityVocabularyMatchesTheServer reads this file as
// text and fails when either list drifts. Keep each list one literal.
var DeclarableCapabilities = []string{"ai", "embed", "query_external", "write_external", "upload_file"}

// AutoGrantedCapabilities are the names Ronja grants from the app's
// allowlists, or to every app, so a stored copy grants nothing. The server
// drops them on write; Normalized drops them so a file an older CLI wrote still
// compares equal to the row.
var AutoGrantedCapabilities = []string{
	"call_function", "codex", "http_fetch", "list_users", "query_metric",
	"query_ronja", "resource_link", "run_agent", "run_workflow",
}
