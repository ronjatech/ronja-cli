package markers

import "regexp"

// runModulePathPattern is backend/engine/pymarkers/run_modules.go's
// runModulePathPattern: the file paths the server loads as a module and lets a
// module hold. TestPatternParity keeps the two byte-identical.
var runModulePathPattern = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*/)*[A-Za-z_][A-Za-z0-9_]*\.py$`)

// RunModulePathRule is pymarkers.RunModulePathRule, the rule in words, kept
// word for word by TestRunModulePathRuleParity.
const RunModulePathRule = "file and folder names must be letters, digits and underscores, not starting with a digit, apart from the .py ending"

// IsRunModulePath reports whether path is a .py file whose folder names and file
// name are letters, digits and underscores, none starting with a digit.
func IsRunModulePath(path string) bool {
	return runModulePathPattern.MatchString(path)
}
