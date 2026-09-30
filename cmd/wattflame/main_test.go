package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/useless-husband/wattflame/internal/profile"
	"github.com/useless-husband/wattflame/internal/sampler"
)

func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = run(args, &out, &errb)
	return code, out.String(), errb.String()
}

type fakeSym struct{}

func (fakeSym) Symbolicate(target uint32, addr uint64) sampler.Symbol {
	names := map[uint64]string{0x1000: "main", 0x1100: "encode", 0x1200: "checksum"}
	start := addr &^ 0xff
	if n, ok := names[start]; ok {
		return sampler.Symbol{Found: true, Name: n, Module: "app", ModulePath: "/work/app", Start: start, Len: 0x100}
	}
	return sampler.Symbol{}
}
func (fakeSym) ThreadName(target uint32, tid uint64) string { return "" }

func writeProfile(t *testing.T, name string, encode, checksum uint64) string {
	t.Helper()
	b := profile.NewBuilder(fakeSym{})
	b.SetProcess(0, "app", "/work/app")
	add := func(energy uint64, frames ...uint64) {
		r := &sampler.Record{PID: 7, TID: 1, Frames: frames}
		r.W[sampler.WEnergyNJ][0] = energy
		r.W[sampler.WCPUNs][0] = energy / 4
		b.Add(r)
	}
	add(encode, 0x1108, 0x1010)
	add(checksum, 0x1208, 0x1010)
	p := b.Finish(profile.Meta{
		Tool: "wattflame test", Command: []string{"./app"}, PID: 7,
		Started: time.Unix(1790000000, 0).UTC(), DurationNs: 1e9, IntervalUs: 1000,
	}, []profile.Level{{Name: "P"}})
	path := filepath.Join(t.TempDir(), name)
	if err := p.Save(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUsageAndVersion(t *testing.T) {
	if code, _, stderr := runCLI(); code != 2 || !strings.Contains(stderr, "Usage:") {
		t.Errorf("no args: code %d, stderr %q", code, stderr)
	}
	if code, stdout, _ := runCLI("help"); code != 0 || !strings.Contains(stdout, "wattflame record") {
		t.Errorf("help: code %d, stdout %q", code, stdout)
	}
	if code, stdout, _ := runCLI("version"); code != 0 || !strings.HasPrefix(stdout, "wattflame ") {
		t.Errorf("version: code %d, stdout %q", code, stdout)
	}
	if code, _, stderr := runCLI("frobnicate"); code != 2 || !strings.Contains(stderr, `unknown command "frobnicate"`) {
		t.Errorf("unknown command: code %d, stderr %q", code, stderr)
	}
	for _, cmd := range []string{"record", "report", "diff"} {
		if code, _, stderr := runCLI(cmd, "-h"); code != 0 || !strings.Contains(stderr, "Flags:") {
			t.Errorf("%s -h: code %d, stderr %q", cmd, code, stderr)
		}
		if code, _, _ := runCLI(cmd, "-no-such-flag"); code != 2 {
			t.Errorf("%s with a bad flag: code %d", cmd, code)
		}
	}
}

func TestReport(t *testing.T) {
	path := writeProfile(t, "p.json", 3_000_000_000, 1_000_000_000)

	code, stdout, stderr := runCLI("report", path)
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"./app", "4.00 J", "encode", "75.0%", "checksum", "25.0%"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report lacks %q:\n%s", want, stdout)
		}
	}

	code, stdout, _ = runCLI("report", "-folded", "-", path)
	if code != 0 || stdout != "app [7];main;checksum 1000000\napp [7];main;encode 3000000\n" {
		t.Errorf("folded: code %d\n%s", code, stdout)
	}
	code, stdout, _ = runCLI("report", "-folded", "-", "-metric", "samples", "-threads", path)
	if code != 0 || !strings.Contains(stdout, "app [7];main thread;main;encode 1\n") {
		t.Errorf("folded samples: code %d\n%s", code, stdout)
	}
	if code, _, stderr := runCLI("report", "-folded", "-", "-metric", "bogus", path); code != 1 || !strings.Contains(stderr, "unknown metric") {
		t.Errorf("bad metric: code %d, stderr %q", code, stderr)
	}

	dir := t.TempDir()
	html, folded := filepath.Join(dir, "out.html"), filepath.Join(dir, "out.folded")
	code, stdout, stderr = runCLI("report", "-html", html, "-folded", folded, path)
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "flame graph  "+html) {
		t.Errorf("html path not reported:\n%s", stdout)
	}
	page, err := os.ReadFile(html)
	if err != nil || !bytes.Contains(page, []byte(`"format":"wattflame-profile"`)) {
		t.Errorf("html not written properly: %v", err)
	}
	if data, err := os.ReadFile(folded); err != nil || !bytes.Contains(data, []byte("main;encode 3000000")) {
		t.Errorf("folded file: %v %q", err, data)
	}
}

func TestReportErrors(t *testing.T) {
	if code, _, stderr := runCLI("report"); code != 2 || !strings.Contains(stderr, "Usage:") {
		t.Errorf("no file: code %d, stderr %q", code, stderr)
	}
	if code, _, stderr := runCLI("report", "/no/such/file.json"); code != 1 || !strings.Contains(stderr, "no such file") {
		t.Errorf("missing file: code %d, stderr %q", code, stderr)
	}
	junk := filepath.Join(t.TempDir(), "junk.json")
	os.WriteFile(junk, []byte(`{"format":"something else"}`), 0o644)
	if code, _, stderr := runCLI("report", junk); code != 1 || !strings.Contains(stderr, "not a wattflame profile") {
		t.Errorf("foreign file: code %d, stderr %q", code, stderr)
	}
}

