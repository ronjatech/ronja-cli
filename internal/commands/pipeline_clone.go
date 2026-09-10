package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/metricfile"
	"github.com/ronjatech/ronja-cli/internal/tablerefs"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja pipeline clone` copies a feature's derived tables into a new local
// folder, one .sql file each.
//
// READ-ONLY, like `wf clone`: no draft is created, so cloning to read somebody's
// pipeline leaves nothing behind. Checkout happens on the first push.
//
// It clones DERIVED tables and METRICS. Foundation, integration and dynamic
// tables have no SQL a file could hold — they appear inside refs, as ids, and
// that is the whole of their presence here.
//
// A metric has no SQL either, but it does have a recipe, so it clones into
// `metrics/<name>.json` rather than into a .sql file. It runs the identical
// draft flow a derived table runs, which is what makes `push`, `publish` and
// `discard` mean the same thing for it — and is why leaving it out would have
// made "metrics cannot be edited from a folder" true by omission after the rest
// of this loop had already made it false.
func newPipelineCloneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clone <feature-id> [directory]",
		Short: "Copy a feature's derived tables and metrics into a new local folder",
		Long: `Copy a feature's derived tables and metrics into a new local folder.

Fetches every derived table in the feature and writes one .sql file per table
(named after the table), plus metrics/<name>.json for every metric, together
with ronja.json and a .ronja/ sync baseline:

  ronja pipeline clone collection-abc123
  ronja pipeline clone collection-abc123 ./sales-pipeline

If you already have an open draft of a table — from the web builder, from chat,
or from an earlier push — its SQL is cloned instead of the live version, because
that is your newest state. This is said on stderr when it happens.

Refs are written in id form. A table whose stored SQL carries a positional ref
this client cannot resolve is REFUSED rather than written half-right, because a
surviving positional ref would be re-resolved against a different input list on
the next push and would silently read a different table.

