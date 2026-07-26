// Package app implements TELL's command-line application.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chasebryan/TELL/internal/report"
)

const (
	ExitPass  = 0
	ExitFail  = 1
	ExitError = 2

	defaultTimeout        = 2 * time.Second
	minimumTimeout        = 10 * time.Millisecond
	maximumTimeout        = 60 * time.Second
	defaultMaxOutputBytes = 65_536
	maximumOutputBytes    = 1_048_576
	maximumSeedBytes      = 16 * 1024 * 1024
	cleanupGrace          = 250 * time.Millisecond
	defaultReportPath     = "tell-report-v1.json"
)

const helpText = `Usage:
  tell run --seed PATH --stdin [--timeout 2s] [--max-output-bytes 65536] [--report tell-report-v1.json] -- COMMAND [ARG...]
  tell version
  tell help

TELL deterministically audits a command's black-box rejection behavior.

Safety: TELL is not a sandbox. Targets run with your privileges and may have
filesystem or network side effects.

Report warning: reports contain command arguments and raw target output; review
them before publishing or sharing.
`

const runHelpText = `Usage:
  tell run --seed PATH --stdin [--timeout 2s] [--max-output-bytes 65536] [--report tell-report-v1.json] -- COMMAND [ARG...]

Required:
  --seed PATH              known-valid binary seed (maximum 16 MiB)
  --stdin                  send each exact input through standard input
  --                       end TELL options; the remaining argv is literal
  COMMAND                  target executable

Options:
  --timeout DURATION       per-execution timeout (default 2s; 10ms through 60s)
  --max-output-bytes N     cap for each output stream (default 65536; max 1048576)
  --report PATH            report destination (default tell-report-v1.json)

TELL is not a sandbox. The target runs with your privileges and can have
filesystem or network side effects.

Reports contain command arguments and raw target output; review them before
publishing or sharing.
`

type options struct {
	seedPath      string
	timeout       time.Duration
	maxOutput     int
	reportPath    string
	command       []string
	stdinSelected bool
}

// Main runs one CLI invocation and returns its process exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if err := validateUTF8(args); err != nil {
		return usageError(stderr, err)
	}
	if len(args) == 0 {
		return usageError(stderr, errors.New("a subcommand is required"))
	}

	switch args[0] {
	case "help":
		if len(args) != 1 {
			return usageError(stderr, errors.New("tell help takes no arguments"))
		}
		_, _ = io.WriteString(stdout, helpText)
		return ExitPass
	case "version":
		if len(args) != 1 {
			return usageError(stderr, errors.New("tell version takes no arguments"))
		}
		_, _ = fmt.Fprintf(stdout, "tell %s\n", report.ToolVersion)
		return ExitPass
	case "run":
		if len(args) == 2 && args[1] == "--help" {
			_, _ = io.WriteString(stdout, runHelpText)
			return ExitPass
		}
		parsed, err := parseRun(args[1:])
		if err != nil {
			return usageError(stderr, err)
		}
		return audit(ctx, parsed, stdout, stderr)
	default:
		return usageError(stderr, fmt.Errorf("unknown subcommand %q", args[0]))
	}
}

func validateUTF8(args []string) error {
	for _, argument := range args {
		if !utf8.ValidString(argument) {
			return errors.New("arguments must be valid UTF-8")
		}
	}
	return nil
}

func usageError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "tell: %v\n\n%s", err, helpText)
	return ExitError
}

func parseRun(args []string) (options, error) {
	parsed := options{
		timeout:    defaultTimeout,
		maxOutput:  defaultMaxOutputBytes,
		reportPath: defaultReportPath,
	}
	seen := make(map[string]bool)
	separator := false

	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			separator = true
			parsed.command = append([]string(nil), args[index+1:]...)
			break
		}

		name, value, inline := splitOption(argument)
		switch name {
		case "--seed", "--timeout", "--max-output-bytes", "--report":
			if seen[name] {
				return options{}, fmt.Errorf("%s may be specified only once", name)
			}
			seen[name] = true
			if !inline {
				index++
				if index >= len(args) {
					return options{}, fmt.Errorf("%s requires a value", name)
				}
				value = args[index]
			}
			if value == "" {
				return options{}, fmt.Errorf("%s requires a nonempty value", name)
			}
			switch name {
			case "--seed":
				parsed.seedPath = value
			case "--timeout":
				duration, err := parseTimeout(value)
				if err != nil {
					return options{}, err
				}
				parsed.timeout = duration
			case "--max-output-bytes":
				limit, err := parseOutputLimit(value)
				if err != nil {
					return options{}, err
				}
				parsed.maxOutput = limit
			case "--report":
				parsed.reportPath = value
			}
		case "--stdin":
			if inline {
				return options{}, errors.New("--stdin does not take a value")
			}
			if seen[name] {
				return options{}, errors.New("--stdin may be specified only once")
			}
			seen[name] = true
			parsed.stdinSelected = true
		case "--help":
			return options{}, errors.New("--help must be used as tell run --help")
		default:
			if strings.HasPrefix(argument, "-") {
				return options{}, fmt.Errorf("unknown option %q", argument)
			}
			return options{}, fmt.Errorf("unexpected argument %q before --", argument)
		}
	}

	if !separator {
		return options{}, errors.New("literal -- separator is required")
	}
	if parsed.seedPath == "" {
		return options{}, errors.New("--seed is required")
	}
	if !parsed.stdinSelected {
		return options{}, errors.New("--stdin is required in v1")
	}
	if len(parsed.command) == 0 || parsed.command[0] == "" {
		return options{}, errors.New("COMMAND is required after --")
	}
	return parsed, nil
}

func splitOption(argument string) (name, value string, inline bool) {
	if index := strings.IndexByte(argument, '='); index >= 0 {
		return argument[:index], argument[index+1:], true
	}
	return argument, "", false
}

func parseTimeout(value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid --timeout %q: %w", value, err)
	}
	if duration%time.Millisecond != 0 {
		return 0, errors.New("--timeout must be a whole number of milliseconds")
	}
	if duration < minimumTimeout || duration > maximumTimeout {
		return 0, fmt.Errorf("--timeout must be between %s and %s", minimumTimeout, maximumTimeout)
	}
	return duration, nil
}

func parseOutputLimit(value string) (int, error) {
	limit, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid --max-output-bytes %q", value)
	}
	if limit < 0 || limit > maximumOutputBytes {
		return 0, fmt.Errorf("--max-output-bytes must be between 0 and %d", maximumOutputBytes)
	}
	return int(limit), nil
}

func readSeed(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open seed: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maximumSeedBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read seed: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close seed: %w", closeErr)
	}
	if len(data) > maximumSeedBytes {
		return nil, fmt.Errorf("seed exceeds %d-byte limit", maximumSeedBytes)
	}
	return data, nil
}
