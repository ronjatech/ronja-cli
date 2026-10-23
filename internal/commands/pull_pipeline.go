package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/checkfile"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/metricfile"
	"github.com/ronjatech/ronja-cli/internal/tabledocs"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `ronja pipeline pull`: every table and metric this folder manages on the
// stack, read live and written back into the folder — and, beside them, the
// checks files of those tables and the docs sidecars of the tables the folder
// documents. Each kind of file keeps the shape its author gave it:
//
//   - a .sql file is the live table's SQL in this folder's spelling — its own
//     siblings by stem, declared aliases by alias, anything else as the literal
//     id (reported). Its opt-in header is part of that SQL; a description,
//     column note or name changed in the web app does not change the SQL, so
//     each value the header DECLARES is rewritten to what the row now says
//     (tabledocs.RewriteHeader) — and a header that changed is SQL the live
//     row does not hold yet, which the next deploy publishes (reported as
//     pending). A value the header does not declare is not added.
//   - a metric file is the live recipe, its source spelled as the file spells
//     it when that still names the live table; "description",
//     "reportingTimezone", "name" and "tags" are rewritten only where the file
//     declares them.
//   - a checks file keeps its entries; each one a live check resolves to
//     takes that check's body and the fields the entry declares, one whose
//     check is gone (it was the folder's, and somebody deleted it) is dropped,
//     and one never applied is left for the next deploy.
//   - a docs sidecar takes the row's prose for what it declares.
//
// A claimed address whose file is gone is written back; a file the server holds
// no claim for has never been deployed from here and is left as it is.
func pullPipeline(ctx context.Context, client *api.Client, f *folder) (*pullPlan, error) {
	local, _, err := readPipelineFiles(f.Root)
	if err != nil {
		return nil, err
	}
	codec := newPipelineCodec(f, local)
	plan := &pullPlan{files: map[string]*string{}}
	store := f.live(pipelineBaseline(f))

	// Tables first: a checks file is matched against its table's live row.
	for _, address := range sortedKeys(f.managed.claims) {
		claim := f.managed.claims[address]
		if claim.ResourceKind != wfdir.ManagedTable {
			continue
		}
		live, err := client.GetTable(ctx, claim.ResourceID)
		if err != nil {
			return nil, pullTableGone(f, claim, err)
		}
		before := localOrNil(local, address)
		content, headerRewritten, err := pulledSQL(f, codec, address, before, live)
		if err != nil {
			return nil, err
		}
		if before != nil && *before == content {
			continue
		}
		// What this stack last applied is the live SQL it recorded, in this
		// folder's disk form: a file that is not it is the folder's own change.
		// When the live table has not moved since (and nothing the header
		// declares has), there is nothing to absorb — the file stays, and the
		// anchor step reports it pending. When it has, the pull would replace
		// the folder's change, which runPull refuses without --force.
		if recorded := store.get(address); before != nil && recorded != "" && wfdir.HashString(*before) != recorded {
			liveDisk, _ := codec.canonicalDisk(*before, live)
			if wfdir.HashString(liveDisk) == recorded && !headerRewritten {
				plan.kept = append(plan.kept, address)
				continue
			}
			plan.undeployed = append(plan.undeployed, address)
		}
		plan.files[address] = &content
		plan.literals = append(plan.literals, markerLiterals(address, before, content)...)
	}

	if err := pullMetrics(ctx, client, f, codec, plan); err != nil {
		return nil, err
	}
	if err := pullChecks(ctx, client, f, local, plan); err != nil {
		return nil, err
	}
	if err := pullSidecars(ctx, client, f, plan); err != nil {
		return nil, err
	}

	plan.gate = func(ctx context.Context) error {
		sidecars, err := readTableDocsSidecars(f.Root)
		if err != nil {
			return err
		}
		return gateTableDocsSidecars(ctx, client, f, resolveTableDocsSidecars(f, sidecars))
	}
	plan.anchor = func(ctx context.Context, f *folder) ([]string, error) {
		res, err := runPipelinePush(ctx, f, nil, pipelinePushOptions{NoCreate: true, LiveFirst: true, Pull: true})
		if err != nil {
			return nil, err
		}
		var pending []string
		for _, file := range res.Files {
			if file.Outcome == pullPending {
				pending = append(pending, fmt.Sprintf("%s — %s", file.Path, file.Error))
			}
		}
		for _, m := range res.Metrics {
			switch m.Outcome {
			case pullPending:
				pending = append(pending, fmt.Sprintf("%s — %s", m.Path, m.Error))
			case pullOutcomeInexpressible:
				return pending, pullInexpressible(f, fmt.Sprintf("%s: %s", m.Path, m.Error))
			}
		}
		for _, d := range res.Docs {
			if d.Outcome == pullPending {
				pending = append(pending, fmt.Sprintf("%s — %s", d.Path, d.Error))
			}
		}
		for _, c := range res.Checks {
			switch {
			case c.Outcome == checksOutcomePendingPublish:
				pending = append(pending, fmt.Sprintf("%s — %s", c.Path, c.Error))
			case c.Outcome != "":
				return nil, fmt.Errorf("%s: %s", c.Path, c.Error)
			}
			for _, one := range c.Checks {
				if one.Outcome == pullPending {
					pending = append(pending, fmt.Sprintf("%s — check %q: %s", c.Path, one.Name, one.Error))
				}
			}
		}
		return pending, nil
	}
	return plan, nil
}

