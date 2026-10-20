package api

// SQLSemanticsLegacy is the read view's `sqlSemantics` value for an artifact
// whose SQL still runs through the legacy embed, where Python decodes backslash
// escapes before DuckDB sees the text (`'\s'` arrives as `'s'`); a raw artifact
// runs its bytes as written. The key is read-only (the first write that changes
// an artifact's code moves it to raw) and omitted otherwise, so an empty value
// means raw or an older server, and the CLI says nothing for either.
const SQLSemanticsLegacy = "legacy"
