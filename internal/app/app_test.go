//go:build linux

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chasebryan/TELL/internal/report"
)

const fixtureEnvironment = "TELL_APP_TEST_FIXTURE"

func TestTargetFixture(t *testing.T) {
	if os.Getenv(fixtureEnvironment) != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || len(os.Args) < separator+3 {
		os.Exit(120)
	}
	arguments := os.Args[separator+1:]
	mode := arguments[0]
	seed, err := base64.StdEncoding.DecodeString(arguments[1])
	if err != nil {
		os.Exit(121)
	}
	extra := arguments[2:]
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(122)
	}
	baseline := bytes.Equal(input, seed)

	uniformReject := func() {
		_, _ = os.Stdout.Write([]byte{0, 0xff, '\n'})
		_, _ = os.Stderr.Write([]byte{'e', 'r', 'r', 0xfe, '\n'})
		os.Exit(23)
	}
	switch mode {
	case "uniform":
		if baseline {
			os.Exit(0)
		}
		uniformReject()
	case "vary":
		if baseline {
			os.Exit(0)
		}
		_, _ = fmt.Fprintf(os.Stdout, "%x", input)
		os.Exit(23)
	case "accept-one":
		if baseline || len(input) == 0 {
			os.Exit(0)
		}
		uniformReject()
	case "signal":
		if baseline {
			os.Exit(0)
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		time.Sleep(time.Second)
		os.Exit(123)
	case "timeout", "timeout-one":
		if baseline {
			os.Exit(0)
		}
		if mode == "timeout-one" && len(input) != 0 {
			uniformReject()
		}
		time.Sleep(10 * time.Second)
		os.Exit(124)
	case "stdout-overflow", "stderr-overflow", "exact-cap":
		if baseline {
			os.Exit(0)
		}
		if len(extra) != 1 {
			os.Exit(125)
		}
		limit, err := strconv.Atoi(extra[0])
		if err != nil {
			os.Exit(126)
		}
		count := limit
		if mode != "exact-cap" {
			count++
		}
		output := bytes.Repeat([]byte{'x'}, count)
		if mode == "stderr-overflow" {
			_, _ = os.Stderr.Write(output)
		} else {
			_, _ = os.Stdout.Write(output)
		}
		os.Exit(23)
	case "literal":
		if len(extra) < 1 {
			os.Exit(127)
		}
		encoded, err := base64.StdEncoding.DecodeString(extra[0])
		if err != nil {
			os.Exit(128)
		}
		var expected []string
		if err := json.Unmarshal(encoded, &expected); err != nil || !reflect.DeepEqual(extra[1:], expected) {
			os.Exit(129)
		}
		if baseline {
			os.Exit(0)
		}
		uniformReject()
	case "context":
		if len(extra) != 3 {
			os.Exit(130)
		}
		cwd, err := os.Getwd()
		if err != nil || cwd != extra[0] || os.Getenv(extra[1]) != extra[2] {
			os.Exit(131)
		}
		if baseline {
			os.Exit(0)
		}
		uniformReject()
	case "baseline-reject":
		os.Exit(9)
	case "baseline-signal":
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		time.Sleep(time.Second)
		os.Exit(132)
	case "baseline-timeout":
		time.Sleep(10 * time.Second)
		os.Exit(133)
	case "baseline-stdout-overflow":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte{'x'}, 6))
		os.Exit(0)
	case "baseline-stderr-overflow":
		_, _ = os.Stderr.Write(bytes.Repeat([]byte{'x'}, 6))
		os.Exit(0)
	default:
		os.Exit(134)
	}
}

func TestHelpVersionUsageAndUTF8(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdout: "Usage:\n"},
		{name: "run help", args: []string{"run", "--help"}, wantCode: 0, wantStdout: "Usage:\n"},
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: "tell 1.0.0\n"},
		{name: "missing command", args: nil, wantCode: 2, wantStderr: "tell: a subcommand is required"},
		{name: "unknown", args: []string{"unknown"}, wantCode: 2, wantStderr: "tell: unknown subcommand"},
		{name: "help extra", args: []string{"help", "extra"}, wantCode: 2, wantStderr: "tell help takes no arguments"},
		{name: "version extra", args: []string{"version", "extra"}, wantCode: 2, wantStderr: "tell version takes no arguments"},
		{name: "invalid utf8", args: []string{"run", string([]byte{0xff})}, wantCode: 2, wantStderr: "arguments must be valid UTF-8"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Main(context.Background(), test.args, &stdout, &stderr)
			if code != test.wantCode {
				t.Fatalf("code = %d, want %d", code, test.wantCode)
			}
			if !strings.Contains(stdout.String(), test.wantStdout) {
				t.Fatalf("stdout = %q, want substring %q", stdout.String(), test.wantStdout)
			}
			if !strings.Contains(stderr.String(), test.wantStderr) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), test.wantStderr)
			}
			if strings.Contains(stdout.String(), "\x1b[") || strings.Contains(stderr.String(), "\x1b[") {
				t.Fatal("CLI emitted ANSI color")
			}
		})
	}
}