func pullTableGone(f *folder, claim *api.ManagedClaim, err error) error {
	if status := api.StatusOf(err); status == 404 || status == 400 {
		return &managedRefusal{Reason: syncReasonLost, Detail: fmt.Sprintf(
			"%s %s, which this folder manages at %s on stack %q, cannot be read (deleted, or out of your reach) — there is nothing live to pull. Run `ronja sync apply --stack %s` to see the claim's state and its remedy",
			claim.ResourceKind, claim.ResourceID, claim.Address, f.Stack, f.Stack)}
	}
	return fmt.Errorf("read %s %s: %w", claim.ResourceKind, claim.ResourceID, err)
}

// pulledSQL is one live table as its .sql file: the SQL in this folder's
// spelling, with the header's declared values rewritten to the row's — and
// whether any was (a value the row changed without its SQL).
func pulledSQL(f *folder, codec pipelineCodec, path string, before *string, live *api.Table) (string, bool, error) {
	code, unresolved := live.CanonicalCode()
	if unresolved {
		return "", false, pullInexpressible(f, fmt.Sprintf("the live SQL of %s (%s) carries positional refs this client cannot resolve (%s) — open it in the web builder and replace each with the table's id",
			path, live.IdentityID(), strings.Join(positionalRefsOf(code), ", ")))
	}
	local := ""
	if before != nil {
		local = *before
	}
	content := pullTableDisk(codec, local, code)

	docs, _ := tabledocs.ParseHeader(content)
	declaredName, _ := tabledocs.ParseName(content)
	var name *string
	if declaredName != nil && !sameName(*declaredName, live.Name) {
		n := live.Name
		name = &n
	}
	var docsNow tabledocs.Docs
	if !docs.Empty() && !docsHeldExactly(docs, live) {
		if docs.Description != nil && *docs.Description != live.Description {
			d := live.Description
			docsNow.Description = &d
		}
		onRow := live.ColumnDescriptions()
		for _, col := range docs.Names() {
			text, measured := onRow[col]
			if !measured || text == docs.Columns[col] {
				// A column the row has not measured keeps the file's prose: the
				// row says nothing about it to take.
				continue
			}
			if docsNow.Columns == nil {
				docsNow.Columns = map[string]string{}
			}
			docsNow.Columns[col] = text
		}
	}
	if name != nil || !docsNow.Empty() {
		rewritten := tabledocs.RewriteHeader(content, name, docsNow)
		return rewritten, rewritten != content, nil
	}
	return content, false, nil
}

func positionalRefsOf(code string) []string {
	var out []string
	for _, o := range markers.Scan(code) {
		if o.Positional {
			out = append(out, o.Marker)
		}
	}
	return out
}

