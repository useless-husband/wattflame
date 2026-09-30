//go:build darwin && arm64

package sampler_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/useless-husband/wattflame/internal/preloadlib"
	"github.com/useless-husband/wattflame/internal/sampler"
)

var (
	testprog string
	preload  string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wattflame-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := func() int {
		defer os.RemoveAll(dir)
		clang, err := exec.LookPath("clang")
		if err != nil {
			fmt.Fprintln(os.Stderr, "skipping sampler tests: no clang to build the test program")
			return 0
		}
		testprog = filepath.Join(dir, "testprog")
		out, err := exec.Command(clang, "-O1", "-g", "-o", testprog, "testdata/testprog.c").CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot build testprog: %v\n%s", err, out)
			return 1
		}
		preload, err = preloadlib.Path()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}

type result struct {
	energy, cpu   uint64
	records       int
	byLeaf        map[string]uint64 // leaf function -> energy
	stacks        map[string]int    // "leaf<caller<..." -> count
	threadNames   map[string]bool
	status        sampler.ExitStatus
	exited        bool
	targets       []sampler.TargetInfo
	stats         sampler.Stats
	lrCallers     map[string]int // name of the function lr points into, for frameless-leaf checks
	leafWithStack int
}

// record launches testprog with args and collects everything until it exits
// or the timeout passes.
func record(t *testing.T, timeout time.Duration, args ...string) result {
	t.Helper()
	sess, err := sampler.Launch(append([]string{testprog}, args...), os.Environ(), preload)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer sess.Close()
	res := result{byLeaf: map[string]uint64{}, stacks: map[string]int{}, threadNames: map[string]bool{}, lrCallers: map[string]int{}}
	collect := func(r *sampler.Record) {
		res.records++
		var e uint64
		for l := 0; l < sampler.MaxLevels; l++ {
			e += r.W[sampler.WEnergyNJ][l]
			res.cpu += r.W[sampler.WCPUNs][l]
		}
		res.energy += e
		if name := sess.ThreadName(r.PID, r.TID); name != "" {
			res.threadNames[name] = true
		}
		if len(r.Frames) == 0 {
			return
		}
		res.leafWithStack++
		names := make([]string, 0, len(r.Frames))
		for i, addr := range r.Frames {
			if i > 0 {
				addr--
			}
			names = append(names, sess.Symbolicate(r.Target, addr).Name)
		}
		res.byLeaf[names[0]] += e
		res.stacks[strings.Join(names, "<")]++
		if r.LR != 0 {
			res.lrCallers[names[0]+"<-"+sess.Symbolicate(r.Target, r.LR-1).Name]++
		}
	}
	if err := sess.Start(time.Millisecond, 128); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sess.Drain(collect)
		if exited, _ := sess.RootExited(); exited {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sess.Stop()
	sess.Drain(collect)
	res.exited, res.status = sess.RootExited()
	res.targets = sess.Targets()
	res.stats = sess.Stats()
	return res
}

func hasStack(res result, leaf, ancestor string) bool {
	for s := range res.stacks {
		parts := strings.Split(s, "<")
		if parts[0] != leaf {
			continue
		}
		for _, p := range parts[1:] {
			if p == ancestor {
				return true
			}
		}
	}
	return false
}

func TestLaunchRecordsStacksAndCounters(t *testing.T) {
	res := record(t, 10*time.Second, "spin", "0.5", "3")
	if !res.exited {
		t.Fatal("program did not exit")
	}
	if res.status.Signal != "" || res.status.Code != 3 {
		t.Errorf("exit status = %v, want 3", res.status)
	}
	if res.records < 200 {
		t.Errorf("only %d records for a second of CPU time at 1 kHz", res.records)
	}
	for _, fn := range []string{"hot_a", "hot_b"} {
		if _, ok := res.byLeaf[fn]; !ok {
			t.Errorf("no samples with %s as the leaf; leaves: %v", fn, keys(res.byLeaf))
		}
	}
	if !hasStack(res, "hot_a", "main") {
		t.Errorf("hot_a never seen under main: %v", keys2(res.stacks))
	}
	if !hasStack(res, "hot_b", "worker") {
		t.Errorf("hot_b never seen under worker: %v", keys2(res.stacks))
	}
	if !res.threadNames["test-worker"] {
		t.Errorf("thread name not seen: %v", res.threadNames)
	}
	// Two threads spin for 0.5 s each.
	if res.cpu < 600e6 || res.cpu > 1400e6 {
		t.Errorf("attributed CPU time = %.0f ms, want about 1000", float64(res.cpu)/1e6)
	}
	if res.stats.Samples == 0 || res.stats.Ticks < 300 {
		t.Errorf("stats = %+v", res.stats)
	}
	if len(res.targets) != 1 || res.targets[0].Name != "testprog" || !strings.HasSuffix(res.targets[0].Path, "/testprog") {
		t.Errorf("targets = %+v", res.targets)
	}

	if res.energy == 0 {
		// Virtual machines (CI runners) do not expose the energy counters.
		t.Skip("this machine reports no per-thread energy; skipping the energy checks")
	}
	// Both loops run flat out on a performance core, so each should get a
	// substantial share.
	for _, fn := range []string{"hot_a", "hot_b"} {
		share := float64(res.byLeaf[fn]) / float64(res.energy)
		if share < 0.2 || share > 0.8 {
			t.Errorf("%s has %.0f%% of the energy, want roughly half", fn, 100*share)
		}
	}
	k, th := res.targets[0].EnergyNJ, res.targets[0].ThreadsEnergyNJ
	if k == 0 {
		t.Fatal("no kernel process energy")
	}
	if ratio := float64(th) / float64(k); ratio < 0.95 || ratio > 1.02 {
		t.Errorf("thread counters add up to %.1f%% of the kernel's process total", 100*ratio)
	}
	// What was emitted must match what was read from the counters.
	if diff := float64(res.energy)/float64(k) - 1; diff < -0.10 || diff > 0.10 {
		t.Errorf("emitted %d nJ, kernel billed %d nJ", res.energy, k)
	}
}

func TestFramelessLeafKeepsItsCaller(t *testing.T) {
	res := record(t, 10*time.Second, "nested", "0.4")
	if res.lrCallers["leaf_inner<-hot_outer"] == 0 {
		t.Fatalf("never caught leaf_inner with lr pointing into hot_outer: %v", res.lrCallers)
	}
	// The frame-pointer walk alone skips hot_outer for a frameless leaf.
	skipped := 0
	for s, n := range res.stacks {
		if strings.HasPrefix(s, "leaf_inner<main") {
			skipped += n
		}
	}
	if skipped == 0 {
		t.Skip("leaf_inner got a frame on this toolchain; nothing to recover")
	}
}

func TestProgramWaitsForStart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	sess, err := sampler.Launch([]string{testprog, "touch", marker, "0.1"}, os.Environ(), preload)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer sess.Close()
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("main() ran before Start: energy used until then would be missed")
	}
	if err := sess.Start(time.Millisecond, 64); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if exited, _ := sess.RootExited(); exited {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sess.Stop()
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("program never ran: %v", err)
	}
}