Nothing is created on the server: cloning does not open a draft. The target
directory must be empty or absent.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			client := newClient(resolved.URL, resolved.Token)
			featureID := args[0]

			// An explicitly named directory can be rejected before a single
			// request: "that folder is not empty" does not become truer after a
			// listing and twenty GETs, and this is the form CI uses.
			root := ""
			if len(args) == 2 {
				if root, err = checkTarget(args[1]); err != nil {
					return err
				}
			}

			feature, err := client.GetFeature(cmd.Context(), featureID)
			if err != nil {
				if api.StatusOf(err) == 404 {
					return fmt.Errorf("no feature %s on %s that you can read", featureID, resolved.URL)
				}
				return err
			}
			items, err := client.ListFeatureTables(cmd.Context(), featureID)
			if err != nil {
				return fmt.Errorf("list the tables of %s: %w", featureID, err)
			}
			cloneable, metricRows, skipped := partitionCloneable(items)
			if len(cloneable) == 0 && len(metricRows) == 0 {
				reportSkippedKinds(skipped)
				return fmt.Errorf("%q has no derived tables and no metrics to clone — a pipeline folder holds derived-table SQL and metric definitions, and this feature has neither",
					feature.Name)
			}

			// The default name needs the feature's title, so this waits for the
			// read above — but it still precedes the per-table fetch, which is the
			// expensive leg.
			if root == "" {
				if root, err = checkTarget(wfdir.Slug(feature.Name, wfdir.KindPipeline)); err != nil {
					return err
				}
			}

			// ONE claimed-path set across both writers, for uniqueMetricPath's
			// reason: the two kinds share a namespace server-side, and a folder
			// holding revenue.sql beside metrics/revenue.json is one whose very
			// first push this loop's own collision guard refuses.
			used := map[string]bool{}
			sources, err := fetchCloneSources(cmd.Context(), client, used, cloneable)
			if err != nil {
				return err
			}
			metricSources, err := fetchCloneMetrics(cmd.Context(), client, used, metricRows, &skipped)
			if err != nil {
				return err
			}
			// The binding is keyed by organization as well as instance, so it has
			// to be known before one is written — and it is resolved BEFORE the
			// first file is laid down, not after. A folder whose files landed and
			// whose manifest did not is one every command refuses (the target
			// directory is no longer empty) and nothing can repair: the whole point
			// of doing it here is that a failed /me leaves an empty directory
			// rather than a half-clone.
			if err := ensureTenant(cmd.Context(), resolved); err != nil {
				return err
			}
			// Checked BEFORE anything is created, and fatal rather than per-file:
			// the baseline written below claims every one of these paths is on
			// disk, and a set that cannot all be written produces a folder whose
			// baseline lies from birth. See wfdir.CheckLocalPaths.
			paths := make([]string, 0, len(sources))
			for _, s := range sources {
				paths = append(paths, s.Path)
			}
			if err := wfdir.CheckLocalPaths(paths, wfdir.PipelineKind); err != nil {
				return err
			}
			// The METRIC files are checked SEPARATELY and with a different
			// question, because CheckLocalPaths asks two things and only one of
			// them applies here. It asks "can this path be written to disk", which
			// a metric file must pass — and "is this path SYNCABLE for this kind",
			// which a metric file must FAIL: a pipeline folder's SyncExt is ".sql",
			// so `metrics/x.json` is deliberately invisible to Enumerate and is
			// read by the metric pass alone. Running the whole check over them
			// would refuse every clone of a feature holding a metric, for a
			// property this carrier is designed to have.
			//
			// Case collisions are already impossible here: uniqueMetricPath and
			// uniqueSQLPath share one claimed-path set, folded to lower case.
			for _, m := range metricSources {
				if err := wfdir.ValidatePath(m.Path); err != nil {
					return fmt.Errorf("metric %q cannot be written to disk as %s (%v) — rename it in the web app, then clone again",
						m.Name, m.Path, err)
				}
			}

			if err := os.MkdirAll(root, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", root, err)
			}
			for _, s := range sources {
				// Validated once more inside WriteFile: the path is built from a
				// server-supplied name, and writing it is a local filesystem
				// operation that must not be able to escape the folder.
				if err := wfdir.WriteFile(root, s.Path, s.Code); err != nil {
					return fmt.Errorf("write %s: %w", s.Path, err)
				}
			}
			for _, m := range metricSources {
				if err := wfdir.WriteFile(root, m.Path, m.Body); err != nil {
					return fmt.Errorf("write %s: %w", m.Path, err)
				}
			}

			key := wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID}
			manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: feature.Name}
			binding := wfdir.Binding{FeatureID: featureID, Tables: map[string]string{}}
			baseline := &wfdir.InstanceState{
				SourceID: featureID,
				Files:    map[string]wfdir.FileState{},
				Tables:   map[string]wfdir.TableState{},
			}
			// The live fingerprints go wherever this folder's shape puts them:
			// the lock file for a --stack clone, the local baseline otherwise.
			// See liveHashes.
			lock := &wfdir.Lock{}
			liveHash := liveHashes{lock: lock, stack: flagStack, inst: baseline}
			for _, s := range sources {
				// The STABLE identity, never the draft's id: a draft dies at commit
				// while the binding lives in the customer's git.
				binding.Tables[s.Path] = s.TableID
				// TWO fingerprints, from two rows, and this is the clearest place to
				// see why they cannot be one: when the clone preferred an open draft,
				// what is on disk is the DRAFT's SQL while the live table holds
				// something else entirely. Recorded as one hash, the next push
				// compared the live table against the draft's bytes and refused a
				// folder that had done nothing wrong.
				if s.LiveCode != "" {
					recordLiveAgreement(liveHash, s.Path, s.TableID, s.LiveCode)
				}
				if s.DraftID != "" {
					// No WIRE fingerprint: this clone read that draft, it did not
					// write it, so there are no sent bytes to vouch for. See
					// recordDraftWrite — the first push writes unconditionally.
					recordDraftWrite(baseline, s.Path, s.TableID, s.DraftID, s.Code, "")
				} else {
					recordDraftPointer(baseline, s.Path, s.TableID, "")
				}
				recordSynced(baseline, s.Path, s.Code)
			}
			for _, m := range metricSources {
				// BOTH fingerprints, and neither is optional. The live one is what
				// the drift guard compares the row against on the next push; the
				// declared one is what tells that push the file has not changed
				// since this clone wrote it. Recorded here rather than left to the
				// first push, so a fresh clone reads as clean instead of proposing
				// to re-stage every metric it just copied.
				//
				// ⚠️ On a LEGACY (unnamed instances[]) folder these land in
				// .ronja/state.json, which is per-user and git-ignored — so a
				// colleague's clone of that folder starts with no metric id and its
				// first push would try to CREATE a metric that already exists. That
				// push is caught by the adopt-by-name recovery in
				// reconcileMetricNameTaken rather than duplicating the row; a
				// --stack folder has the recording committed and never gets there.
				liveHash.setMetricSeen(m.Path, m.MetricID, m.LiveSHA256, m.Declared)
			}
			if err := recordFirstBindingInto(manifest, lock, key, binding); err != nil {
				return err
			}
			if err := wfdir.SaveFolder(root, manifest, lock); err != nil {
				return err
			}
			state := &wfdir.State{}
			state.Set(key, baseline)
			if err := wfdir.SaveState(root, state); err != nil {
				return err
			}

			reportSkippedKinds(skipped)
			for _, s := range sources {
				if s.DraftID != "" {
					fmt.Fprintf(os.Stderr, "  Note: %s came from your open draft %s (newer than the live table).\n",
						s.Path, s.DraftID)
				}
			}

			if flagJSON {
				files := make([]map[string]any, 0, len(sources))
				for _, s := range sources {
					files = append(files, map[string]any{
						"path": s.Path, "tableID": s.TableID, "name": s.Name,
						"clonedFromDraft": s.DraftID != "",
					})
				}
				// A SEPARATE key rather than more entries in `files`, because the
				// entries are a different shape: a metric has a metricID and never
				// a tableID, and folding the two would make a script reading
				// `tableID` see an empty string for half the folder.
				metricFiles := make([]map[string]any, 0, len(metricSources))
				for _, m := range metricSources {
					metricFiles = append(metricFiles, map[string]any{
						"path": m.Path, "metricID": m.MetricID, "name": m.Name,
					})
				}
				return emitJSON(map[string]any{
					"root":        root,
					"url":         resolved.URL,
					"kind":        wfdir.KindPipeline,
					"featureID":   featureID,
					"title":       feature.Name,
					"tables":      len(sources),
					"files":       files,
					"metrics":     len(metricSources),
					"metricFiles": metricFiles,
					"skipped":     skippedPayload(skipped),
				})
			}
			printPipelineCloneReport(root, resolved.URL, feature, sources, metricSources, skipped)
			return nil
		},
	}
	return cmd
}

