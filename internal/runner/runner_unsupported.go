//go:build !linux

package runner

import "context"

// Run reports the platform limitation while keeping the package buildable on
// non-Linux systems.
func Run(_ context.Context, argv []string, _ []byte, cfg Config) (Result, error) {
	if _, err := validate(argv, cfg); err != nil {
		return Result{}, err
	}
	return Result{}, ErrUnsupportedPlatform
}
