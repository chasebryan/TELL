//go:build linux

package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type stopCause uint8

const (
	stopNone stopCause = iota
	stopTimeout
	stopOutputLimit
	stopCanceled
	stopInfrastructure
)

type stopper struct {
	once sync.Once

	mu      sync.Mutex
	cause   stopCause
	killErr error
	pid     int
	process *os.Process
}

func newStopper(process *os.Process) *stopper {
	return &stopper{pid: process.Pid, process: process}
}

func (s *stopper) stop(cause stopCause) {
	s.once.Do(func() {
		s.mu.Lock()
		s.cause = cause
		s.killErr = killForStop(s.process, s.pid)
		s.mu.Unlock()
	})
}

func killForStop(process *os.Process, pid int) error {
	// The process-group signal is authoritative and always comes first. Killing
	// the still-unreaped direct child as well prevents a target that moved itself
	// to another process group from making Wait block forever; escaped
	// descendants remain outside TELL's process-group guarantee.
	groupErr := killProcessGroup(pid)
	directErr := process.Kill()
	if errors.Is(directErr, os.ErrProcessDone) || errors.Is(directErr, syscall.ESRCH) {
		directErr = nil
	}
	if groupErr != nil {
		return groupErr
	}
	if directErr != nil {
		return fmt.Errorf("runner: kill direct child %d: %w", pid, directErr)
	}
	return nil
}

func (s *stopper) state() (stopCause, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cause, s.killErr
}

func killProcessGroup(pid int) error {
	if pid <= 0 {
		return errors.New("runner: refusing to kill a nonpositive process group")
	}
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return fmt.Errorf("runner: kill process group %d: %w", pid, err)
}

type captureResult struct {
	stream Stream
	err    error
}

type writeResult struct {
	err error
}

// Run launches argv directly, sends input verbatim to stdin, and supervises
// the new Linux process group. Every successful Start is followed by exactly
// one Wait call before Run returns.
func Run(ctx context.Context, argv []string, input []byte, cfg Config) (Result, error) {
	cfg, err := validate(argv, cfg)
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, &CanceledError{Cause: err}
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("runner: create stdin pipe: %w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		return Result{}, fmt.Errorf("runner: create stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return Result{}, fmt.Errorf("runner: create stderr pipe: %w", err)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = cfg.CleanupGrace

	if err := cmd.Start(); err != nil {
		closePipeSet(stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW)
		return Result{}, fmt.Errorf("runner: start target: %w", err)
	}
	startedAt := time.Now()
	pid := cmd.Process.Pid
	st := newStopper(cmd.Process)

	// These descriptors belong only to the child after Start. Prompt closure is
	// important: otherwise EOF and EPIPE cannot propagate correctly.
	var infrastructureErr error
	if err := stdinR.Close(); err != nil {
		infrastructureErr = fmt.Errorf("runner: close child stdin read end: %w", err)
	}
	if err := stdoutW.Close(); err != nil && infrastructureErr == nil {
		infrastructureErr = fmt.Errorf("runner: close child stdout write end: %w", err)
	}
	if err := stderrW.Close(); err != nil && infrastructureErr == nil {
		infrastructureErr = fmt.Errorf("runner: close child stderr write end: %w", err)
	}
	if infrastructureErr != nil {
		st.stop(stopInfrastructure)
	}

	stdoutDone := make(chan captureResult, 1)
	stderrDone := make(chan captureResult, 1)
	stdinDone := make(chan writeResult, 1)
	var stdoutForced atomic.Bool
	var stderrForced atomic.Bool
	var stdinForced atomic.Bool
	go func() {
		stream, err := capture(stdoutR, cfg.MaxOutputBytes, func() { st.stop(stopOutputLimit) })
		if err != nil && !stdoutForced.Load() {
			st.stop(stopInfrastructure)
		}
		stdoutDone <- captureResult{stream: stream, err: err}
	}()
	go func() {
		stream, err := capture(stderrR, cfg.MaxOutputBytes, func() { st.stop(stopOutputLimit) })
		if err != nil && !stderrForced.Load() {
			st.stop(stopInfrastructure)
		}
		stderrDone <- captureResult{stream: stream, err: err}
	}()
	go func() {
		written := writeInput(stdinW, input)
		if written.err != nil && !errors.Is(written.err, syscall.EPIPE) && !stdinForced.Load() {
			st.stop(stopInfrastructure)
		}
		stdinDone <- written
	}()

	processDone := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		remaining := time.Until(startedAt.Add(cfg.Timeout))
		if remaining <= 0 {
			st.stop(stopTimeout)
			return
		}
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			select {
			case <-processDone:
				return
			default:
			}
			st.stop(stopCanceled)
		case <-timer.C:
			select {
			case <-processDone:
				return
			default:
			}
			st.stop(stopTimeout)
		case <-processDone:
		}
	}()

	// This is the sole Wait call after the successful Start.
	waitErr := cmd.Wait()
	close(processDone)
	<-watcherDone

	// A child may have forked after Start. Kill its original process group even
	// after the direct child is gone, then bound pipe cleanup in case a hostile
	// descendant escaped that group while retaining a descriptor.
	secondKillErr := killProcessGroup(pid)

	stdoutCapture, stderrCapture, inputWrite, forcedClose, pipeCleanupErr := joinIO(
		cfg.CleanupGrace,
		stdoutR,
		stderrR,
		stdinW,
		stdoutDone,
		stderrDone,
		stdinDone,
		&stdoutForced,
		&stderrForced,
		&stdinForced,
	)

	cause, firstKillErr := st.state()
	if infrastructureErr != nil {
		return Result{}, infrastructureErr
	}
	if firstKillErr != nil {
		return Result{}, firstKillErr
	}
	if secondKillErr != nil {
		return Result{}, secondKillErr
	}
	if pipeCleanupErr != nil {
		return Result{}, pipeCleanupErr
	}
	if stdoutCapture.err != nil && !forcedClose.stdout {
		return Result{}, fmt.Errorf("runner: capture stdout: %w", stdoutCapture.err)
	}
	if stderrCapture.err != nil && !forcedClose.stderr {
		return Result{}, fmt.Errorf("runner: capture stderr: %w", stderrCapture.err)
	}
	if inputWrite.err != nil && !errors.Is(inputWrite.err, syscall.EPIPE) && !forcedClose.stdin {
		return Result{}, fmt.Errorf("runner: deliver stdin: %w", inputWrite.err)
	}

	result := Result{Stdout: stdoutCapture.stream, Stderr: stderrCapture.stream}
	switch cause {
	case stopCanceled:
		return Result{}, &CanceledError{Cause: ctx.Err()}
	case stopTimeout:
		result.Outcome = OutcomeTimedOut
		return result, nil
	case stopOutputLimit:
		result.Outcome = OutcomeOutputLimited
		return result, nil
	case stopInfrastructure:
		return Result{}, errors.New("runner: process stopped after an infrastructure error")
	}

	if waitErr == nil {
		code := 0
		result.Outcome = OutcomeAccepted
		result.ExitCode = &code
		return result, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return Result{}, fmt.Errorf("runner: wait for target: %w", waitErr)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return Result{}, errors.New("runner: target returned an unknown Linux wait status")
	}
	if status.Exited() {
		code := status.ExitStatus()
		result.Outcome = OutcomeRejected
		result.ExitCode = &code
		return result, nil
	}
	if status.Signaled() {
		signal := int(status.Signal())
		result.Outcome = OutcomeSignaled
		result.SignalNumber = &signal
		return result, nil
	}
	return Result{}, fmt.Errorf("runner: target returned unclassified wait status %#x", uint32(status))
}

