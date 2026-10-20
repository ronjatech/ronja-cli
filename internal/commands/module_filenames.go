package commands

import (
	"fmt"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/markers"
)

// moduleFileNameProblem is the ONE wording of the refusal, shared by `module
// validate` (every file) and `module push` (the first it writes), and word for
// word the server's (rmodule requirePythonFile). It is "" for a file the server
// accepts and for a non-.py file, which checkPushable refuses on its own.
func moduleFileNameProblem(path string) string {
	if !strings.HasSuffix(path, ".py") || markers.IsRunModulePath(path) {
		return ""
	}
	return fmt.Sprintf("module file %q cannot be imported: %s. Rename it to %q",
		path, markers.RunModulePathRule, suggestModuleFilePath(path))
}

func suggestModuleFilePath(path string) string {
	segs := strings.Split(strings.TrimSuffix(path, ".py"), "/")
	for i, seg := range segs {
		b := []byte(seg)
		for j, c := range b {
			if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
				b[j] = '_'
			}
		}
		if len(b) == 0 || b[0] >= '0' && b[0] <= '9' {
			b = append([]byte{'_'}, b...)
		}
		segs[i] = string(b)
	}
	return strings.Join(segs, "/") + ".py"
}

func refuseModuleFileNames(files map[string]string) error {
	for _, path := range sortedPaths(files) {
		if problem := moduleFileNameProblem(path); problem != "" {
			return fmt.Errorf("%s.\n  Nothing was pushed; `ronja module validate` lists every file name that breaks the rule", problem)
		}
	}
	return nil
}
