//go:build linux

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

const fixtureMarker = "__TELL_RUNNER_FIXTURE__"

func fixtureCommand(mode string, args ...string) []string {
	argv := []string{os.Args[0], "-test.run=^TestRunnerFixture$", "--", fixtureMarker, mode}
	return append(argv, args...)
}

func testConfig(timeout time.Duration, outputBytes int) Config {
	return Config{
		Timeout:        timeout,
		MaxOutputBytes: outputBytes,
		CleanupGrace:   DefaultCleanupGrace,
	}
}

func TestRunExactStdinAndEOF(t *testing.T) {
	input := []byte{0x00, 0xff, 'a', '\n', 0x80, 0x00, 'z'}
	result, err := Run(context.Background(), fixtureCommand("echo-stdin"), input, testConfig(2*time.Second, 1024))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != OutcomeAccepted || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("termination = %#v", result)
	}
	if !bytes.Equal(result.Stdout.Data, input) {
		t.Fatalf("stdout = %x, want %x", result.Stdout.Data, input)
	}
	if len(result.Stderr.Data) != 0 || result.Stdout.Truncated || result.Stderr.Truncated {
		t.Fatalf("unexpected streams: %#v", result)
	}
}

func TestRunLiteralArgvAndArgvZero(t *testing.T) {
	temp := t.TempDir()
	alias := filepath.Join(temp, "fixture command")
	fixturePath, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fixturePath, alias); err != nil {
		t.Fatal(err)
	}
	shellSentinel := filepath.Join(temp, "shell-sentinel")
	literals := []string{
		"has spaces",
		`double"quote`,
		"single'quote",
		"*",
		"$HOME",
		"semi;colon",
		`back\\slash`,
		fmt.Sprintf("$(touch %s)", shellSentinel),
	}
	argv := []string{alias, "-test.run=^TestRunnerFixture$", "--", fixtureMarker, "echo-argv"}
	argv = append(argv, literals...)
	result, err := Run(context.Background(), argv, nil, testConfig(2*time.Second, 4096))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want, err := json.Marshal(argv)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Stdout.Data, want) {
		t.Fatalf("argv bytes = %s, want %s", result.Stdout.Data, want)
	}
	if _, err := os.Stat(shellSentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("command-substitution text was executed: %v", err)
	}
}

func TestRunInheritsWorkingDirectoryAndEnvironment(t *testing.T) {
	const environmentValue = "exact inherited value"
	t.Setenv("TELL_RUNNER_TEST_ENV", environmentValue)
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), fixtureCommand("cwd-env"), nil, testConfig(2*time.Second, 4096))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := append([]byte(workingDirectory), 0)
	want = append(want, environmentValue...)
	if !bytes.Equal(result.Stdout.Data, want) {
		t.Fatalf("cwd/environment bytes = %q, want %q", result.Stdout.Data, want)
	}
}