// cloneSource is one table as it will be laid down on disk.
type cloneSource struct {
	Path    string
	Name    string
	TableID string
	// DraftID is the caller's own draft the SQL came from, empty when it came
	// from the live table. Recorded in the baseline so the next push resumes that
	// draft rather than trying to check out a second one.
	DraftID string
	// Code is the CANONICAL id-ref form, which is what is written to disk and
	// what the baseline hashes.
	Code string
	// LiveCode is the LIVE table's canonical SQL, which is Code again unless the
	// clone preferred an open draft. Carried separately because the baseline
	// records a fingerprint per ROW, and the live table is the row the drift
	// guard's first leg compares. Empty when the live row could not be
	// canonicalized — no live baseline, which disarms that leg rather than
	// recording a hash of something unreadable.
	LiveCode string
	// URL is the table's frontend page, as the SERVER stamped it.
	URL string
}

// skippedTables is what a clone left behind, split by REASON rather than lumped
// together — the two halves say different things, and one of them is easy to say
// falsely.
type skippedTables struct {
	// Kinds are the non-derived kinds seen, sorted. These tables genuinely have
	// no SQL a file could hold.
	Kinds []string
	// KindCount is how many rows those kinds accounted for.
	KindCount int
	// RecipeUnavailable is how many METRICS this clone could not write because
	// the instance answered no recipe for them.
	//
	// Counted apart from Kinds because it is a fact about the INSTANCE rather
	// than about the kind: a metric row cannot exist without a recipe (the
	// schema's own CHECK says so), so an empty one means this backend does not
	// serve the field yet. Saying that, rather than saying "metrics are not
	// cloned", is the difference between a caveat somebody can act on and one
	// that talks them out of a capability that works.
	RecipeUnavailable int
}

