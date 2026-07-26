// Package runner executes one target process with bounded, byte-exact I/O.
package runner

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

const (
	// DefaultCleanupGrace is the fixed v1 grace period used to drain and close
	// inherited process pipes after the direct child exits.
	DefaultCleanupGrace = 250 * time.Millisecond
	MinTimeout          = 10 * time.Millisecond
	MaxTimeout          = 60 * time.Second
	MaxOutputBytes      = 1_048_576
)

// Outcome is the externally meaningful result of a successfully launched
// target process.
type Outcome string

const (
	OutcomeAccepted      Outcome = "accepted"
	OutcomeRejected      Outcome = "rejected"
	OutcomeSignaled      Outcome = "signaled"
	OutcomeTimedOut      Outcome = "timed_out"
	OutcomeOutputLimited Outcome = "output_limited"
)

// Config controls one target execution.
type Config struct {
	Timeout        time.Duration
	MaxOutputBytes int
	CleanupGrace   time.Duration
}

// Stream is one separately captured output stream. Data never exceeds the
// configured cap. Truncated reports that byte cap+1 was observed.
type Stream struct {
	Data      []byte
	Truncated bool
}

// Result describes a target termination. Exactly one of ExitCode and
// SignalNumber is set for a natural termination; induced timeout and output
// limit terminations leave both nil.
type Result struct {
	Outcome      Outcome
	ExitCode     *int
	SignalNumber *int
	Stdout       Stream
	Stderr       Stream
}

var (
	// ErrCanceled identifies a caller cancellation or interruption. Run still
	// kills and reaps a successfully started process before returning it.
	ErrCanceled = errors.New("runner canceled")
	// ErrUnsupportedPlatform is returned by Run on non-Linux systems.
	ErrUnsupportedPlatform = errors.New("tell process supervision is supported only on Linux")
)

// CanceledError retains the context cancellation cause while also matching
// ErrCanceled through errors.Is.
type CanceledError struct {
	Cause error
}

func (e *CanceledError) Error() string {
	if e.Cause == nil {
		return ErrCanceled.Error()
	}
	return fmt.Sprintf("%s: %v", ErrCanceled, e.Cause)
}

func (e *CanceledError) Unwrap() error { return e.Cause }

func (e *CanceledError) Is(target error) bool {
	return target == ErrCanceled || errors.Is(e.Cause, target)
}

func validate(argv []string, cfg Config) (Config, error) {
	if len(argv) == 0 || argv[0] == "" {
		return Config{}, errors.New("runner: command argv is empty")
	}
	for i, arg := range argv {
		if !utf8.ValidString(arg) {
			return Config{}, fmt.Errorf("runner: argv[%d] is not valid UTF-8", i)
		}
	}
	if cfg.Timeout < MinTimeout || cfg.Timeout > MaxTimeout {
		return Config{}, fmt.Errorf("runner: timeout must be between %s and %s", MinTimeout, MaxTimeout)
	}
	if cfg.Timeout%time.Millisecond != 0 {
		return Config{}, errors.New("runner: timeout must be a whole number of milliseconds")
	}
	if cfg.MaxOutputBytes < 0 || cfg.MaxOutputBytes > MaxOutputBytes {
		return Config{}, fmt.Errorf("runner: maximum output bytes must be between 0 and %d", MaxOutputBytes)
	}
	if cfg.CleanupGrace == 0 {
		cfg.CleanupGrace = DefaultCleanupGrace
	}
	if cfg.CleanupGrace != DefaultCleanupGrace {
		return Config{}, fmt.Errorf("runner: cleanup grace must be %s", DefaultCleanupGrace)
	}
	return cfg, nil
}
