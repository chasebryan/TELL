package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/chasebryan/TELL/internal/mutate"
	"github.com/chasebryan/TELL/internal/observe"
	"github.com/chasebryan/TELL/internal/report"
	"github.com/chasebryan/TELL/internal/runner"
)

type outcomeCounts struct {
	accepted      int
	rejected      int
	signaled      int
	timedOut      int
	outputLimited int
}

func audit(ctx context.Context, options options, stdout, stderr io.Writer) int {
	seed, err := readSeed(options.seedPath)
	if err != nil {
		return operationalError(stderr, err)
	}
	configuration := runner.Config{
		Timeout:        options.timeout,
		MaxOutputBytes: options.maxOutput,
		CleanupGrace:   cleanupGrace,
	}

	baselineResult, err := runner.Run(ctx, options.command, seed, configuration)
	if err != nil {
		return runnerError(stderr, "baseline", err)
	}
	if baselineResult.Outcome != runner.OutcomeAccepted || baselineResult.ExitCode == nil || *baselineResult.ExitCode != 0 {
		return operationalError(stderr, fmt.Errorf("baseline is not a valid accepted precondition: outcome=%s", baselineResult.Outcome))
	}

	document := report.NewDocument()
	document.Configuration = report.Configuration{
		Transport:               "stdin",
		MutationProfile:         mutate.Profile,
		TimeoutMS:               options.timeout.Milliseconds(),
		MaxOutputBytesPerStream: options.maxOutput,
	}
	document.Command = append([]string(nil), options.command...)
	document.Seed = report.NewInput(seed)
	document.Baseline = execution(baselineResult)

	classes := observe.NewSet()
	var outcomes outcomeCounts
	counts, err := mutate.ForEach(seed, func(id string, descriptor mutate.Descriptor, candidate []byte, byteLength int64, sha256Hex string) error {
		if err := ctx.Err(); err != nil {
			return &runner.CanceledError{Cause: err}
		}
		result, err := runner.Run(ctx, options.command, candidate, configuration)
		if err != nil {
			return fmt.Errorf("execute %s: %w", id, err)
		}

		caseReport := report.Case{
			ID: id,
			Mutation: report.Mutation{
				Kind:       descriptor.Kind,
				Offset:     descriptor.Offset,
				NewLength:  descriptor.NewLength,
				Mask:       descriptor.Mask,
				DataBase64: descriptor.DataBase64,
			},
			Input: report.Input{
				ByteLength: int(byteLength),
				SHA256Hex:  sha256Hex,
			},
			Execution: execution(result),
		}

		switch result.Outcome {
		case runner.OutcomeAccepted:
			outcomes.accepted++
		case runner.OutcomeRejected:
			if result.ExitCode == nil {
				return fmt.Errorf("%s: rejected result has no exit code", id)
			}
			outcomes.rejected++
			classID, err := classes.Add(id, observe.Observation{
				ExitCode: *result.ExitCode,
				Stdout:   result.Stdout.Data,
				Stderr:   result.Stderr.Data,
			})
			if err != nil {
				return fmt.Errorf("classify %s: %w", id, err)
			}
			caseReport.ObservationClassID = &classID
		case runner.OutcomeSignaled:
			outcomes.signaled++
		case runner.OutcomeTimedOut:
			outcomes.timedOut++
		case runner.OutcomeOutputLimited:
			outcomes.outputLimited++
		default:
			return fmt.Errorf("%s: unknown outcome %q", id, result.Outcome)
		}
		document.Cases = append(document.Cases, caseReport)
		return nil
	})
	if err != nil {
		return runnerError(stderr, "mutation audit", err)
	}
	if counts.Planned > mutate.MaxPlannedCases || counts.Unique == 0 || len(document.Cases) != counts.Unique {
		return operationalError(stderr, errors.New("internal mutation count invariant failed"))
	}
	document.MutationCounts = report.MutationCounts{
		Planned:          counts.Planned,
		Unique:           counts.Unique,
		SkippedUnchanged: counts.SkippedUnchanged,
		SkippedDuplicate: counts.SkippedDuplicate,
	}

	observedClasses := classes.Classes()
	for _, class := range observedClasses {
		stdoutMetadata := report.NewStream(class.Value.Stdout, false)
		stderrMetadata := report.NewStream(class.Value.Stderr, false)
		document.RejectionClasses = append(document.RejectionClasses, report.RejectionClass{
			ID:               class.ID,
			ExitCode:         class.Value.ExitCode,
			StdoutByteLength: len(class.Value.Stdout),
			StdoutSHA256Hex:  stdoutMetadata.SHA256Hex,
			StderrByteLength: len(class.Value.Stderr),
			StderrSHA256Hex:  stderrMetadata.SHA256Hex,
			CaseIDs:          append([]string(nil), class.CaseIDs...),
		})
	}

	document.Reasons = failureReasons(outcomes, len(observedClasses))
	if len(document.Reasons) == 0 {
		document.Verdict = "pass"
	} else {
		document.Verdict = "fail"
	}
	if err := ctx.Err(); err != nil {
		return runnerError(stderr, "audit", &runner.CanceledError{Cause: err})
	}
	if err := report.WriteAtomicContext(ctx, options.reportPath, document); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return runnerError(stderr, "report publication", err)
		}
		return operationalError(stderr, fmt.Errorf("write report: %w", err))
	}
	_, _ = fmt.Fprintln(stdout, summary(document.Verdict, counts.Unique, outcomes, len(observedClasses), options.reportPath))
	if document.Verdict == "pass" {
		return ExitPass
	}
	return ExitFail
}