// partitionCloneable splits a feature's tables into the derived live rows a
// folder can hold and everything else.
//
// The live filter is belt-and-braces: the list endpoint excludes shadow rows by
// its own default, so a draft or a committed version should never appear. It is
// applied anyway because the cost of being wrong is a folder bound to a row that
// stops existing the moment its author publishes.
func partitionCloneable(items []*api.TableListItem) (cloneable, metrics []*api.TableListItem, skipped skippedTables) {
	kinds := map[string]bool{}
	// De-duplicated by ID, because the listing is OFFSET-paginated: a table
	// created or deleted while the walk is in flight shifts a row across a page
	// boundary and it comes back twice. Two entries for one table produce two
	// files (the second suffixed -2), two bindings pointing at the same row, and
	// a folder where a push writes one table from two files in an order nothing
	// defines. Dropping the duplicate is not hiding the skew — the row that
	// slipped the other way is still missed, and that is reported as one fewer
	// file, exactly as ListFeatureTables' own note says.
	seen := map[string]bool{}
	for _, item := range items {
		if item == nil || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		if item.ParentModelID != "" ||
			item.ShadowStatus == api.ShadowStatusDraft ||
			item.ShadowStatus == api.ShadowStatusCommitted {
			continue
		}
		switch item.Kind {
		case api.TableKindDerived:
			cloneable = append(cloneable, item)
		case api.TableKindMetric:
			metrics = append(metrics, item)
		default:
			kinds[item.Kind] = true
			skipped.KindCount++
		}
	}
	for kind := range kinds {
		skipped.Kinds = append(skipped.Kinds, kind)
	}
	sort.Strings(skipped.Kinds)
	// Sorted by name then id, so the filenames — and any collision suffixes on
	// them — are the same on every clone of the same feature.
	byNameThenID := func(rows []*api.TableListItem) {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Name != rows[j].Name {
				return rows[i].Name < rows[j].Name
			}
			return rows[i].ID < rows[j].ID
		})
	}
	byNameThenID(cloneable)
	byNameThenID(metrics)
	return cloneable, metrics, skipped
}

// metricCloneSource is one metric as it will be laid down on disk.
type metricCloneSource struct {
	Path     string
	Name     string
	MetricID string
	// Body is the file's bytes, and Declared is the fingerprint of what a push of
	// exactly those bytes would send — computed by PARSING what was just written
	// rather than by predicting it, so the clone's recording and the push's
	// cannot disagree about what this file declares.
	Body     string
	Declared string
	// LiveSHA256 is the live metric's recipe fingerprint, taken over the bytes
	// the server sent. Empty for a row that answered no recipe at all, which
	// disarms the drift guard rather than recording a hash of nothing.
	LiveSHA256 string
	URL        string
}