// pullTableDisk de-aliases live SQL for one file: the file's own sibling
// spellings first (pipelineCodec.spellings), then a sibling this file did not
// reference before by its stem — unless the file spelled that id literally,
// which is kept — then the declared aliases, and an id nothing names stays the
// literal id.
func pullTableDisk(c pipelineCodec, local, code string) string {
	stems := c.spellings(local)
	literal := map[string]bool{}
	for _, o := range markers.Scan(local) {
		if o.Family.Kind == markers.KindTable && markers.IsResourceID(markers.KindTable, o.Arg) {
			literal[o.Arg] = true
		}
	}
	for path, id := range c.f.Binding.Tables {
		stem := tableNameFor(path)
		if _, spelled := stems[id]; spelled || literal[id] || c.pathByStem[stem] != path || len(c.ambiguous[stem]) > 0 {
			continue
		}
		stems[id] = stem
	}
	return rewriteMarkers(code, func(o markers.Occurrence) string {
		if o.Family.Kind == markers.KindTable {
			if stem, ok := stems[o.Arg]; ok {
				return stem
			}
		}
		if alias, ok := c.f.Codec.toAlias[aliasKey{kind: o.Family.Kind, name: o.Arg}]; ok {
			return alias
		}
		return o.Arg
	})
}

// pullAnchorTable is pushOneTable's pull branch: the live row is recorded as
// seen on every leg the file manages — SQL, documentation, name — exactly the
// acknowledgement a --force records, without its write. The file is then up to
// date when the live row holds it, and pending when only a deploy can make it
// (a rewritten header is SQL the row does not hold). The open draft is never
// read for drift and keeps its pointer.
func pullAnchorTable(f *folder, liveHash liveHashes, inst *wfdir.InstanceState, path, content string,
	out pipelineFileResult, live *api.Table, liveCode string, liveKnown bool,
	docs tabledocs.Docs, declaredName *string, draft *api.Table) pipelineFileResult {
	// A file that is not the live SQL, on a live table that has not moved since
	// this stack applied it, is the folder's own change: pull left it (kept).
	ownChange := liveKnown && liveCode != content && wfdir.HashString(liveCode) == liveHash.get(path)
	if liveKnown {
		recordLiveAgreement(liveHash, path, out.TableID, liveCode)
	}
	if !docs.Empty() {
		recordMetaAgreement(liveHash, path, out.TableID, metaOf(live))
	}
	if declaredName != nil && !sameName(liveHash.tableName(path), live.Name) {
		liveHash.setTableName(path, out.TableID, live.Name)
	}
	held := liveKnown && liveCode == content &&
		(docs.Empty() || docsHeldExactly(docs, live)) &&
		(declaredName == nil || sameName(*declaredName, live.Name))
	if !held {
		out.Outcome = pullPending
		out.Error = "its documentation header is part of its SQL, and the live table's description, column notes or name changed without its SQL — the next apply publishes this table once to write the header"
		switch {
		case !liveKnown:
			out.Error = "the live SQL carries positional refs, so only a deploy of the file can make the live table hold it"
		case ownChange:
			out.Error = "this folder changed it since the stack last applied it, and the live table has not changed since — pull left it as it is, and the next apply publishes it"
		}
		return out
	}
	recordSynced(inst, path, content)
	if draft == nil && inst.Tables[path].DraftID != "" {
		recordDraftPointer(inst, path, out.TableID, "")
	}
	out.Outcome = pushOutcomeUpToDate
	return out
}

