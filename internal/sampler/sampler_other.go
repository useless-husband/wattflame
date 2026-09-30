//go:build !(darwin && arm64)

package sampler

import (
	"errors"
	"os"
	"syscall"
	"time"
)

var errUnsupported = errors.New("wattflame records on macOS 13 or later on Apple Silicon only; " +
	"`wattflame report` and `wattflame diff` work everywhere")

// Session is a placeholder on platforms without a sampler.
type Session struct{}

// Supported reports whether this machine can be profiled.
func Supported() error { return errUnsupported }

// PerfLevels returns the names of the CPU performance levels.
func PerfLevels() []string { return nil }

// Launch is unavailable on this platform.
func Launch(argv, env []string, preloadPath string) (*Session, error) {
	return nil, errUnsupported
}

// Attach is unavailable on this platform.
func Attach(pid int) (*Session, error) { return nil, errUnsupported }

func (s *Session) Start(interval time.Duration, maxDepth int) error { return errUnsupported }
func (s *Session) Stop()                                            {}
func (s *Session) Drain(fn func(*Record)) int                       { return 0 }
func (s *Session) RootExited() (bool, syscall.WaitStatus)           { return true, 0 }
func (s *Session) RootPID() int                                     { return 0 }
func (s *Session) Signal(sig os.Signal)                             {}
func (s *Session) Stats() Stats                                     { return Stats{} }
func (s *Session) Targets() []TargetInfo                            { return nil }
func (s *Session) Symbolicate(target uint32, addr uint64) Symbol    { return Symbol{} }
func (s *Session) ThreadName(pid int, tid uint64) string            { return "" }
func (s *Session) Close()                                           {}