// fetchCloneMetrics reads each metric's recipe and lays out the file that
// defines it.
//
// THE RECIPE IS WRITTEN VERBATIM, in ID FORM, exactly as the .sql files' refs
// are and for the same reason: `clone` refuses a destination that is not empty,
// so the folder it writes declares no dependencies and claims no stems — there
// is nothing an alias codec here could translate. `ronja bind` is what turns
// those ids into names later.
//
// THE DESCRIPTION IS CLONED ONLY WHEN A PERSON WROTE IT. `descriptionSource`
// distinguishes prose Ronja may rewrite from prose it may not, and a push sends
// what a file declares as `user` — so cloning Ronja's own AI-written description
// into the file would, on the very next push, re-stamp it as human-authored and
// stop the describer ever touching it again. An absent key is the right claim
// there: this folder does not manage that prose.
//
// The reporting timezone is left out for the same three-state reason and one
// more: it is not on the row this client mirrors at all, and inventing a claim
// about the calendar a number is evaluated at — from a field nobody read — is
// the one guess in this file that would change what people see.
func fetchCloneMetrics(ctx context.Context, client *api.Client, used map[string]bool,
	items []*api.TableListItem, skipped *skippedTables) ([]metricCloneSource, error) {

	var out []metricCloneSource
	for i, item := range items {
		// The same heartbeat fetchCloneSources prints, for the same reason: one
		// request per metric, one metric at a time, is minutes of nothing at all on
		// a feature that defines many of them — and nothing at all reads as a hung
		// command. On stderr, so the report on stdout is unchanged.
		if i > 0 && i%cloneProgressEvery == 0 {
			fmt.Fprintf(os.Stderr, "  … read %d of %d metrics\n", i, len(items))
		}
		row, err := client.GetTable(ctx, item.ID)
		if err != nil {
			return nil, fmt.Errorf("read metric %s (%s): %w", item.ID, item.Name, err)
		}
		if len(row.MetricRecipe) == 0 {
			// A metric row with no recipe cannot exist by the schema's own CHECK,
			// so this is an INSTANCE that does not serve the field rather than a
			// broken row. Skipped loudly rather than refusing the whole clone: the
			// derived tables beside it are perfectly cloneable, and a clone that
			// died on the one metric an older backend cannot describe would make
			// this command unusable against exactly the instances most likely to
			// need it.
			//
			// Skipped rather than written as an empty file, because a metric file
			// with no recipe is one this loop refuses to parse — it would be a file
			// committed to git that no push could ever send.
			skipped.RecipeUnavailable++
			fmt.Fprintf(os.Stderr, "  Note: metric %s (%s) was not cloned — this instance answered no recipe for it, so there is nothing a file could hold.\n",
				item.ID, item.Name)
			continue
		}
		body, err := renderMetricFile(row)
		if err != nil {
			return nil, fmt.Errorf("write the metric file for %s (%s): %w", item.ID, item.Name, err)
		}
		declared, err := declaredFingerprintOf(body)
		if err != nil {
			return nil, fmt.Errorf("read back the metric file for %s (%s): %w", item.ID, item.Name, err)
		}
		out = append(out, metricCloneSource{
			Path:       uniqueMetricPath(used, item.Name, item.ID),
			Name:       item.Name,
			MetricID:   row.IdentityID(),
			Body:       body,
			Declared:   declared,
			LiveSHA256: metricfile.RecipeSHA256(row.MetricRecipe),
			URL:        row.URL,
		})
	}
	return out, nil
}

// renderMetricFile lays out one metric row as the file that defines it,
// pretty-printed so the thing a person is about to edit reads as a document
// rather than as one line of JSON.
func renderMetricFile(row *api.Table) (string, error) {
	file := map[string]any{"recipe": row.MetricRecipe}
	if row.Description != "" && row.DescriptionSource == api.DescriptionSourceUser {
		file["description"] = row.Description
	}
	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return "", err
	}
	return string(body) + "\n", nil
}

// declaredFingerprintOf computes what a push of these exact bytes would declare,
// by running them through the SAME parse and resolve a push runs.
//
// Predicting it instead would be a second implementation of the fingerprint, and
// the two would disagree the first time either the parser's compaction or the
// resolver's re-assembly changed — leaving a freshly cloned folder reporting
// every metric as pending on its first status.
func declaredFingerprintOf(body string) (string, error) {
	parsed, err := metricfile.Parse([]byte(body))
	if err != nil {
		return "", err
	}
	source, err := parsed.Source()
	if err != nil {
		return "", err
	}
	resolved, err := parsed.Resolve(source)
	if err != nil {
		return "", err
	}
	return metricfile.Fingerprint(resolved, parsed.Description, parsed.ReportingTimezone), nil
}

