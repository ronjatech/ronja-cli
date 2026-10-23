package commands

import (
	"regexp"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A model of POST /workflow/validate's `writeTargets` as the server
// (backend/resource/rworkflow/validate_write_targets.go) computes it — FROM THE POSTED FILES, never from anything stored —
// for the fakes the write gate is tested against. Each rule cites the line it
// mirrors:
//
//   - only `.py` sources are scanned (validate_write_targets_test.go :16-20,
//     "a non-.py file contributes nothing"), with `#` comment lines skipped as
//     dead text (pymarkers/sites.go liveView :126-130; this model drops whole
//     comment lines only, which every fixture here respects);
//   - per file, families in siteKindOrder (sites.go :258-261): write markers,
//     output calls, codex markers, file markers, save-folder literals;
//   - a `{{ write('X') }}` is a write_marker table target (validate_write_
//     targets.go :120-135); an X that is not table-shaped is a NAME FORM with no
//     targetID (:130-133, Site.Binds = rrn.IsTable, sites.go :79-83);
//   - an output call is tools.<verb>( for the six verbs (extract.go
//     outputCallNames :11-18), its table argument the `tableID=` or `name=`
//     keyword, else the SECOND positional string (extract.go pickTableIDArg
//     :237-245) — `tools.replaceTable(df, "table-x")`; position output_literal
//     with the verb in `call` (validate_write_targets.go :127-129);
//   - a `{{ codex('cdx-…') }}` that binds is a read_write codex target
//     (:136-147); one without the cdx- prefix is malformed and not listed;
//   - a write-mode `{{ file('p', 'write') }}` and a saveFile/saveTo* folder=
//     literal are folder targets carrying the normalized path and no id
//     (:148-158, Site.FolderWriteScope sites.go :113-125, NormalizeFolder
//     folders.go :384-404); the root and a `..` path declare nothing;
//   - a table or codex id is resolved against the caller's reach: readable
//     ones carry targetFeatureID, targetName, targetTableKind and — for a
//     workflow's output — ownerWorkflowID and logicalName (:183-219); an
//     unreadable or unknown one keeps only its position and id (:56-63).

// fakeWriteRow is what a fake knows about one table or codex the caller can
// read.
type fakeWriteRow struct {
	FeatureID       string
	Name            string
	TableKind       string
	OwnerWorkflowID string
	LogicalName     string
}

var (
	fakeOutputCallPattern = regexp.MustCompile(`(^|[^A-Za-z0-9_])tools\.(replaceTable|appendToTable|replaceTableFromDir|appendToTableFromDir|saveAsTable|saveAsTableFromDir)\(`)
	fakeFileMarkerPattern = regexp.MustCompile(`\{\{+\s*file\(\s*['"]([^'"]+)['"]\s*,\s*['"]([^'"]+)['"]\s*\)\s*\}\}+`)
	fakeSaveFolderPattern = regexp.MustCompile(`(^|[^A-Za-z0-9_])(saveFile|saveTo[A-Za-z]+)\(`)
	fakeStringLiteral     = regexp.MustCompile(`^\s*(?:"([^"]*)"|'([^']*)')\s*$`)
)

// fakeWriteTargets computes writeTargets for posted files. rows answers a
// readable (kind, id); nil reads nothing.
func fakeWriteTargets(files []api.ValidateFile, rows func(kind, id string) (fakeWriteRow, bool)) []api.ValidateWriteTarget {
	out := []api.ValidateWriteTarget{}
	for _, file := range files {
		if !strings.HasSuffix(file.Path, ".py") {
			continue
		}
		lines := strings.Split(file.Content, "\n")
		live := func(i int) bool { return !strings.HasPrefix(strings.TrimSpace(lines[i]), "#") }
		// write markers
		for i := range lines {
			if !live(i) {
				continue
			}
			for _, o := range markers.ScanWrites(lines[i]) {
				t := api.ValidateWriteTarget{Position: api.WritePositionMarker, TargetKind: api.WriteTargetTable,
					Direction: "write", Path: file.Path, Line: i + 1, Arg: o.Arg}
				if markers.IsResourceID(markers.KindTable, o.Arg) {
					t.TargetID = o.Arg
				} else {
					t.NameForm = true
				}
				out = append(out, t)
			}
		}
		// output calls
		for i := range lines {
			if !live(i) {
				continue
			}
			for _, loc := range fakeOutputCallPattern.FindAllStringSubmatchIndex(lines[i], -1) {
				verb := lines[i][loc[4]:loc[5]]
				args, ok := fakeCallArgs(lines[i][loc[1]:])
				if !ok {
					continue
				}
				arg, ok := fakePickTableArg(args)
				if !ok {
					continue
				}
				t := api.ValidateWriteTarget{Position: api.WritePositionOutputLiteral, TargetKind: api.WriteTargetTable,
					Direction: "write", Path: file.Path, Line: i + 1, Call: verb, Arg: arg}
				if markers.IsResourceID(markers.KindTable, arg) {
					t.TargetID = arg
				} else {
					t.NameForm = true
				}
				out = append(out, t)
			}
		}
		// codex markers
		for i := range lines {
			if !live(i) {
				continue
			}
			for _, o := range markers.Scan(lines[i]) {
				if o.Family.Kind != markers.KindCodex || !markers.IsResourceID(markers.KindCodex, o.Arg) {
					continue
				}
				out = append(out, api.ValidateWriteTarget{Position: api.WritePositionCodexMarker, TargetKind: api.WriteTargetCodex,
					Direction: "read_write", Path: file.Path, Line: i + 1, Arg: o.Arg, TargetID: o.Arg})
			}
		}
		// file markers (write mode)
		for i := range lines {
			if !live(i) {
				continue
			}
			for _, m := range fakeFileMarkerPattern.FindAllStringSubmatch(lines[i], -1) {
				if strings.ToLower(strings.TrimSpace(m[2])) != "write" {
					continue
				}
				arg := strings.TrimSpace(m[1])
				norm, ok := wfdir.NormalizeFolderPath(arg)
				if !ok || norm == "" {
					continue
				}
				out = append(out, api.ValidateWriteTarget{Position: api.WritePositionFileMarker, TargetKind: api.WriteTargetFolder,
					Direction: "write", Path: file.Path, Line: i + 1, Arg: arg, TargetPath: norm})
			}
		}
		// save-folder literals
		for i := range lines {
			if !live(i) {
				continue
			}
			for _, loc := range fakeSaveFolderPattern.FindAllStringSubmatchIndex(lines[i], -1) {
				verb := lines[i][loc[4]:loc[5]]
				args, ok := fakeCallArgs(lines[i][loc[1]:])
				if !ok {
					continue
				}
				arg, ok := fakeKwarg(args, "folder")
				if !ok {
					continue
				}
				norm, ok := wfdir.NormalizeFolderPath(arg)
				if !ok || norm == "" {
					continue
				}
				out = append(out, api.ValidateWriteTarget{Position: api.WritePositionFolderLiteral, TargetKind: api.WriteTargetFolder,
					Direction: "write", Path: file.Path, Line: i + 1, Call: verb, Arg: arg, TargetPath: norm})
			}
		}
	}
	if rows == nil {
		return out
	}
	for i := range out {
		t := &out[i]
		if t.TargetID == "" {
			continue
		}
		row, ok := rows(t.TargetKind, t.TargetID)
		if !ok {
			continue
		}
		t.Readable = true
		t.TargetFeatureID, t.TargetName = row.FeatureID, row.Name
		if t.TargetKind == api.WriteTargetTable {
			t.TargetTableKind = row.TableKind
			if row.LogicalName != "" {
				t.OwnerWorkflowID, t.LogicalName = row.OwnerWorkflowID, row.LogicalName
			}
		}
	}
	return out
}

// fakeCallArgs is the argument list of a call whose open paren has just been
// consumed, up to its matching close paren on the same line.
func fakeCallArgs(rest string) (string, bool) {
	depth := 1
	var quote byte
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
			if depth == 0 {
				return rest[:i], true
			}
		}
	}
	return "", false
}