func execution(result runner.Result) report.Execution {
	termination := report.Termination{
		ExitCode:     result.ExitCode,
		SignalNumber: result.SignalNumber,
	}
	switch result.Outcome {
	case runner.OutcomeAccepted, runner.OutcomeRejected:
		termination.Kind = "exit"
	case runner.OutcomeSignaled:
		termination.Kind = "signal"
	case runner.OutcomeTimedOut:
		termination.Kind = "timeout"
	case runner.OutcomeOutputLimited:
		termination.Kind = "output_limit"
	}
	return report.Execution{
		Outcome:     string(result.Outcome),
		Termination: termination,
		Stdout:      report.NewStream(result.Stdout.Data, result.Stdout.Truncated),
		Stderr:      report.NewStream(result.Stderr.Data, result.Stderr.Truncated),
	}
}

func failureReasons(outcomes outcomeCounts, classCount int) []string {
	reasons := make([]string, 0, 5)
	if outcomes.accepted > 0 {
		reasons = append(reasons, "accepted_mutation")
	}
	if classCount > 1 {
		reasons = append(reasons, "nonuniform_rejection")
	}
	if outcomes.signaled > 0 {
		reasons = append(reasons, "target_signaled")
	}
	if outcomes.timedOut > 0 {
		reasons = append(reasons, "target_timeout")
	}
	if outcomes.outputLimited > 0 {
		reasons = append(reasons, "output_limit_exceeded")
	}
	return reasons
}

func summary(verdict string, cases int, outcomes outcomeCounts, classes int, reportPath string) string {
	if verdict == "pass" {
		return fmt.Sprintf("PASS cases=%d rejected=%d classes=%d report=%s", cases, outcomes.rejected, classes, safeSummaryPath(reportPath))
	}
	parts := []string{
		"FAIL",
		fmt.Sprintf("cases=%d", cases),
		fmt.Sprintf("accepted=%d", outcomes.accepted),
		fmt.Sprintf("rejected=%d", outcomes.rejected),
		fmt.Sprintf("signaled=%d", outcomes.signaled),
		fmt.Sprintf("timed_out=%d", outcomes.timedOut),
		fmt.Sprintf("output_limited=%d", outcomes.outputLimited),
		fmt.Sprintf("classes=%d", classes),
		"report=" + safeSummaryPath(reportPath),
	}
	return strings.Join(parts, " ")
}

func safeSummaryPath(path string) string {
	if strings.IndexFunc(path, func(character rune) bool {
		return unicode.IsControl(character) || character == '\u2028' || character == '\u2029'
	}) >= 0 {
		return strconv.Quote(path)
	}
	return path
}

func runnerError(stderr io.Writer, stage string, err error) int {
	if errors.Is(err, runner.ErrCanceled) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return operationalError(stderr, fmt.Errorf("%s interrupted: %w", stage, err))
	}
	return operationalError(stderr, fmt.Errorf("%s incomplete: %w", stage, err))
}

func operationalError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "tell: %v\n", err)
	return ExitError
}
