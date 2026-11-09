package commands

import (
	"errors"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// applyFolderOut is what applyFolder learns beside its verdict: metric
// adoptions and the per-unit outcomes the per-file loops report.
type applyFolderOut struct {
	adopted  map[string]string
	outcomes map[string]unitOutcome
}

type unitOutcome struct {
	outcome, detail string
}

func (o *applyFolderOut) set(path, outcome, detail string) {
	if o.outcomes == nil {
		o.outcomes = map[string]unitOutcome{}
	}
	o.outcomes[path] = unitOutcome{outcome: outcome, detail: detail}
}

// automationFiles records the outcome of each file the automation loop reached.
func (o *applyFolderOut) automationFiles(push *automationPushResult) {
	if push == nil {
		return
	}
	for _, file := range push.Files {
		switch file.Outcome {
		case automationOutcomeCreated, automationOutcomeUpdated, automationOutcomeUnchanged, automationOutcomeDeleted:
			o.set(file.Path, unitOutcomeApplied, "")
		case automationOutcomeRefused:
			o.set(file.Path, unitOutcomeRefused, file.Error)
		default:
			o.set(file.Path, unitOutcomeNotAttempted, file.Error)
		}
	}
}

// pipelinePushed records each file the pipeline push refused, and each unit
// it left live with no draft: a table or metric live already held, and a docs
// sidecar, which writes to the live row. A staged unit's outcome is the
// publish's.
func (o *applyFolderOut) pipelinePushed(push *pipelinePushResult) {
	if push == nil {
		return
	}
	for _, file := range push.Files {
		switch file.Outcome {
		case pushOutcomeRefused, pushOutcomeBuildFailed, pushOutcomeDocsFailed:
			o.set(file.Path, unitOutcomeRefused, file.Error)
		}
	}
	for _, path := range push.liveAlready {
		o.set(path, unitOutcomeApplied, "")
	}
	for _, metric := range push.Metrics {
		if metric.Outcome == metricOutcomeUpToDate && (metric.Tags == nil || metric.Tags.Outcome != tagOutcomeFailed) {
			o.set(metric.Path, unitOutcomeApplied, "")
		}
	}
	for _, doc := range push.Docs {
		switch doc.Outcome {
		case docsOutcomeWritten, docsOutcomeUpToDate, docsOutcomeNothing:
			o.set(doc.Path, unitOutcomeApplied, "")
		}
	}
}

// pipelinePublished records each draft the pipeline publish committed, and
// each it did not, with its reason.
func (o *applyFolderOut) pipelinePublished(pub *pipelinePublishResult) {
	if pub == nil {
		return
	}
	record := func(path, outcome, reason string) {
		switch outcome {
		case outcomePublished:
			o.set(path, unitOutcomeApplied, "")
		case pushOutcomeRefused, outcomeConflict:
			o.set(path, unitOutcomeRefused, reason)
		}
	}
	for _, file := range pub.Files {
		record(file.Path, file.Outcome, file.Error)
	}
	for _, metric := range pub.Metrics {
		record(metric.Path, metric.Outcome, metric.Error)
	}
}

// settleUnitOutcomes gives each unit of a folder that ran its outcome: what
// the loop reported for it, else what the folder's result says about it.
func settleUnitOutcomes(line *syncApplyFolderReport, kind wfdir.Kind, known map[string]unitOutcome, err error) {
	var checksErr *checksNotAppliedError
	var tagsErr *tagsNotAppliedError
	sqlLive := errors.As(err, &checksErr) || errors.As(err, &tagsErr)
	for i := range line.Units {
		u := &line.Units[i]
		if o, ok := known[u.Path]; ok {
			u.Outcome, u.OutcomeDetail = o.outcome, o.detail
			continue
		}
		switch {
		case line.Verdict == verdictApplied, sqlLive && kind.Name == wfdir.KindPipeline:
			u.Outcome = unitOutcomeApplied
		case line.Verdict == verdictUnknown:
			// The instance did not answer, or the run was interrupted mid-write:
			// the write may have landed.
		case line.Reason == syncReasonInterrupted:
			u.Outcome = unitOutcomeNotAttempted
		case wfdir.KeysRowsByFile(kind):
			u.Outcome = unitOutcomeNotAttempted
		default:
			u.Outcome, u.OutcomeDetail = unitOutcomeRefused, line.Detail
		}
	}
}

func markUnitsNotAttempted(line *syncApplyFolderReport) {
	for i := range line.Units {
		if line.Units[i].Outcome == "" {
			line.Units[i].Outcome = unitOutcomeNotAttempted
		}
	}
}
