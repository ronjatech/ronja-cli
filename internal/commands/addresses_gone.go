package commands

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// RENAME DETECTION and the reporting of the address declarations: the half of
// addresses.go that reads what the evaluation left behind — a bound path with
// no file and no declaration — and says so, in status and in a refusal.

// goneAddress is a bound path with no file and no declaration covering it —
// what rename detection finds.
type goneAddress struct {
	Path string
	Kind string
	ID   string
	// New are the files of the same kind with no row behind them: the likely
	// other half of a rename. Named in the message only when there are any.
	New []string
}

// goneAddresses is rename detection: every address this stack has a row bound
// to whose file is gone, AFTER the declarations were applied — so a declared
// move or removal is never reported, and an undeclared one always is.
//
// It visits the STATE, not the local baseline, which is the point: a fresh CI
// checkout has no .ronja/ at all, and a metric's only record of its row is the
// lock's metrics map, which nothing else iterates. A deleted metric file used to
// orphan its metric without a word.
func (f *folder) goneAddresses() ([]goneAddress, error) {
	if !wfdir.KeysRowsByFile(f.Kind) {
		return nil, nil
	}
	// Against the state AFTER the declarations, whoever the caller is: a gone
	// address read before them would report every declared move as an orphan.
	if _, err := f.resolveAddresses(); err != nil {
		return nil, err
	}
	onDisk, err := f.addressUnitsOnDisk()
	if err != nil {
		return nil, err
	}
	bound := map[string]goneAddress{}
	for p, id := range f.Binding.Tables {
		if id != "" {
			bound[p] = goneAddress{Path: p, Kind: wfdir.AddressTable, ID: id}
		}
	}
	for p, id := range f.Binding.Automations {
		if id != "" {
			bound[p] = goneAddress{Path: p, Kind: wfdir.AddressAutomation, ID: id}
		}
	}
	if f.Kind.Name == wfdir.KindPipeline {
		var metricPaths []string
		if f.Stack != "" {
			metricPaths = f.Lock.MetricPaths(f.Stack)
		} else if inst := f.State.For(f.Key); inst != nil {
			for p, m := range inst.Metrics {
				if m.MetricID != "" {
					metricPaths = append(metricPaths, p)
				}
			}
		}
		for _, p := range metricPaths {
			id, _, _ := f.live(f.State.For(f.Key)).metricSeen(p)
			bound[p] = goneAddress{Path: p, Kind: wfdir.AddressMetric, ID: id}
		}
	}

	// The files with no row behind them, per kind — the other half of a rename.
	newOf := map[string][]string{}
	var metricFiles []pipelineMetricFile
	for p, kind := range onDisk {
		switch kind {
		case wfdir.AddressMetric:
			alias, _ := wfdir.MetricAlias(p)
			metricFiles = append(metricFiles, pipelineMetricFile{Path: p, Alias: alias})
		default:
			if f.boundAddress(kind, p) == "" {
				newOf[kind] = append(newOf[kind], p)
			}
		}
	}
	for _, m := range resolveMetricIdentities(f, f.live(f.State.For(f.Key)), metricFiles) {
		if m.MetricID == "" {
			newOf[wfdir.AddressMetric] = append(newOf[wfdir.AddressMetric], m.Path)
		}
	}

	var out []goneAddress
	for p, g := range bound {
		if _, present := onDisk[p]; present {
			continue
		}
		g.New = append([]string(nil), newOf[g.Kind]...)
		sort.Strings(g.New)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// describeGone is rename detection's sentence for one address, in the one
// wording the push's note and apply's refusal share:
//
//	orders.sql is bound but gone, and sales_orders.sql is new — add a "moved"
//	entry to ronja.json if this is a rename, or a "removed" entry if orders.sql
//	was dropped on purpose.
//
// The new file is named only when there is one; a delete with nothing beside it
// is not a rename candidate, and naming nothing would read as a typo.
func describeGone(g goneAddress) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is bound but gone", g.Path)
	switch len(g.New) {
	case 0:
	case 1:
		fmt.Fprintf(&b, ", and %s is new", g.New[0])
	default:
		fmt.Fprintf(&b, ", and %s are new", strings.Join(g.New, ", "))
	}
	fmt.Fprintf(&b, " — add a \"moved\" entry to %s if this is a rename, or a \"removed\" entry if %s was dropped on purpose",
		wfdir.ManifestName, g.Path)
	return b.String()
}

// goneRefusal is rename detection as `sync apply` refuses it: every gone address
// in the folder, and what happens to the rows meanwhile.
func goneRefusal(gone []goneAddress) string {
	lines := make([]string, 0, len(gone)+1)
	for _, g := range gone {
		lines = append(lines, describeGone(g)+".")
	}
	lines = append(lines, fmt.Sprintf("Apply never turns a rename into a create plus an orphan, and never deletes because a file is missing; the %s bound there %s left alone until one of the two is declared.",
		agree(len(gone), "row", "rows"), agree(len(gone), "is", "are")))
	return strings.Join(lines, "\n  ")
}

// printAddressStatus is the `status` commands' lines for the address
// declarations: pending moves and removals (status writes nothing, so every one
// it shows is still to be written by a push), rename detection, and a
// declaration that cannot be applied as it stands.
func printAddressStatus(out *os.File, moved []addressMove, removed []addressRemoval, gone []string, problem string) {
	for _, m := range moved {
		fmt.Fprintf(out, "    %-9s %s → %s (%s %s; written by the next push)\n", "moves", m.From, m.To, m.Kind, m.ID)
	}
	for _, r := range removed {
		fmt.Fprintf(out, "    %-9s %s (%s %s is left as it is; written by the next push)\n", "unmanages", r.Path, r.Kind, r.ID)
	}
	for _, g := range gone {
		fmt.Fprintf(out, "    %-9s %s\n", "gone", g)
	}
	if problem != "" {
		fmt.Fprintf(out, "    %-9s %s\n", "refused", problem)
	}
}