func TestRunExitAndSignalClassification(t *testing.T) {
	tests := []struct {
		name     string
		argv     []string
		outcome  Outcome
		exitCode *int
		signal   *int
	}{
		{name: "accepted", argv: fixtureCommand("exit", "0"), outcome: OutcomeAccepted, exitCode: intPointer(0)},
		{name: "rejected", argv: fixtureCommand("exit", "23"), outcome: OutcomeRejected, exitCode: intPointer(23)},
		{name: "normal exit 137", argv: fixtureCommand("exit", "137"), outcome: OutcomeRejected, exitCode: intPointer(137)},
		{name: "natural signal", argv: fixtureCommand("signal", strconv.Itoa(int(syscall.SIGTERM))), outcome: OutcomeSignaled, signal: intPointer(int(syscall.SIGTERM))},
		{name: "natural SIGKILL", argv: fixtureCommand("signal", strconv.Itoa(int(syscall.SIGKILL))), outcome: OutcomeSignaled, signal: intPointer(int(syscall.SIGKILL))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := Run(context.Background(), test.argv, nil, testConfig(2*time.Second, 1024))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.Outcome != test.outcome || !equalOptionalInt(result.ExitCode, test.exitCode) || !equalOptionalInt(result.SignalNumber, test.signal) {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestRunTimeout(t *testing.T) {
	started := time.Now()
	result, err := Run(context.Background(), fixtureCommand("wait"), nil, testConfig(100*time.Millisecond, 1024))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != OutcomeTimedOut || result.ExitCode != nil || result.SignalNumber != nil {
		t.Fatalf("result = %#v", result)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("timeout cleanup took %v", elapsed)
	}
}

func TestRunTimeoutKillsDirectChildThatChangesProcessGroup(t *testing.T) {
	result, err := Run(context.Background(), fixtureCommand("move-group-and-wait"), nil, testConfig(150*time.Millisecond, 1024))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != OutcomeTimedOut {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := Run(ctx, fixtureCommand("wait"), nil, testConfig(5*time.Second, 1024))
	if !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
}

func TestRunCallerDeadlineCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := Run(ctx, fixtureCommand("wait"), nil, testConfig(5*time.Second, 1024))
	if !errors.Is(err, ErrCanceled) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline cancellation", err)
	}
	if got := err.Error(); got == "" {
		t.Fatal("cancellation error has an empty message")
	}
}

func TestRunOutputLimits(t *testing.T) {
	const capBytes = 64
	tests := []struct {
		name            string
		mode            string
		count           int
		outcome         Outcome
		stdoutTruncated bool
		stderrTruncated bool
		stdoutLength    int
		stderrLength    int
	}{
		{name: "stdout exact cap", mode: "stdout", count: capBytes, outcome: OutcomeAccepted, stdoutLength: capBytes},
		{name: "stderr exact cap", mode: "stderr", count: capBytes, outcome: OutcomeAccepted, stderrLength: capBytes},
		{name: "stdout cap plus one", mode: "stdout", count: capBytes + 1, outcome: OutcomeOutputLimited, stdoutTruncated: true, stdoutLength: capBytes},
		{name: "stderr cap plus one", mode: "stderr", count: capBytes + 1, outcome: OutcomeOutputLimited, stderrTruncated: true, stderrLength: capBytes},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := Run(context.Background(), fixtureCommand(test.mode, strconv.Itoa(test.count)), nil, testConfig(2*time.Second, capBytes))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.Outcome != test.outcome || result.Stdout.Truncated != test.stdoutTruncated || result.Stderr.Truncated != test.stderrTruncated {
				t.Fatalf("result = %#v", result)
			}
			if len(result.Stdout.Data) != test.stdoutLength || len(result.Stderr.Data) != test.stderrLength {
				t.Fatalf("stream lengths = (%d, %d)", len(result.Stdout.Data), len(result.Stderr.Data))
			}
		})
	}
}

func TestRunZeroOutputLimit(t *testing.T) {
	for _, test := range []struct {
		name      string
		count     string
		outcome   Outcome
		truncated bool
	}{
		{name: "empty", count: "0", outcome: OutcomeAccepted},
		{name: "one byte", count: "1", outcome: OutcomeOutputLimited, truncated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := Run(context.Background(), fixtureCommand("stdout", test.count), nil, testConfig(2*time.Second, 0))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.Outcome != test.outcome || result.Stdout.Truncated != test.truncated || len(result.Stdout.Data) != 0 {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestRunOutputLimitWinsOverInducedSIGKILL(t *testing.T) {
	result, err := Run(context.Background(), fixtureCommand("stdout-and-wait", "65"), nil, testConfig(2*time.Second, 64))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != OutcomeOutputLimited || !result.Stdout.Truncated || len(result.Stdout.Data) != 64 {
		t.Fatalf("result = %#v", result)
	}
	if result.ExitCode != nil || result.SignalNumber != nil {
		t.Fatalf("induced SIGKILL leaked into termination fields: %#v", result)
	}
}

func TestRunDrainsStdoutAndStderrConcurrently(t *testing.T) {
	const streamBytes = 256 * 1024
	result, err := Run(
		context.Background(),
		fixtureCommand("both-streams", strconv.Itoa(streamBytes)),
		nil,
		testConfig(3*time.Second, streamBytes),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != OutcomeAccepted || result.Stdout.Truncated || result.Stderr.Truncated {
		t.Fatalf("result = %#v", result)
	}
	if !bytes.Equal(result.Stdout.Data, bytes.Repeat([]byte{'o'}, streamBytes)) {
		t.Fatal("stdout was not captured exactly")
	}
	if !bytes.Equal(result.Stderr.Data, bytes.Repeat([]byte{'e'}, streamBytes)) {
		t.Fatal("stderr was not captured exactly")
	}
}

func TestRunEarlyEPIPEIsNonfatal(t *testing.T) {
	input := bytes.Repeat([]byte{0xa5}, 1<<20)
	result, err := Run(context.Background(), fixtureCommand("exit", "0"), input, testConfig(2*time.Second, 1024))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != OutcomeAccepted {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunTimeoutKillsDescendantProcessGroup(t *testing.T) {
	temp := t.TempDir()
	ready := filepath.Join(temp, "ready")
	sentinel := filepath.Join(temp, "sentinel")
	result, err := Run(
		context.Background(),
		fixtureCommand("spawn-delayed-sentinel", ready, sentinel, "900ms"),
		nil,
		testConfig(500*time.Millisecond, 1024),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != OutcomeTimedOut {
		t.Fatalf("result = %#v", result)
	}
	if !pollUntil(2*time.Second, func() bool { _, err := os.Stat(ready); return err == nil }) {
		t.Fatal("descendant never wrote readiness marker")
	}
	if !remainsAbsentFor(sentinel, 700*time.Millisecond) {
		t.Fatal("timed-out descendant wrote sentinel")
	}
}

func TestRunPostWaitCleanupKillsDescendantProcessGroup(t *testing.T) {
	temp := t.TempDir()
	ready := filepath.Join(temp, "ready")
	sentinel := filepath.Join(temp, "sentinel")
	result, err := Run(
		context.Background(),
		fixtureCommand("spawn-and-exit-delayed-sentinel", ready, sentinel, "3s"),
		nil,
		testConfig(2*time.Second, 1024),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != OutcomeAccepted {
		t.Fatalf("result = %#v", result)
	}
	if !pollUntil(2*time.Second, func() bool { _, err := os.Stat(ready); return err == nil }) {
		t.Fatal("descendant never wrote readiness marker")
	}
	if !remainsAbsentFor(sentinel, 4*time.Second) {
		t.Fatal("descendant survived post-Wait process-group cleanup")
	}
}

func TestRunEscapedPipeHolderCannotHang(t *testing.T) {
	temp := t.TempDir()
	ready := filepath.Join(temp, "ready")
	done := filepath.Join(temp, "done")
	started := time.Now()
	result, err := Run(
		context.Background(),
		fixtureCommand("spawn-escaped-pipe-holder", ready, done, "3s"),
		bytes.Repeat([]byte{0x5a}, 1<<20),
		testConfig(2*time.Second, 1024),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != OutcomeAccepted {
		t.Fatalf("result = %#v", result)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("inherited pipe delayed Run for %v", elapsed)
	}
	if !pollUntil(4*time.Second, func() bool { _, err := os.Stat(done); return err == nil }) {
		t.Fatal("escaped fixture did not exit normally")
	}
}

func TestRunLaunchErrors(t *testing.T) {
	if _, err := Run(context.Background(), []string{filepath.Join(t.TempDir(), "missing")}, nil, testConfig(time.Second, 1024)); err == nil {
		t.Fatal("missing executable succeeded")
	}
	nonExecutable := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(nonExecutable, []byte("not an executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), []string{nonExecutable}, nil, testConfig(time.Second, 1024)); err == nil {
		t.Fatal("non-executable command succeeded")
	}
}

func TestRunRejectsInvalidConfigurationAndArgv(t *testing.T) {
	valid := fixtureCommand("exit", "0")
	tests := []struct {
		name string
		argv []string
		cfg  Config
	}{
		{name: "empty argv", cfg: testConfig(time.Second, 1)},
		{name: "empty command", argv: []string{""}, cfg: testConfig(time.Second, 1)},
		{name: "invalid utf8", argv: []string{os.Args[0], string([]byte{0xff})}, cfg: testConfig(time.Second, 1)},
		{name: "short timeout", argv: valid, cfg: testConfig(time.Millisecond, 1)},
		{name: "long timeout", argv: valid, cfg: testConfig(61*time.Second, 1)},
		{name: "fractional millisecond", argv: valid, cfg: testConfig(10*time.Millisecond+time.Nanosecond, 1)},
		{name: "negative cap", argv: valid, cfg: testConfig(time.Second, -1)},
		{name: "large cap", argv: valid, cfg: testConfig(time.Second, MaxOutputBytes+1)},
		{name: "wrong cleanup grace", argv: valid, cfg: Config{Timeout: time.Second, MaxOutputBytes: 1, CleanupGrace: time.Second}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Run(context.Background(), test.argv, nil, test.cfg); err == nil {
				t.Fatal("Run succeeded")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, valid, nil, testConfig(time.Second, 1)); !errors.Is(err, ErrCanceled) {
		t.Fatalf("pre-start cancellation = %v", err)
	}
}

func TestRunDoesNotLeakLocalDescriptorsOrGoroutines(t *testing.T) {
	beforeFDs := countFDs(t)
	beforeGoroutines := runtime.NumGoroutine()
	for range 20 {
		result, err := Run(context.Background(), fixtureCommand("exit", "0"), nil, testConfig(5*time.Second, 16))
		if err != nil || result.Outcome != OutcomeAccepted {
			t.Fatalf("Run = (%#v, %v)", result, err)
		}
	}
	if result, err := Run(context.Background(), fixtureCommand("wait"), nil, testConfig(100*time.Millisecond, 16)); err != nil || result.Outcome != OutcomeTimedOut {
		t.Fatalf("timeout cleanup = (%#v, %v)", result, err)
	}
	if result, err := Run(context.Background(), fixtureCommand("stdout", "17"), nil, testConfig(2*time.Second, 16)); err != nil || result.Outcome != OutcomeOutputLimited {
		t.Fatalf("output-limit cleanup = (%#v, %v)", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelTimer := time.AfterFunc(100*time.Millisecond, cancel)
	if _, err := Run(ctx, fixtureCommand("wait"), nil, testConfig(2*time.Second, 16)); !errors.Is(err, ErrCanceled) {
		t.Fatalf("cancellation cleanup error = %v", err)
	}
	cancelTimer.Stop()
	cancel()
	if result, err := Run(context.Background(), fixtureCommand("exit", "0"), bytes.Repeat([]byte{0xa5}, 1<<20), testConfig(2*time.Second, 16)); err != nil || result.Outcome != OutcomeAccepted {
		t.Fatalf("EPIPE cleanup = (%#v, %v)", result, err)
	}
	temp := t.TempDir()
	ready := filepath.Join(temp, "ready")
	done := filepath.Join(temp, "done")
	if result, err := Run(context.Background(), fixtureCommand("spawn-escaped-pipe-holder", ready, done, "2s"), nil, testConfig(3*time.Second, 16)); err != nil || result.Outcome != OutcomeAccepted {
		t.Fatalf("forced-pipe cleanup = (%#v, %v)", result, err)
	}
	if !pollUntil(4*time.Second, func() bool { _, err := os.Stat(done); return err == nil }) {
		t.Fatal("escaped pipe-holder did not finish")
	}
	if !pollUntil(2*time.Second, func() bool { return runtime.NumGoroutine() <= beforeGoroutines+2 }) {
		t.Fatalf("goroutines grew from %d to %d", beforeGoroutines, runtime.NumGoroutine())
	}
	afterFDs := countFDs(t)
	if afterFDs != beforeFDs {
		t.Fatalf("file descriptor count grew from %d to %d", beforeFDs, afterFDs)
	}
}

func intPointer(value int) *int { return &value }

func equalOptionalInt(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func pollUntil(limit time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(limit)
	for {
		if condition() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func remainsAbsentFor(path string, observationWindow time.Duration) bool {
	deadline := time.Now().Add(observationWindow)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

func countFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// TestRunnerFixture turns the Go test binary into the actual subprocess used
// by the integration tests. It intentionally avoids external interpreters and
// committed fixture executables.
func TestRunnerFixture(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == fixtureMarker {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	args := os.Args[index+1:]
	if len(args) == 0 {
		fixtureFail("missing fixture mode")
	}
	mode := args[0]
	args = args[1:]
	switch mode {
	case "echo-stdin":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fixtureFail("read stdin: %v", err)
		}
		if _, err := os.Stdout.Write(data); err != nil {
			fixtureFail("write stdout: %v", err)
		}
	case "echo-argv":
		data, err := json.Marshal(os.Args)
		if err != nil {
			fixtureFail("marshal argv: %v", err)
		}
		if _, err := os.Stdout.Write(data); err != nil {
			fixtureFail("write argv: %v", err)
		}
	case "cwd-env":
		workingDirectory, err := os.Getwd()
		if err != nil {
			fixtureFail("get working directory: %v", err)
		}
		data := append([]byte(workingDirectory), 0)
		data = append(data, os.Getenv("TELL_RUNNER_TEST_ENV")...)
		if _, err := os.Stdout.Write(data); err != nil {
			fixtureFail("write cwd and environment: %v", err)
		}
	case "exit":
		requireFixtureArgs(mode, args, 1)
		code, err := strconv.Atoi(args[0])
		if err != nil {
			fixtureFail("parse exit code: %v", err)
		}
		os.Exit(code)
	case "signal":
		requireFixtureArgs(mode, args, 1)
		number, err := strconv.Atoi(args[0])
		if err != nil {
			fixtureFail("parse signal: %v", err)
		}
		if err := syscall.Kill(os.Getpid(), syscall.Signal(number)); err != nil {
			fixtureFail("signal self: %v", err)
		}
		time.Sleep(time.Second)
		fixtureFail("signal did not terminate process")
	case "wait":
		for {
			time.Sleep(time.Hour)
		}
	case "move-group-and-wait":
		parentGroup, err := syscall.Getpgid(os.Getppid())
		if err != nil {
			fixtureFail("get parent process group: %v", err)
		}
		if err := syscall.Setpgid(0, parentGroup); err != nil {
			fixtureFail("move process group: %v", err)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "stdout", "stderr":
		requireFixtureArgs(mode, args, 1)
		count, err := strconv.Atoi(args[0])
		if err != nil {
			fixtureFail("parse output count: %v", err)
		}
		destination := io.Writer(os.Stdout)
		if mode == "stderr" {
			destination = os.Stderr
		}
		if _, err := destination.Write(bytes.Repeat([]byte{'x'}, count)); err != nil {
			fixtureFail("write %s: %v", mode, err)
		}
	case "both-streams":
		requireFixtureArgs(mode, args, 1)
		count, err := strconv.Atoi(args[0])
		if err != nil {
			fixtureFail("parse output count: %v", err)
		}
		if _, err := os.Stdout.Write(bytes.Repeat([]byte{'o'}, count)); err != nil {
			fixtureFail("write stdout: %v", err)
		}
		if _, err := os.Stderr.Write(bytes.Repeat([]byte{'e'}, count)); err != nil {
			fixtureFail("write stderr: %v", err)
		}
	case "stdout-and-wait":
		requireFixtureArgs(mode, args, 1)
		count, err := strconv.Atoi(args[0])
		if err != nil {
			fixtureFail("parse output count: %v", err)
		}
		if _, err := os.Stdout.Write(bytes.Repeat([]byte{'x'}, count)); err != nil {
			fixtureFail("write stdout: %v", err)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "spawn-delayed-sentinel":
		requireFixtureArgs(mode, args, 3)
		child := exec.Command(os.Args[0], "-test.run=^TestRunnerFixture$", "--", fixtureMarker, "delayed-sentinel", args[0], args[1], args[2])
		if err := child.Start(); err != nil {
			fixtureFail("start descendant: %v", err)
		}
		waitForFixtureFile(args[0])
		for {
			time.Sleep(time.Hour)
		}
	case "spawn-and-exit-delayed-sentinel":
		requireFixtureArgs(mode, args, 3)
		child := exec.Command(os.Args[0], "-test.run=^TestRunnerFixture$", "--", fixtureMarker, "delayed-sentinel", args[0], args[1], args[2])
		if err := child.Start(); err != nil {
			fixtureFail("start descendant: %v", err)
		}
		waitForFixtureFile(args[0])
	case "delayed-sentinel":
		requireFixtureArgs(mode, args, 3)
		if err := os.WriteFile(args[0], []byte("ready"), 0o600); err != nil {
			fixtureFail("write readiness marker: %v", err)
		}
		delay, err := time.ParseDuration(args[2])
		if err != nil {
			fixtureFail("parse delay: %v", err)
		}
		time.Sleep(delay)
		if err := os.WriteFile(args[1], []byte("escaped"), 0o600); err != nil {
			fixtureFail("write sentinel: %v", err)
		}
	case "spawn-escaped-pipe-holder":
		requireFixtureArgs(mode, args, 3)
		child := exec.Command(os.Args[0], "-test.run=^TestRunnerFixture$", "--", fixtureMarker, "pipe-holder", args[0], args[1], args[2])
		child.Stdin = os.Stdin
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := child.Start(); err != nil {
			fixtureFail("start pipe holder: %v", err)
		}
		waitForFixtureFile(args[0])
	case "pipe-holder":
		requireFixtureArgs(mode, args, 3)
		if err := os.WriteFile(args[0], []byte("ready"), 0o600); err != nil {
			fixtureFail("write readiness marker: %v", err)
		}
		delay, err := time.ParseDuration(args[2])
		if err != nil {
			fixtureFail("parse delay: %v", err)
		}
		time.Sleep(delay)
		if err := os.WriteFile(args[1], []byte("done"), 0o600); err != nil {
			fixtureFail("write completion marker: %v", err)
		}
	default:
		fixtureFail("unknown fixture mode %q", mode)
	}
	os.Exit(0)
}

func requireFixtureArgs(mode string, args []string, count int) {
	if len(args) != count {
		fixtureFail("%s received %d args, want %d", mode, len(args), count)
	}
}

func waitForFixtureFile(path string) {
	if !pollUntil(2*time.Second, func() bool { _, err := os.Stat(path); return err == nil }) {
		fixtureFail("timed out waiting for %s", path)
	}
}

func fixtureFail(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(125)
}