func TestForkedChildIsProfiled(t *testing.T) {
	res := record(t, 10*time.Second, "fork", "0.4")
	if len(res.targets) != 2 {
		t.Fatalf("targets = %+v, want parent and forked child", res.targets)
	}
	if res.targets[0].PID == res.targets[1].PID {
		t.Errorf("both targets have pid %d", res.targets[0].PID)
	}
	if _, ok := res.byLeaf["hot_child"]; !ok {
		t.Errorf("no samples from the child: %v", keys(res.byLeaf))
	}
	if _, ok := res.byLeaf["hot_a"]; !ok {
		t.Errorf("no samples from the parent: %v", keys(res.byLeaf))
	}
}

func TestCloseKillsARunningProgram(t *testing.T) {
	sess, err := sampler.Launch([]string{testprog, "spin", "30"}, os.Environ(), preload)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	pid := sess.RootPID()
	if err := sess.Start(time.Millisecond, 64); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	n := 0
	sess.Drain(func(*sampler.Record) { n++ })
	if n == 0 {
		t.Error("no records after 150 ms")
	}
	sess.Stop()
	sess.Close()
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Errorf("pid %d still exists after Close (kill(0) = %v)", pid, err)
	}
}

func TestCloseWithoutStartReleasesTheProgram(t *testing.T) {
	sess, err := sampler.Launch([]string{testprog, "spin", "30"}, os.Environ(), preload)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	pid := sess.RootPID()
	sess.Close()
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Errorf("pid %d left behind (kill(0) = %v)", pid, err)
	}
}