func TestParseRunContractAndBoundaries(t *testing.T) {
	valid := []string{"--seed=seed.bin", "--stdin", "--timeout", "10ms", "--max-output-bytes=0", "--report", "out.json", "--", "command", "space arg"}
	parsed, err := parseRun(valid)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.seedPath != "seed.bin" || parsed.timeout != 10*time.Millisecond || parsed.maxOutput != 0 || parsed.reportPath != "out.json" || !reflect.DeepEqual(parsed.command, []string{"command", "space arg"}) {
		t.Fatalf("parsed options = %#v", parsed)
	}

	validTimeouts := []string{"10ms", "0.01s", "1500ms", "1.5s", "60s"}
	for _, value := range validTimeouts {
		if _, err := parseTimeout(value); err != nil {
			t.Errorf("parseTimeout(%q): %v", value, err)
		}
	}
	invalidTimeouts := []string{"", "9ms", "60.001s", "10001us", "garbage"}
	for _, value := range invalidTimeouts {
		if _, err := parseTimeout(value); err == nil {
			t.Errorf("parseTimeout(%q) succeeded", value)
		}
	}
	for _, value := range []string{"0", "65536", "1048576"} {
		if _, err := parseOutputLimit(value); err != nil {
			t.Errorf("parseOutputLimit(%q): %v", value, err)
		}
	}
	for _, value := range []string{"-1", "1048577", "1.5", "x"} {
		if _, err := parseOutputLimit(value); err == nil {
			t.Errorf("parseOutputLimit(%q) succeeded", value)
		}
	}

	invalid := [][]string{
		{"--stdin", "--", "cmd"},
		{"--seed", "seed", "--", "cmd"},
		{"--seed", "seed", "--stdin", "cmd"},
		{"--seed", "seed", "--stdin", "--"},
		{"--seed", "seed", "--stdin", "--unknown", "--", "cmd"},
		{"--seed", "seed", "--seed", "other", "--stdin", "--", "cmd"},
		{"--seed", "seed", "--stdin", "--stdin", "--", "cmd"},
		{"--seed", "seed", "--stdin=true", "--", "cmd"},
		{"--seed=", "--stdin", "--", "cmd"},
		{"--seed", "seed", "--stdin", "positional", "--", "cmd"},
	}
	for index, args := range invalid {
		if _, err := parseRun(args); err == nil {
			t.Errorf("invalid case %d succeeded: %#v", index, args)
		}
	}
}

func TestUniformRejectionPassesAndReportsRawBytesDeterministically(t *testing.T) {
	seed := []byte("VALID")
	firstCode, firstStdout, firstStderr, firstBytes, first := runFixtureAudit(t, seed, "uniform", nil, defaultTimeout, defaultMaxOutputBytes)
	if firstCode != ExitPass {
		t.Fatalf("code = %d, stdout=%q stderr=%q", firstCode, firstStdout, firstStderr)
	}
	if first.Verdict != "pass" || len(first.Reasons) != 0 || first.MutationCounts.Unique == 0 || len(first.Cases) != first.MutationCounts.Unique || len(first.RejectionClasses) != 1 {
		t.Fatalf("unexpected report: verdict=%q reasons=%#v counts=%+v cases=%d classes=%d", first.Verdict, first.Reasons, first.MutationCounts, len(first.Cases), len(first.RejectionClasses))
	}
	if !strings.HasPrefix(firstStdout, fmt.Sprintf("PASS cases=%d rejected=%d classes=1 report=", first.MutationCounts.Unique, first.MutationCounts.Unique)) || firstStderr != "" {
		t.Fatalf("summary stdout=%q stderr=%q", firstStdout, firstStderr)
	}
	for _, item := range first.Cases {
		if item.ObservationClassID == nil || *item.ObservationClassID != first.RejectionClasses[0].ID {
			t.Fatalf("case %s class ID = %v", item.ID, item.ObservationClassID)
		}
		stdoutData, err := base64.StdEncoding.DecodeString(item.Execution.Stdout.DataBase64)
		if err != nil || !bytes.Equal(stdoutData, []byte{0, 0xff, '\n'}) {
			t.Fatalf("case %s stdout round trip = %x, err=%v", item.ID, stdoutData, err)
		}
		stderrData, err := base64.StdEncoding.DecodeString(item.Execution.Stderr.DataBase64)
		if err != nil || !bytes.Equal(stderrData, []byte{'e', 'r', 'r', 0xfe, '\n'}) {
			t.Fatalf("case %s stderr round trip = %x, err=%v", item.ID, stderrData, err)
		}
	}

	secondCode, _, _, secondBytes, _ := runFixtureAudit(t, seed, "uniform", nil, defaultTimeout, defaultMaxOutputBytes)
	if secondCode != ExitPass || !bytes.Equal(firstBytes, secondBytes) {
		t.Fatal("identical fixtures did not produce byte-identical reports")
	}
}

func TestCompletedAuditFailureOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		seed       []byte
		extra      []string
		timeout    time.Duration
		limit      int
		wantReason string
	}{
		{name: "different observations", mode: "vary", seed: []byte("VALID"), timeout: defaultTimeout, limit: defaultMaxOutputBytes, wantReason: "nonuniform_rejection"},
		{name: "accepted mutation", mode: "accept-one", seed: []byte("A"), timeout: defaultTimeout, limit: defaultMaxOutputBytes, wantReason: "accepted_mutation"},
		{name: "natural signal", mode: "signal", seed: []byte("A"), timeout: defaultTimeout, limit: defaultMaxOutputBytes, wantReason: "target_signaled"},
		{name: "timeout", mode: "timeout-one", seed: []byte("A"), timeout: defaultTimeout, limit: defaultMaxOutputBytes, wantReason: "target_timeout"},
		{name: "stdout overflow", mode: "stdout-overflow", seed: []byte("A"), extra: []string{"4"}, timeout: defaultTimeout, limit: 4, wantReason: "output_limit_exceeded"},
		{name: "stderr overflow", mode: "stderr-overflow", seed: []byte("A"), extra: []string{"4"}, timeout: defaultTimeout, limit: 4, wantReason: "output_limit_exceeded"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr, _, document := runFixtureAudit(t, test.seed, test.mode, test.extra, test.timeout, test.limit)
			if code != ExitFail || document.Verdict != "fail" || !contains(document.Reasons, test.wantReason) {
				t.Fatalf("code=%d verdict=%q reasons=%#v stdout=%q stderr=%q", code, document.Verdict, document.Reasons, stdout, stderr)
			}
			if !strings.HasPrefix(stdout, "FAIL cases=") || stderr != "" {
				t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
			}
		})
	}
}

func TestExactlyAtOutputCapDoesNotOverflow(t *testing.T) {
	code, _, stderr, _, document := runFixtureAudit(t, []byte("A"), "exact-cap", []string{"4"}, defaultTimeout, 4)
	if code != ExitPass || document.Verdict != "pass" {
		t.Fatalf("code=%d verdict=%q reasons=%#v stderr=%q", code, document.Verdict, document.Reasons, stderr)
	}
	for _, item := range document.Cases {
		if item.Execution.Stdout.CapturedByteLength != 4 || item.Execution.Stdout.Truncated {
			t.Fatalf("case %s stdout=%+v", item.ID, item.Execution.Stdout)
		}
	}
}