func TestDiff(t *testing.T) {
	before := writeProfile(t, "before.json", 3_000_000_000, 1_000_000_000)
	after := writeProfile(t, "after.json", 1_000_000_000, 1_000_000_000)
	code, stdout, stderr := runCLI("diff", before, after)
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"4.00 J", "2.00 J", "−50.0%", "−2.00 J", "encode"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("diff lacks %q:\n%s", want, stdout)
		}
	}
	if code, _, _ := runCLI("diff", before); code != 2 {
		t.Errorf("one file: code %d", code)
	}
	if code, _, stderr := runCLI("diff", before, "/no/such.json"); code != 1 || stderr == "" {
		t.Errorf("missing file: code %d", code)
	}
}

func TestRecordArgumentErrors(t *testing.T) {
	if sampler.Supported() != nil {
		code, _, stderr := runCLI("record", "--", "true")
		if code != 1 || !strings.Contains(stderr, "Apple Silicon") {
			t.Errorf("unsupported platform: code %d, stderr %q", code, stderr)
		}
		return
	}
	if code, _, stderr := runCLI("record"); code != 2 || !strings.Contains(stderr, "either a command to run or -p") {
		t.Errorf("nothing to record: code %d, stderr %q", code, stderr)
	}
	if code, _, _ := runCLI("record", "-p", "1", "--", "true"); code != 2 {
		t.Errorf("both pid and command: code %d", code)
	}
	if code, _, stderr := runCLI("record", "-hz", "0", "--", "true"); code != 1 || !strings.Contains(stderr, "-hz") {
		t.Errorf("bad rate: code %d, stderr %q", code, stderr)
	}
	if code, _, stderr := runCLI("record", "--", "/no/such/program"); code != 1 || !strings.Contains(stderr, "cannot run") {
		t.Errorf("missing program: code %d, stderr %q", code, stderr)
	}
}

func TestWhyNoStacks(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tests := []struct {
		name, path, want string
	}{
		{"system binary", "/bin/ls", "macOS system binary"},
		{"system binary in /usr/bin", "/usr/bin/true", "macOS system binary"},
		{"shell script", write("run.sh", "#!/bin/sh\necho hi\n"), "script run by /bin/sh"},
		{"script with arguments", write("run2.sh", "#! /bin/bash -eu\n"), "script run by /bin/bash"},
		{"own binary", write("prog", "\xcf\xfa\xed\xfe not really"), "hardened runtime"},
		{"script with a private interpreter", write("run.py", "#!/opt/homebrew/bin/python3\n"), "hardened runtime"},
		{"env with an unknown program", write("run.x", "#!/usr/bin/env -S no-such-interpreter-xyz\n"), "hardened runtime"},
		{"empty shebang", write("odd", "#!\n"), "hardened runtime"},
		{"/usr/local is not protected", "/usr/local/bin/whatever", "hardened runtime"},
	}
	for _, tt := range tests {
		if got := whyNoStacks(tt.path); !strings.Contains(got, tt.want) {
			t.Errorf("%s: %q does not mention %q", tt.name, got, tt.want)
		}
	}
	// A symlink into a protected directory is still protected.
	link := filepath.Join(dir, "ls")
	if err := os.Symlink("/bin/ls", link); err == nil && runtime.GOOS == "darwin" {
		if !isProtected(link) {
			t.Error("symlink to /bin/ls is not recognised as protected")
		}
	}
}

func TestRecordEndToEnd(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("recording needs macOS on Apple Silicon")
	}
	clang, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("no clang to build the test program")
	}
	dir := t.TempDir()
	prog := filepath.Join(dir, "testprog")
	if out, err := exec.Command(clang, "-O1", "-g", "-o", prog, "../../internal/sampler/testdata/testprog.c").CombinedOutput(); err != nil {
		t.Fatalf("cannot build testprog: %v\n%s", err, out)
	}
	out := filepath.Join(dir, "run.json")

	code, stdout, stderr := runCLI("record", "-o", out, "--", prog, "spin", "0.3", "5")
	if code != 5 {
		t.Errorf("exit code %d, want the program's own (5); stderr %q", code, stderr)
	}
	for _, want := range []string{"exit 5", "hot_a", "hot_b", "profile      " + out, "flame graph  " + filepath.Join(dir, "run.html")} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
	p, err := profile.Load(out)
	if err != nil {
		t.Fatal(err)
	}
	if p.Meta.Exit != "exit 5" || p.Meta.PID == 0 || p.Meta.IntervalUs != 1000 || len(p.Meta.Processes) != 1 {
		t.Errorf("meta = %+v", p.Meta)
	}
	if p.Meta.DurationNs < 250e6 || p.Meta.DurationNs > 3e9 {
		t.Errorf("duration = %d ns", p.Meta.DurationNs)
	}
	if len(p.Levels) == 0 || p.Meta.Machine.Chip == "" {
		t.Errorf("levels %v, machine %+v", p.Levels, p.Meta.Machine)
	}
	if _, err := os.Stat(filepath.Join(dir, "run.html")); err != nil {
		t.Error(err)
	}

	// -d stops a program that would otherwise run for a long time, and
	// -no-html/-q keep quiet.
	out2 := filepath.Join(dir, "short.json")
	start := time.Now()
	code, stdout, stderr = runCLI("record", "-q", "-no-html", "-d", "300ms", "-o", out2, "--", prog, "spin", "30")
	if code != 0 || stdout != "" {
		t.Errorf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("-d 300ms took %s", d)
	}
	p2, err := profile.Load(out2)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Meta.Exit != "stopped after 300ms" {
		t.Errorf("exit = %q", p2.Meta.Exit)
	}
	if _, err := os.Stat(filepath.Join(dir, "short.html")); err == nil {
		t.Error("-no-html still wrote a page")
	}
}