// pullAnchorMetric is pushOneMetric's pull branch, on the table's rule: the
// live definition, name and declared fingerprint are recorded as seen only when
// the file holds the live row exactly. A file the pull left as it is (the
// folder's own change, on a definition that has not moved) is pending and
// records nothing. Anything else is a file that does not say what the live
// metric says after a pull wrote it — recording the live definition as seen
// would acknowledge a change the folder never took in, so it is refused as
// inexpressible (pullOutcomeInexpressible) and nothing is recorded.
func pullAnchorMetric(live liveHashes, file pipelineMetricFile, out pipelineMetricFileResult, row *api.Table, liveNow string) pipelineMetricFileResult {
	held := sameRecipe(row.MetricRecipe, file.Resolved) &&
		metricZoneHeld(file.File.ReportingTimezone, row.ReportingTimezone) &&
		(file.File.Description == nil || *file.File.Description == row.Description) &&
		(file.File.Name == nil || sameName(*file.File.Name, row.Name))
	if !held {
		if seenID, seenLive, seenDeclared := live.metricSeen(file.Path); seenID == out.MetricID && seenLive == liveNow &&
			seenDeclared != "" && seenDeclared != file.Declared {
			out.Outcome = pullPending
			out.Error = "this folder changed it since the stack last applied it, and the live metric has not changed since — pull left it as it is, and the next apply stages it"
			return out
		}
		out.Outcome = pullOutcomeInexpressible
		out.Error = "the file does not say exactly what the live metric holds, so nothing was recorded for it"
		return out
	}
	if file.File.Name != nil && !sameName(live.metricName(file.Path, out.MetricID), row.Name) {
		live.setMetricName(file.Path, out.MetricID, row.Name)
	}
	live.setMetricSeen(file.Path, out.MetricID, liveNow, file.Declared)
	out.Outcome = metricOutcomeUpToDate
	return out
}

// pullAnchorChecks is applyTableChecks's pull branch: every entry a live check
// matches is recorded as owned, as the push records a check it verified;
// nothing is written; an entry no live check matches is left pending.
func pullAnchorChecks(store liveHashes, file pipelineChecksFile, tableID string, entries []checkfile.Entry,
	targets []*api.TableCheck, managed map[string]wfdir.LockCheckEntry, out pipelineChecksResult) pipelineChecksResult {
	for i, e := range entries {
		row := targets[i]
		if row == nil || !e.Matches(liveCheckOf(row)) {
			out.Checks = append(out.Checks, pipelineCheckResult{Name: e.Name, Outcome: pullPending,
				Error: "not applied yet; the next apply writes it"})
			continue
		}
		for key, m := range managed {
			if m.CheckID == row.ID {
				delete(managed, key)
			}
		}
		managed[checkfile.Fold(e.Name)] = wfdir.LockCheckEntry{CheckID: row.ID, LiveSHA256: e.LiveSHA256(liveCheckOf(row))}
		out.Checks = append(out.Checks, pipelineCheckResult{Name: e.Name, CheckID: row.ID, Outcome: checksOutcomeUnchanged})
	}
	store.setChecksSeen(file.Path, tableID, managed)
	return out
}

// pullMetrics plans the metric files the folder's metric claims name.
func pullMetrics(ctx context.Context, client *api.Client, f *folder, codec pipelineCodec, plan *pullPlan) error {
	files, err := readMetricFiles(f.Root)
	if err != nil {
		return err
	}
	store := f.live(pipelineBaseline(f))
	byPath := map[string]pipelineMetricFile{}
	for _, file := range resolveMetricFiles(f, codec, store, files) {
		byPath[file.Path] = file
	}
	for _, address := range sortedKeys(f.managed.claims) {
		claim := f.managed.claims[address]
		if claim.ResourceKind != wfdir.ManagedMetric {
			continue
		}
		row, err := client.GetTable(ctx, claim.ResourceID)
		if err != nil {
			return pullTableGone(f, claim, err)
		}
		file, have := byPath[address]
		if have && file.Problem != "" {
			return pullInexpressible(f, fmt.Sprintf("%s cannot be read as it stands (%s) — fix it, then pull", address, file.Problem))
		}
		var tags *[]string
		if !have || file.File.Tags != nil {
			current, err := client.ListTableTags(ctx, row.IdentityID())
			switch {
			case err == nil:
				names := make([]string, 0, len(current))
				for _, tag := range current {
					names = append(names, tag.Name)
				}
				sort.Strings(names)
				tags = &names
			case have:
				return fmt.Errorf("read the tags of metric %s: %w", row.IdentityID(), err)
			}
		}
		if have && pulledMetricHolds(file, row, tags) {
			continue
		}
		// The file's declared fingerprint against the one this stack recorded
		// when it last applied it: a difference is the folder's own change. A
		// live definition that has not moved since, with everything else the
		// file declares held, leaves nothing to absorb — the file stays.
		if seenID, seenLive, seenDeclared := store.metricSeen(address); have && seenID == claim.ResourceID &&
			seenDeclared != "" && file.Declared != seenDeclared {
			if metricfile.RecipeSHA256(row.MetricRecipe) == seenLive && pulledMetricMetaHolds(file, row, tags) {
				plan.kept = append(plan.kept, address)
				continue
			}
			plan.undeployed = append(plan.undeployed, address)
		}
		before := readDiskFile(f.Root, address)
		body, literals, err := renderPulledMetric(f, codec, address, file, have, row, tags)
		if err != nil {
			return err
		}
		if before != nil && *before == body {
			continue
		}
		plan.files[address] = &body
		plan.literals = append(plan.literals, literals...)
	}
	return nil
}

