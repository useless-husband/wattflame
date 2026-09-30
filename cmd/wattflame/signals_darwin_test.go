//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/useless-husband/wattflame/internal/profile"
)

// A signal sent to wattflame alone (not typed at a terminal, which would
// reach the whole process group) has to be passed on to the program, and the
// recording must still be written. SIGQUIT is in the list because Go's default
// reaction to it is to exit on the spot, which would strand the program.
func TestSignalsArePassedOn(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("recording needs macOS on Apple Silicon")
	}
	clang, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("no clang to build the test program")
	}
	dir := t.TempDir()
	prog := filepath.Join(dir, "testprog")
	if out, err := exec.Command(clang, "-O1", "-o", prog, "../../internal/sampler/testdata/testprog.c").CombinedOutput(); err != nil {
		t.Fatalf("cannot build testprog: %v\n%s", err, out)
	}
	bin := filepath.Join(dir, "wattflame")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("cannot build wattflame: %v\n%s", err, out)
	}

	for _, tt := range []struct {
		sig  syscall.Signal
		code int
		exit string
	}{
		{syscall.SIGINT, 130, "killed by interrupt"},
		{syscall.SIGTERM, 143, "killed by terminated"},
		{syscall.SIGQUIT, 131, "killed by quit"},
	} {
		out := filepath.Join(dir, tt.sig.String()+".json")
		marker := filepath.Join(dir, tt.sig.String()+".started")
		cmd := exec.Command(bin, "record", "-q", "-no-html", "-o", out, "--", prog, "touch", marker, "30")
		// A session of its own: no controlling terminal, so nothing else
		// delivers the signal to the program.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// The program creates the marker as the first thing in main().
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			if time.Now().After(deadline) {
				cmd.Process.Kill()
				t.Fatalf("%v: the program never started", tt.sig)
			}
		}
		time.Sleep(100 * time.Millisecond)
		start := time.Now()
		if err := cmd.Process.Signal(tt.sig); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			code := -1
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err == nil {
				code = 0
			}
			if code != tt.code {
				t.Errorf("%v: wattflame exited with %d, want %d", tt.sig, code, tt.code)
			}
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			t.Fatalf("%v: wattflame still running after 10 s; the program never got the signal", tt.sig)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("%v: took %s to wind down", tt.sig, d)
		}
		p, err := profile.Load(out)
		if err != nil {
			t.Errorf("%v: no usable profile: %v", tt.sig, err)
			continue
		}
		if p.Meta.Exit != tt.exit {
			t.Errorf("%v: exit = %q, want %q", tt.sig, p.Meta.Exit, tt.exit)
		}
		if p.Meta.PID > 0 {
			if err := syscall.Kill(p.Meta.PID, 0); err != syscall.ESRCH {
				t.Errorf("%v: the program (pid %d) is still there", tt.sig, p.Meta.PID)
			}
		}
	}
}