// uniqueMetricPath turns a metric's name into its file's path, keeping the set
// distinct.
//
// It shares `used` with uniqueSQLPath, and that sharing is the point rather than
// an economy: a metric and a table cannot hold one name inside a feature —
// `requireNameFreeTx` has no `kind` predicate — so a clone that gave them
// separate namespaces would write `revenue.sql` and `metrics/revenue.json` and
// produce a folder whose very first push this loop's own collision guard
// refuses.
func uniqueMetricPath(used map[string]bool, name, metricID string) string {
	base := wfdir.FileSlug(name, wfdir.FileSlug(metricID, "metric"))
	stem := base
	for i := 2; used[strings.ToLower(stem+".sql")] || used[strings.ToLower(wfdir.MetricPath(stem))]; i++ {
		stem = fmt.Sprintf("%s-%d", base, i)
	}
	used[strings.ToLower(wfdir.MetricPath(stem))] = true
	return wfdir.MetricPath(stem)
}

// cloneProgressEvery is how often the per-table fetch says where it is. Ten is
// often enough that a big feature never looks stuck and rare enough that a small
// one prints nothing at all.
const cloneProgressEvery = 10

// fetchCloneSources reads each table's SQL — preferring the caller's own draft —
// and canonicalizes it, refusing the whole clone if any of it cannot be.
//
// The refusal is collective and happens before a byte is written: a folder
// missing one file would have a baseline that never mentions it, so the file
// would simply never appear and nothing would ever say why.
// `used` is the folder's whole claimed-path set, shared with fetchCloneMetrics:
// a metric and a table cannot hold one name inside a feature, so the two writers
// have to be able to see each other's filenames.
func fetchCloneSources(ctx context.Context, client *api.Client, used map[string]bool, items []*api.TableListItem) ([]cloneSource, error) {
	var out []cloneSource
	var refused []string

	for i, item := range items {
		// Two requests per table, one table at a time, is minutes for a large
		// feature — and until now it was minutes of nothing at all, which reads as
		// a hung command. On stderr, so the report on stdout is unchanged.
		if i > 0 && i%cloneProgressEvery == 0 {
			fmt.Fprintf(os.Stderr, "  … read %d of %d tables\n", i, len(items))
		}
		row, err := client.GetTable(ctx, item.ID)
		if err != nil {
			return nil, fmt.Errorf("read table %s (%s): %w", item.ID, item.Name, err)
		}
		source := row
		draftID := ""
		// Prefer the caller's own draft: it is their newest state, and cloning
		// live over it would look like their web-builder or chat edits had
		// vanished. Best-effort — a failure here degrades to the live SQL rather
		// than aborting a clone that can perfectly well proceed.
		if draft, draftErr := client.GetTableDraft(ctx, item.ID); draftErr != nil {
			fmt.Fprintf(os.Stderr, "  Note: could not check for your open draft of %s (%v) — cloning the live version.\n",
				item.ID, draftErr)
		} else if draft != nil {
			source = draft
			draftID = draft.ID
		}

		// Canonicalized and NOT de-aliased, deliberately: `clone` refuses a
		// destination that is not empty, so the folder this writes declares no
		// dependencies and claims no stems — there is nothing an alias codec here
		// could translate. The ids it writes to disk are what a later `ronja bind`
		// converts into names.
		code, unresolved := source.CanonicalCode()
		if unresolved {
			refused = append(refused, fmt.Sprintf("%s (%s): %s",
				item.Name, item.ID, strings.Join(tablerefs.PositionalRefs(code), ", ")))
			continue
		}
		// The LIVE row's own SQL, whether or not the draft was preferred. A live
		// row that cannot be canonicalized is NOT a refusal here — the file on disk
		// comes from the draft and is perfectly valid; what is lost is the live
		// fingerprint, and an empty one honestly says "no baseline for that row".
		liveCode := code
		if draftID != "" {
			if resolved, liveUnresolved := row.CanonicalCode(); !liveUnresolved {
				liveCode = resolved
			} else {
				liveCode = ""
			}
		}
		out = append(out, cloneSource{
			Path:     uniqueSQLPath(used, item.Name, item.ID),
			Name:     item.Name,
			TableID:  row.IdentityID(),
			DraftID:  draftID,
			Code:     code,
			LiveCode: liveCode,
			URL:      row.URL,
		})
	}

	if len(refused) > 0 {
		return nil, fmt.Errorf("these tables carry positional refs this client cannot resolve, so nothing was written:\n    %s\n  A positional ref is an index into the table's stored input list, and these point past the end of it (or at something that is not a table) — which means the stored SQL and the stored inputs disagree.\n  Open each table in the web builder and replace the ref with the table's id, then clone again",
			strings.Join(refused, "\n    "))
	}
	return out, nil
}