// pulledMetricHolds reports that a metric file already declares exactly what
// the live row holds: its bytes are the author's, and stay.
func pulledMetricHolds(file pipelineMetricFile, row *api.Table, tags *[]string) bool {
	return sameRecipe(row.MetricRecipe, file.Resolved) && pulledMetricMetaHolds(file, row, tags)
}

// pulledMetricMetaHolds is pulledMetricHolds without the recipe: every key but
// the definition that the file declares, the row holds.
func pulledMetricMetaHolds(file pipelineMetricFile, row *api.Table, tags *[]string) bool {
	if !metricZoneHeld(file.File.ReportingTimezone, row.ReportingTimezone) {
		return false
	}
	if file.File.Description != nil && *file.File.Description != row.Description {
		return false
	}
	if file.File.Name != nil && !sameName(*file.File.Name, row.Name) {
		return false
	}
	if file.File.Tags != nil && tags != nil && metricfile.TagsFingerprint(*file.File.Tags) != metricfile.TagsFingerprint(*tags) {
		return false
	}
	return true
}

// renderPulledMetric lays out one live metric as its file, keeping the keys the
// file declares (every key a clone writes, for a file that is gone).
func renderPulledMetric(f *folder, codec pipelineCodec, address string, file pipelineMetricFile, have bool,
	row *api.Table, tags *[]string) (string, []pullLiteral, error) {
	var recipe map[string]json.RawMessage
	if err := json.Unmarshal(row.MetricRecipe, &recipe); err != nil {
		return "", nil, pullInexpressible(f, fmt.Sprintf("the live recipe of %s is not a JSON object (%v)", address, err))
	}
	var literals []pullLiteral
	var sourceID string
	if raw, ok := recipe["source"]; ok {
		_ = json.Unmarshal(raw, &sourceID)
	}
	spelling := sourceID
	if have {
		if current, err := file.File.Source(); err == nil {
			if id, err := resolveMetricSource(f, codec, current); err == nil && id == sourceID {
				spelling = current
			}
		}
	}
	if spelling == sourceID {
		switch {
		case sourceID == "":
		case siblingStemFor(codec, sourceID) != "":
			spelling = siblingStemFor(codec, sourceID)
		default:
			if alias, ok := f.Codec.toAlias[aliasKey{kind: markers.KindTable, name: sourceID}]; ok {
				spelling = alias
			} else if !have || func() bool { s, _ := file.File.Source(); return s != sourceID }() {
				literals = append(literals, pullLiteral{Path: address, Field: "recipe.source", Kind: markers.KindTable, ID: sourceID})
			}
		}
	}
	if spelling != "" {
		encoded, _ := json.Marshal(spelling)
		recipe["source"] = encoded
	}
	recipeBody, err := json.Marshal(recipe)
	if err != nil {
		return "", nil, err
	}

	out := map[string]any{"recipe": json.RawMessage(recipeBody)}
	switch {
	case have && file.File.Description != nil:
		out["description"] = row.Description
	case !have && row.Description != "" && row.DescriptionSource == api.DescriptionSourceUser:
		out["description"] = row.Description
	}
	if have && file.File.ReportingTimezone != nil {
		out["reportingTimezone"] = row.ReportingTimezone
	}
	stem, _ := wfdir.MetricAlias(address)
	if (have && file.File.Name != nil) || (!have && stem != row.Name) {
		out["name"] = row.Name
	}
	if tags != nil && (have || len(*tags) > 0) {
		out["tags"] = *tags
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", nil, err
	}
	text := string(body) + "\n"
	// Read back as a push reads it: the file must parse and resolve to the
	// live definition.
	parsed, err := metricfile.Parse([]byte(text))
	if err != nil {
		return "", nil, pullInexpressible(f, fmt.Sprintf("%s would not be a metric file this loop accepts: %v", address, err))
	}
	source, err := parsed.Source()
	if err != nil {
		return "", nil, pullInexpressible(f, fmt.Sprintf("%s: %v", address, err))
	}
	id, err := resolveMetricSource(f, codec, source)
	if err != nil {
		return "", nil, pullInexpressible(f, fmt.Sprintf("%s: %v", address, err))
	}
	resolved, err := parsed.Resolve(id)
	if err != nil || !sameRecipe(row.MetricRecipe, resolved) {
		return "", nil, pullInexpressible(f, fmt.Sprintf("%s would not read back as the live definition of %s", address, row.IdentityID()))
	}
	return text, literals, nil
}