func TestLaunchErrors(t *testing.T) {
	if _, err := sampler.Launch([]string{"/no/such/program"}, os.Environ(), preload); err == nil || !strings.Contains(err.Error(), "cannot run") {
		t.Errorf("missing program: err = %v", err)
	}
	if _, err := sampler.Launch(nil, os.Environ(), preload); err == nil {
		t.Error("empty argv accepted")
	}
}

// A macOS system binary cannot be sampled, but it and the processes it starts
// are still counted.
func TestSystemBinaryIsCountedWithoutStacks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	// /usr/bin/env is protected; it execs testprog, which has lost the
	// injected library along the way and so cannot announce itself either.
	sess, err := sampler.Launch([]string{"/usr/bin/env", testprog, "spin", "0.4"}, os.Environ(), preload)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer sess.Close()
	var energy, cpu uint64
	records, withStack := 0, 0
	collect := func(r *sampler.Record) {
		records++
		if len(r.Frames) > 0 {
			withStack++
		}
		if r.Flags&sampler.FlagOpaque == 0 {
			t.Errorf("record without the opaque flag: %+v", r)
		}
		for l := 0; l < sampler.MaxLevels; l++ {
			energy += r.W[sampler.WEnergyNJ][l]
			cpu += r.W[sampler.WCPUNs][l]
		}
	}
	if err := sess.Start(time.Millisecond, 64); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		sess.Drain(collect)
		if exited, _ := sess.RootExited(); exited {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sess.Stop()
	sess.Drain(collect)

	exited, status := sess.RootExited()
	if !exited || status.Code != 0 || status.Signal != "" {
		t.Errorf("exited %v, status %v", exited, status)
	}
	if withStack != 0 {
		t.Errorf("%d records carry a stack", withStack)
	}
	// Two threads spinning for 0.4 s.
	if cpu < 500e6 || cpu > 1200e6 {
		t.Errorf("counted %.0f ms of CPU time, want about 800", float64(cpu)/1e6)
	}
	names := map[string]bool{}
	for _, tg := range sess.Targets() {
		names[tg.Name] = true
		if !tg.Opaque {
			t.Errorf("target %+v is not opaque", tg)
		}
	}
	// The pid is the same before and after exec, the name is not.
	if !names["env"] || !names["testprog"] {
		t.Errorf("targets = %+v, want both env and testprog", sess.Targets())
	}
	if records == 0 {
		t.Error("no records")
	}
}

// A program that starts helpers which cannot be sampled (here a system shell
// running a command) still has their energy in the recording.
func TestUnsampledChildIsCounted(t *testing.T) {
	res := record(t, 10*time.Second, "system", "0.3")
	if len(res.targets) == 0 || res.targets[0].Opaque || res.targets[0].Name != "testprog" {
		t.Fatalf("root target = %+v, want testprog, sampled", res.targets)
	}
	// The forked child announces itself, then becomes /bin/sh, which cannot.
	counted := map[string]bool{}
	for _, tg := range res.targets[1:] {
		if tg.Opaque {
			counted[tg.Name] = true
		}
	}
	if !counted["sleep"] {
		t.Errorf("no energy-only entry for the sleep the shell ran: %+v", res.targets)
	}
	if _, ok := res.byLeaf["hot_a"]; !ok {
		t.Errorf("parent not sampled: %v", keys(res.byLeaf))
	}
}

func TestAttachNeedsRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	cmd := exec.Command(testprog, "spin", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	_, err := sampler.Attach(cmd.Process.Pid)
	if err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Errorf("err = %v, want a hint about sudo", err)
	}
	if _, err := sampler.Attach(os.Getpid()); err == nil {
		t.Error("attached to itself")
	}
	if _, err := sampler.Attach(999999); err == nil || !strings.Contains(err.Error(), "no process") {
		t.Errorf("missing pid: err = %v", err)
	}
}

func TestPerfLevels(t *testing.T) {
	levels := sampler.PerfLevels()
	if len(levels) == 0 || len(levels) > sampler.MaxLevels {
		t.Fatalf("levels = %v", levels)
	}
	for _, l := range levels {
		if l == "" {
			t.Errorf("empty level name in %v", levels)
		}
	}
}

func keys(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keys2(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
