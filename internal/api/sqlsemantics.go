package api

// How an artifact's SQL is handed to DuckDB, as the backend reports it on the
// read view of every artifact that carries SQL (tables, workflows, data apps,
// modules).
//
// THE FACT UNDERNEATH. Ronja used to paste customer SQL into generated Python
// as an ordinary (non-raw) string literal, so Python decoded the backslash
// escapes before DuckDB ever saw the text: a file that wrote `'\s'` sent `'\s'`
// to the server and DuckDB received `'s'`. Artifacts born from now on are raw —
// the bytes on disk are the bytes DuckDB runs — and everything that already
// existed is legacy and keeps running through the old embed until its code is
// next edited, because re-interpreting live SQL under new rules would change
// answers nobody asked to change.
//
// READ-ONLY, AND ONLY EVER ONE VALUE. The server reports `"sqlSemantics":
// "legacy"` on a legacy row and omits the key otherwise, and no write route
// accepts it: the first write that changes an artifact's code moves it to raw
// on its own, a metadata-only write moves nothing, and a restore brings back
// whatever the restored version was written under. So the CLI never sends it;
// it only reads it, to print one line per legacy row.
//
// EMPTY is therefore both "raw" and "an instance older than the field", and the
// CLI treats the two alike: it says nothing.
const SQLSemanticsLegacy = "legacy"
