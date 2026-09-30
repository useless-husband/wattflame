//go:build !(darwin && arm64)

// Package preloadlib carries the small library that wattflame injects into
// the programs it launches. It only exists on macOS on Apple Silicon.
package preloadlib

import "errors"

// Path is unavailable on this platform.
func Path() (string, error) {
	return "", errors.New("recording is only supported on macOS on Apple Silicon")
}