// siblingStemFor is the stem of the folder file bound to a table id, "" when
// none is, or when two files share the stem.
func siblingStemFor(c pipelineCodec, id string) string {
	for path, bound := range c.f.Binding.Tables {
		if bound != id {
			continue
		}
		stem := tableNameFor(path)
		if c.pathByStem[stem] == path && len(c.ambiguous[stem]) == 0 {
			return stem
		}
	}
	return ""
}

// pullChecks plans the checks files of the tables this folder manages.
func pullChecks(ctx context.Context, client *api.Client, f *folder, local map[string]string, plan *pullPlan) error {
	files, err := readChecksFiles(f.Root, local)
	if err != nil {
		return err
	}
	store := f.live(pipelineBaseline(f))
	for _, file := range files {
		if file.Problem != "" || file.SQLPath == "" {
			continue
		}
		claim := f.managed.claims[file.SQLPath]
		if claim == nil || claim.ResourceKind != wfdir.ManagedTable {
			continue
		}
		tableID := claim.ResourceID
		rows, err := client.ListTableChecks(ctx, tableID)
		if err != nil {
			if api.IsUnmatchedRoute(err) {
				continue
			}
			return fmt.Errorf("read the checks on %s: %w", tableID, err)
		}
		managed := map[string]wfdir.LockCheckEntry{}
		if recordedTable, recorded := store.checksSeen(file.Path); recordedTable == tableID {
			for name, entry := range recorded {
				managed[name] = entry
			}
		}
		p := planTableChecks(file.File.Checks, rows, managed, checksClaimFor(f, files, store, tableID, file.Path))
		if p.dup != "" {
			return pullInexpressible(f, fmt.Sprintf("%s: %s", file.Path, p.dup))
		}
		var entries []checkfile.Entry
		changed := false
		for i, e := range file.File.Checks {
			row := p.targets[i]
			if row == nil {
				if seen, owned := managed[checkfile.Fold(e.Name)]; owned {
					// The folder's check, and it is gone from the table: deleted
					// in the app. The file follows — unless the entry is the
					// folder's own change, which the delete would take with it.
					if seen.LiveSHA256 != "" && e.SHA256() != seen.LiveSHA256 {
						plan.undeployed = append(plan.undeployed, fmt.Sprintf("%s (check %q)", file.Path, e.Name))
					}
					changed = true
					continue
				}
				// Never applied: the next deploy's to create.
				entries = append(entries, e)
				continue
			}
			if e.Matches(liveCheckOf(row)) {
				entries = append(entries, e)
				continue
			}
			// The entry against what this stack recorded of the check it owns:
			// an entry that is not it is the folder's own change. A live check
			// unmoved since leaves nothing to absorb — the entry stays.
			if seen, owned := managed[checkfile.Fold(e.Name)]; owned && seen.CheckID == row.ID && seen.LiveSHA256 != "" &&
				e.SHA256() != seen.LiveSHA256 {
				if e.LiveSHA256(liveCheckOf(row)) == seen.LiveSHA256 {
					entries = append(entries, e)
					plan.kept = append(plan.kept, fmt.Sprintf("%s (check %q)", file.Path, e.Name))
					continue
				}
				plan.undeployed = append(plan.undeployed, fmt.Sprintf("%s (check %q)", file.Path, e.Name))
			}
			pulled, err := pulledCheckEntry(e, row)
			if err != nil {
				return pullInexpressible(f, fmt.Sprintf("%s: %v", file.Path, err))
			}
			entries = append(entries, pulled)
			changed = true
		}
		if !changed {
			continue
		}
		body, err := checkfile.Encode(checkfile.File{Checks: entries})
		if err != nil {
			return err
		}
		text := string(body)
		if before := readDiskFile(f.Root, file.Path); before != nil && *before == text {
			continue
		}
		plan.files[file.Path] = &text
	}
	return nil
}