// fakeSplitArgs splits an argument list at top-level commas.
func fakeSplitArgs(args string) []string {
	var out []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(args); i++ {
		c := args[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		case c == ',' && depth == 0:
			out = append(out, args[start:i])
			start = i + 1
		}
	}
	return append(out, args[start:])
}

func fakeStringOf(s string) (string, bool) {
	m := fakeStringLiteral.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	if m[1] != "" {
		return m[1], true
	}
	return m[2], m[2] != ""
}

func fakeKwarg(args, key string) (string, bool) {
	for _, part := range fakeSplitArgs(args) {
		name, value, found := strings.Cut(part, "=")
		if !found || strings.TrimSpace(name) != key {
			continue
		}
		return fakeStringOf(value)
	}
	return "", false
}

func fakePickTableArg(args string) (string, bool) {
	if v, ok := fakeKwarg(args, "tableID"); ok {
		return v, true
	}
	if v, ok := fakeKwarg(args, "name"); ok {
		return v, true
	}
	var positional []string
	for _, part := range fakeSplitArgs(args) {
		if strings.Contains(part, "=") && !strings.ContainsAny(strings.SplitN(part, "=", 2)[0], `"'`) {
			continue
		}
		positional = append(positional, part)
	}
	if len(positional) < 2 {
		return "", false
	}
	return fakeStringOf(positional[1])
}