func capture(r *os.File, limit int, overflow func()) (Stream, error) {
	stream := Stream{Data: make([]byte, 0, min(limit, 32*1024))}
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			remaining := limit - len(stream.Data)
			if remaining > n {
				remaining = n
			}
			if remaining > 0 {
				stream.Data = append(stream.Data, buf[:remaining]...)
			}
			if n > remaining && !stream.Truncated {
				stream.Truncated = true
				overflow()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return stream, nil
			}
			return stream, err
		}
	}
}

func writeInput(w *os.File, input []byte) writeResult {
	var writeErr error
	for len(input) > 0 {
		n, err := w.Write(input)
		if n > 0 {
			input = input[n:]
		}
		if err != nil {
			writeErr = err
			break
		}
		if n == 0 {
			writeErr = io.ErrShortWrite
			break
		}
	}
	if err := w.Close(); writeErr == nil && err != nil {
		writeErr = err
	}
	return writeResult{err: writeErr}
}

type forcedIOClose struct {
	stdout bool
	stderr bool
	stdin  bool
}

func joinIO(
	grace time.Duration,
	stdoutR *os.File,
	stderrR *os.File,
	stdinW *os.File,
	stdoutDone <-chan captureResult,
	stderrDone <-chan captureResult,
	stdinDone <-chan writeResult,
	stdoutForced *atomic.Bool,
	stderrForced *atomic.Bool,
	stdinForced *atomic.Bool,
) (captureResult, captureResult, writeResult, forcedIOClose, error) {
	var stdout captureResult
	var stderr captureResult
	var stdin writeResult
	var forced forcedIOClose
	var cleanupErr error
	outCh := stdoutDone
	errCh := stderrDone
	inCh := stdinDone
	timer := time.NewTimer(grace)
	defer timer.Stop()

	for outCh != nil || errCh != nil || inCh != nil {
		select {
		case stdout = <-outCh:
			outCh = nil
		case stderr = <-errCh:
			errCh = nil
		case stdin = <-inCh:
			inCh = nil
		case <-timer.C:
			if outCh != nil {
				forced.stdout = true
				stdoutForced.Store(true)
				cleanupErr = joinCloseError(cleanupErr, "force-close stdout pipe", stdoutR.Close())
			}
			if errCh != nil {
				forced.stderr = true
				stderrForced.Store(true)
				cleanupErr = joinCloseError(cleanupErr, "force-close stderr pipe", stderrR.Close())
			}
			if inCh != nil {
				forced.stdin = true
				stdinForced.Store(true)
				cleanupErr = joinCloseError(cleanupErr, "force-close stdin pipe", stdinW.Close())
			}
			timer.Reset(grace)
		}
	}
	cleanupErr = joinCloseError(cleanupErr, "close stdout pipe", stdoutR.Close())
	cleanupErr = joinCloseError(cleanupErr, "close stderr pipe", stderrR.Close())
	cleanupErr = joinCloseError(cleanupErr, "close stdin pipe", stdinW.Close())
	return stdout, stderr, stdin, forced, cleanupErr
}

func joinCloseError(current error, operation string, closeErr error) error {
	if closeErr == nil || errors.Is(closeErr, os.ErrClosed) {
		return current
	}
	return errors.Join(current, fmt.Errorf("runner: %s: %w", operation, closeErr))
}

func closePipeSet(files ...*os.File) {
	for _, file := range files {
		if file != nil {
			_ = file.Close()
		}
	}
}