// uniqueSQLPath turns a table name into a filename, keeping the set distinct.
//
// Collisions are real: two tables in one feature may perfectly well be called
// "Orders" and "orders", and both slugify to the same thing. The suffix is
// appended rather than the second file being skipped, because a skipped file is
// a table the folder silently does not manage.
//
// FileSlug, not Slug, and that is the round trip rather than a preference: a
// table's display name is the file stem this folder pushed, so a clone of a
// feature this folder created has to write the same filenames back. Slug folds
// `_` into `-`, so orders_clean.sql came back as orders-clean.sql — a second
// file for one table, beside the one git was already tracking.
func uniqueSQLPath(used map[string]bool, name, tableID string) string {
	// The id is the fallback rather than a constant, so a table whose name
	// slugifies to nothing (an emoji, a non-Latin script) still gets a
	// distinguishable filename instead of colliding with every other such table.
	base := wfdir.FileSlug(name, wfdir.FileSlug(tableID, "table"))
	path := base + ".sql"
	for i := 2; used[strings.ToLower(path)]; i++ {
		path = fmt.Sprintf("%s-%d.sql", base, i)
	}
	// Folded, because the folder has to be writable on a case-insensitive
	// filesystem — CheckLocalPaths refuses a set that is not, and producing one
	// here would turn an ordinary clone into that refusal.
	used[strings.ToLower(path)] = true
	return path
}

// reportSkippedKinds says once, on stderr, what the clone left behind.
//
// One line now rather than two. The second used to say that metrics were "not
// cloned (out of scope for this loop)" — carefully worded so as not to claim
// they could not be, because they always could. They are cloned now, so the
// sentence is gone rather than reworded: a note about something that did not
// happen is exactly the kind of caveat that outlives its own deploy.
func reportSkippedKinds(s skippedTables) {
	if s.KindCount > 0 {
		fmt.Fprintf(os.Stderr, "  Note: %d table(s) not cloned (kinds: %s) — a pipeline folder holds derived-table SQL and metric definitions.\n",
			s.KindCount, strings.Join(s.Kinds, ", "))
	}
}

func skippedPayload(s skippedTables) map[string]any {
	kinds := s.Kinds
	if kinds == nil {
		kinds = []string{}
	}
	return map[string]any{
		"kinds": kinds, "kindCount": s.KindCount, "metricsWithoutRecipe": s.RecipeUnavailable,
	}
}

func printPipelineCloneReport(root, url string, feature *api.Feature, sources []cloneSource,
	metrics []metricCloneSource, skipped skippedTables) {

	out := os.Stdout
	fmt.Fprintf(out, "  Cloned %q into %s\n\n", feature.Name, root)
	fmt.Fprintf(out, "  Feature:  %s\n", feature.ID)
	fmt.Fprintf(out, "  Tables:   %d\n", len(sources))
	if len(metrics) > 0 {
		fmt.Fprintf(out, "  Metrics:  %d\n", len(metrics))
	}
	fmt.Fprintf(out, "  Instance: %s\n\n", url)
	for _, s := range sources {
		fmt.Fprintf(out, "    %-40s %s\n", s.Path, s.TableID)
	}
	for _, m := range metrics {
		fmt.Fprintf(out, "    %-40s %s\n", m.Path, m.MetricID)
	}
	if notCloned := skipped.KindCount + skipped.RecipeUnavailable; notCloned > 0 {
		fmt.Fprintf(out, "\n  Not cloned: %d\n", notCloned)
	}
	fmt.Fprintf(out, "\n  Next: cd %s && ronja pipeline status\n", filepath.Base(root))
}