func TestBaselineAndLaunchErrorsDoNotPublish(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		timeout time.Duration
		limit   int
	}{
		{name: "rejection", mode: "baseline-reject", timeout: defaultTimeout, limit: defaultMaxOutputBytes},
		{name: "signal", mode: "baseline-signal", timeout: defaultTimeout, limit: defaultMaxOutputBytes},
		{name: "timeout", mode: "baseline-timeout", timeout: 10 * time.Millisecond, limit: defaultMaxOutputBytes},
		{name: "stdout overflow", mode: "baseline-stdout-overflow", timeout: defaultTimeout, limit: 4},
		{name: "stderr overflow", mode: "baseline-stderr-overflow", timeout: defaultTimeout, limit: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			seedPath := writeSeed(t, directory, []byte("VALID"))
			reportPath := filepath.Join(directory, "report.json")
			if err := os.WriteFile(reportPath, []byte("existing"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(fixtureEnvironment, "1")
			options := fixtureOptions(seedPath, reportPath, []byte("VALID"), test.mode, nil, test.timeout, test.limit)
			var stdout, stderr bytes.Buffer
			if code := audit(context.Background(), options, &stdout, &stderr); code != ExitError {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			assertFileContents(t, reportPath, "existing")
		})
	}

	t.Run("missing command", func(t *testing.T) {
		directory := t.TempDir()
		seedPath := writeSeed(t, directory, []byte("VALID"))
		reportPath := filepath.Join(directory, "report.json")
		if err := os.WriteFile(reportPath, []byte("existing"), 0o600); err != nil {
			t.Fatal(err)
		}
		options := options{seedPath: seedPath, stdinSelected: true, timeout: defaultTimeout, maxOutput: 10, reportPath: reportPath, command: []string{filepath.Join(directory, "missing")}}
		if code := audit(context.Background(), options, io.Discard, io.Discard); code != ExitError {
			t.Fatalf("code = %d", code)
		}
		assertFileContents(t, reportPath, "existing")
	})

	t.Run("non executable command", func(t *testing.T) {
		directory := t.TempDir()
		seedPath := writeSeed(t, directory, []byte("VALID"))
		reportPath := filepath.Join(directory, "report.json")
		commandPath := filepath.Join(directory, "not-executable")
		if err := os.WriteFile(commandPath, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		options := options{seedPath: seedPath, stdinSelected: true, timeout: defaultTimeout, maxOutput: 10, reportPath: reportPath, command: []string{commandPath}}
		if code := audit(context.Background(), options, io.Discard, io.Discard); code != ExitError {
			t.Fatalf("code = %d", code)
		}
		if _, err := os.Stat(reportPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("report exists or stat failed: %v", err)
		}
	})
}

func TestLiteralArgvExactStdinEOFEnvironmentAndWorkingDirectory(t *testing.T) {
	directory := t.TempDir()
	sentinel := filepath.Join(directory, "shell-expanded")
	literal := []string{
		"space value",
		`"double" and 'single'`,
		"*",
		"$HOME",
		"semi;colon",
		`back\\slash`,
		"$(touch " + sentinel + ")",
	}
	encoded, err := json.Marshal(literal)
	if err != nil {
		t.Fatal(err)
	}
	extra := append([]string{base64.StdEncoding.EncodeToString(encoded)}, literal...)
	code, _, stderr, _, _ := runFixtureAudit(t, []byte{0, 1, 0xff, '\n'}, "literal", extra, defaultTimeout, defaultMaxOutputBytes)
	if code != ExitPass {
		t.Fatalf("literal audit code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shell substitution sentinel exists or stat failed: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	const environmentName = "TELL_FIXTURE_INHERITED"
	const environmentValue = "exact inherited value"
	t.Setenv(environmentName, environmentValue)
	code, _, stderr, _, _ = runFixtureAudit(t, []byte("VALID"), "context", []string{cwd, environmentName, environmentValue}, defaultTimeout, defaultMaxOutputBytes)
	if code != ExitPass {
		t.Fatalf("context audit code=%d stderr=%q", code, stderr)
	}
}

func TestInterruptionAndReportFailureLeaveExistingDestination(t *testing.T) {
	t.Run("interruption", func(t *testing.T) {
		directory := t.TempDir()
		seed := []byte("A")
		seedPath := writeSeed(t, directory, seed)
		reportPath := filepath.Join(directory, "report.json")
		if err := os.WriteFile(reportPath, []byte("existing"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(fixtureEnvironment, "1")
		options := fixtureOptions(seedPath, reportPath, seed, "timeout", nil, maximumTimeout, defaultMaxOutputBytes)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		if code := audit(ctx, options, io.Discard, io.Discard); code != ExitError {
			t.Fatalf("code = %d", code)
		}
		assertFileContents(t, reportPath, "existing")
	})

	t.Run("report publication", func(t *testing.T) {
		directory := t.TempDir()
		seed := []byte("A")
		seedPath := writeSeed(t, directory, seed)
		reportPath := filepath.Join(directory, "report.json")
		if err := os.Mkdir(reportPath, 0o700); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(reportPath, "sentinel")
		if err := os.WriteFile(sentinel, []byte("existing"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(fixtureEnvironment, "1")
		options := fixtureOptions(seedPath, reportPath, seed, "uniform", nil, defaultTimeout, defaultMaxOutputBytes)
		if code := audit(context.Background(), options, io.Discard, io.Discard); code != ExitError {
			t.Fatalf("code = %d", code)
		}
		assertFileContents(t, sentinel, "existing")
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 { // seed and destination directory
			t.Fatalf("temporary report artifact remained: %#v", entries)
		}
	})
}

func TestSeedLimitAndSetupFailure(t *testing.T) {
	directory := t.TempDir()
	reportPath := filepath.Join(directory, "report.json")
	if err := os.WriteFile(reportPath, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := options{seedPath: filepath.Join(directory, "missing"), reportPath: reportPath}
	if code := audit(context.Background(), options, io.Discard, io.Discard); code != ExitError {
		t.Fatalf("missing seed code = %d", code)
	}
	assertFileContents(t, reportPath, "existing")

	tooLarge := filepath.Join(directory, "too-large")
	file, err := os.Create(tooLarge)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maximumSeedBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	options.seedPath = tooLarge
	if code := audit(context.Background(), options, io.Discard, io.Discard); code != ExitError {
		t.Fatalf("oversize seed code = %d", code)
	}
	assertFileContents(t, reportPath, "existing")

	exact := filepath.Join(directory, "exact")
	file, err = os.Create(exact)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maximumSeedBytes); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := readSeed(exact)
	if err != nil || len(data) != maximumSeedBytes {
		t.Fatalf("exact-limit seed len=%d err=%v", len(data), err)
	}
}

func TestFailureReasonOrder(t *testing.T) {
	got := failureReasons(outcomeCounts{accepted: 1, signaled: 1, timedOut: 1, outputLimited: 1}, 2)
	want := []string{"accepted_mutation", "nonuniform_rejection", "target_signaled", "target_timeout", "output_limit_exceeded"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reasons = %#v, want %#v", got, want)
	}
}

func TestSummaryEscapesControlCharactersInReportPath(t *testing.T) {
	path := "report\n\x1b[31m\u2028.json"
	got := summary("fail", 1, outcomeCounts{accepted: 1}, 0, path)
	if strings.ContainsAny(got, "\n\r\x1b") || strings.ContainsRune(got, '\u2028') {
		t.Fatalf("summary contains a line break or terminal control: %q", got)
	}
	if !strings.Contains(got, `report="report\n\x1b[31m\u2028.json"`) {
		t.Fatalf("summary path was not escaped: %q", got)
	}
	ordinary := summary("pass", 1, outcomeCounts{rejected: 1}, 1, "directory/report file.json")
	if !strings.HasSuffix(ordinary, "report=directory/report file.json") {
		t.Fatalf("ordinary report path changed: %q", ordinary)
	}
}

func runFixtureAudit(t *testing.T, seed []byte, mode string, extra []string, timeout time.Duration, limit int) (int, string, string, []byte, report.Document) {
	t.Helper()
	directory := t.TempDir()
	seedPath := writeSeed(t, directory, seed)
	reportPath := filepath.Join(directory, "report.json")
	t.Setenv(fixtureEnvironment, "1")
	options := fixtureOptions(seedPath, reportPath, seed, mode, extra, timeout, limit)
	var stdout, stderr bytes.Buffer
	code := audit(context.Background(), options, &stdout, &stderr)
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report after code %d (stderr %q): %v", code, stderr.String(), err)
	}
	info, err := os.Stat(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("report mode = %#o, want 0600", info.Mode().Perm())
	}
	var document report.Document
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode report: %v\n%s", err, data)
	}
	return code, stdout.String(), stderr.String(), data, document
}

func fixtureOptions(seedPath, reportPath string, seed []byte, mode string, extra []string, timeout time.Duration, limit int) options {
	command := []string{
		os.Args[0],
		"-test.run=^TestTargetFixture$",
		"--",
		mode,
		base64.StdEncoding.EncodeToString(seed),
	}
	command = append(command, extra...)
	return options{
		seedPath:      seedPath,
		stdinSelected: true,
		timeout:       timeout,
		maxOutput:     limit,
		reportPath:    reportPath,
		command:       command,
	}
}

func writeSeed(t *testing.T, directory string, seed []byte) string {
	t.Helper()
	path := filepath.Join(directory, "seed.bin")
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