// pulledCheckEntry is one entry as the live check it resolves to stands: its
// name and body, and the three-state fields the entry declares.
func pulledCheckEntry(e checkfile.Entry, row *api.TableCheck) (checkfile.Entry, error) {
	out := checkfile.Entry{Name: strings.TrimSpace(row.Name)}
	switch row.Kind {
	case checkfile.KindFreshness:
		if row.ExpectedIntervalSeconds%3600 != 0 || row.ExpectedIntervalSeconds <= 0 {
			return out, fmt.Errorf("check %q expects a refresh every %d seconds, which a checks file (whole hours) cannot say", row.Name, row.ExpectedIntervalSeconds)
		}
		out.ExpectedIntervalHours = row.ExpectedIntervalSeconds / 3600
	default:
		out.Expression = strings.TrimSpace(row.Expression)
	}
	if e.Severity != nil {
		sev := row.Severity
		out.Severity = &sev
	}
	if e.Description != nil {
		desc := strings.TrimSpace(row.Description)
		out.Description = &desc
	}
	if e.Enabled != nil {
		enabled := row.Enabled
		out.Enabled = &enabled
	}
	if !out.Matches(liveCheckOf(row)) {
		return out, fmt.Errorf("check %q would not read back as it stands live", row.Name)
	}
	return out, nil
}

// pullSidecars plans the docs sidecars: each takes the row's prose for what it
// declares, where the row has measured the column.
func pullSidecars(ctx context.Context, client *api.Client, f *folder, plan *pullPlan) error {
	sidecars, err := readTableDocsSidecars(f.Root)
	if err != nil {
		return err
	}
	store := f.live(pipelineBaseline(f))
	for _, file := range resolveTableDocsSidecars(f, sidecars) {
		if file.Problem != "" || file.TableID == "" || file.Docs.Empty() {
			continue
		}
		row, err := client.GetTable(ctx, file.TableID)
		if err != nil {
			// Never skipped: a sidecar left unplanned would still be anchored
			// by the step after, acknowledging prose the folder never took in.
			return fmt.Errorf("read %s, which %s documents: %w — nothing was written; pull again", file.TableID, file.Path, err)
		}
		if docsHeldExactly(file.Docs, row) {
			continue
		}
		// The sidecar's fingerprint against the one this stack recorded: a
		// difference is the folder's own prose. A row whose prose has not moved
		// since leaves nothing to absorb — the file stays.
		if seenID, seenMeta, seenDeclared := store.docsSeen(file.Path); seenID == file.TableID && seenDeclared != "" &&
			file.Docs.Fingerprint() != seenDeclared {
			if metaOf(row) == seenMeta {
				plan.kept = append(plan.kept, file.Path)
				continue
			}
			plan.undeployed = append(plan.undeployed, file.Path)
		}
		out := map[string]any{}
		if file.Docs.Description != nil {
			out["description"] = row.Description
		}
		onRow := row.ColumnDescriptions()
		if len(file.Docs.Columns) > 0 {
			cols := map[string]string{}
			for _, col := range file.Docs.Names() {
				if text, measured := onRow[col]; measured {
					cols[col] = text
				} else {
					cols[col] = file.Docs.Columns[col]
				}
			}
			out["columns"] = cols
		}
		body, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		text := string(body) + "\n"
		if before := readDiskFile(f.Root, file.Path); before != nil && *before == text {
			continue
		}
		if docs, err := tabledocs.ParseSidecar([]byte(text)); err != nil || docs.Fingerprint() == file.Docs.Fingerprint() {
			continue
		}
		plan.files[file.Path] = &text
	}
	return nil
}
