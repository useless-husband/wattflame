package report

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/useless-husband/wattflame/internal/profile"
	"github.com/useless-husband/wattflame/internal/sampler"
)

func TestEnergy(t *testing.T) {
	tests := map[uint64]string{
		0:                 "0 J",
		999:               "999 nJ",
		1000:              "1.00 µJ",
		12_340:            "12.3 µJ",
		999_400:           "999 µJ",
		1_000_000:         "1.00 mJ",
		29_257_700_000:    "29.3 J",
		123_456_000_000:   "123 J",
		9_996_000_000:     "10.0 J",
		1_500_000_000_000: "1.50 kJ",
	}
	for in, want := range tests {
		if got := Energy(in); got != want {
			t.Errorf("Energy(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestDuration(t *testing.T) {
	tests := map[uint64]string{
		0:               "0 s",
		500:             "500 ns",
		1500:            "1.50 µs",
		3_200_000_000:   "3.20 s",
		206_000_000:     "206 ms",
		59_000_000_000:  "59.0 s",
		725_000_000_000: "12m5s",
	}
	for in, want := range tests {
		if got := Duration(in); got != want {
			t.Errorf("Duration(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestWattsAndPercent(t *testing.T) {
	watts := map[float64]string{0: "0 W", 0.0000005: "0.50 µW", 0.147: "147 mW", 2.62: "2.62 W", 13.04: "13.0 W", 250: "250 W"}
	for in, want := range watts {
		if got := Watts(in); got != want {
			t.Errorf("Watts(%v) = %q, want %q", in, got, want)
		}
	}
	type pc struct{ part, whole uint64 }
	percents := map[pc]string{
		{0, 100}:          "0%",
		{1, 0}:            "–",
		{1, 10000}:        "<0.1%",
		{371, 1000}:       "37.1%",
		{100, 100}:        "100.0%",
		{9997, 10000}:     "99.97%", // close to all of it: show that it is not quite
		{10074, 10000}:    "100.7%",
		{10001, 10000}:    "100.01%",
		{99900, 100000}:   "99.9%",
		{999999, 1000000}: ">99.99%",
	}
	for in, want := range percents {
		if got := Percent(in.part, in.whole); got != want {
			t.Errorf("Percent(%d, %d) = %q, want %q", in.part, in.whole, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("got %q", got)
	}
	long := "std::__1::vector<std::__1::basic_string<char>>::push_back(std::__1::basic_string<char> const&)"
	got := truncate(long, 40)
	if n := len([]rune(got)); n != 40 {
		t.Errorf("truncated to %d runes: %q", n, got)
	}
	if !strings.HasPrefix(got, "std::__1::vector") || !strings.HasSuffix(got, "const&)") || !strings.Contains(got, "…") {
		t.Errorf("should keep both ends: %q", got)
	}
	if got := truncate("功德無量功德無量功德無量", 5); len([]rune(got)) != 5 {
		t.Errorf("multi-byte: %q", got)
	}
}

// --- a small synthetic profile ------------------------------------------------

type sym map[uint64]string

func (s sym) Symbolicate(target uint32, addr uint64) sampler.Symbol {
	start := addr &^ 0xff
	name, ok := s[start]
	if !ok {
		return sampler.Symbol{}
	}
	mod, path := "app", "/work/app"
	if strings.HasPrefix(name, "_platform") {
		mod, path = "libsystem_platform.dylib", "/usr/lib/system/libsystem_platform.dylib"
	}
	return sampler.Symbol{Found: true, Name: name, Module: mod, ModulePath: path, File: "/src/" + name + ".c", Line: 7, Start: start, Len: 0x100}
}

func (sym) ThreadName(target uint32, tid uint64) string { return "" }

const (
	fMain   = 0x1000
	fParse  = 0x1100
	fHash   = 0x1200
	fMemcpy = 0x1300
)

var table = sym{fMain: "main", fParse: "parse", fHash: "hash<a&b>", fMemcpy: "_platform_memmove"}

type spec struct {
	energy, cpu uint64
	level       int
	frames      []uint64
}

func makeProfile(command string, specs []spec) *profile.Profile {
	b := profile.NewBuilder(table)
	b.SetProcess(0, "app", "/work/app")
	var energy uint64
	for i, s := range specs {
		r := &sampler.Record{PID: 42, TID: 1, Frames: s.frames, TimeNs: uint64(i) * 20_000_000}
		r.W[sampler.WEnergyNJ][s.level] = s.energy
		r.W[sampler.WCPUNs][s.level] = s.cpu
		b.Add(r)
		energy += s.energy
	}
	return b.Finish(profile.Meta{
		Tool:       "wattflame test",
		Command:    strings.Fields(command),
		PID:        42,
		Started:    time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
		DurationNs: 2_000_000_000,
		TreeCPUNs:  4_000_000_000,
		IntervalUs: 1000,
		Exit:       "exit 0",
		Machine:    profile.Machine{Chip: "Apple M5", OS: "macOS 27.0"},
		Processes:  []profile.Process{{PID: 42, Name: "app", KernelEnergyNJ: energy + energy/100, ThreadsEnergyNJ: energy}},
		Sampler: profile.SamplerStats{Ticks: 2000, Overruns: 3, Samples: uint64(len(specs)),
			SuspendNs: uint64(len(specs)) * 9000, SelfEnergyNJ: energy / 50},
	}, []profile.Level{{Name: "Super", Cores: 4}, {Name: "Efficiency", Cores: 6}})
}

func before() *profile.Profile {
	return makeProfile("./app --input big.csv", []spec{
		{6_000_000_000, 1_000_000_000, 0, []uint64{fParse + 8, fMain + 16}},
		{3_000_000_000, 500_000_000, 0, []uint64{fHash + 8, fMain + 16}},
		{900_000_000, 400_000_000, 1, []uint64{fMemcpy + 8, fParse + 16, fMain + 16}},
		{100_000_000, 100_000_000, 1, []uint64{fMain + 8}},
	})
}

func after() *profile.Profile {
	return makeProfile("./app --input big.csv", []spec{
		{1_500_000_000, 300_000_000, 0, []uint64{fParse + 8, fMain + 16}},
		{3_000_000_000, 500_000_000, 0, []uint64{fHash + 8, fMain + 16}},
		{100_000_000, 100_000_000, 1, []uint64{fMain + 8}},
	})
}

func TestSummary(t *testing.T) {
	var buf bytes.Buffer
	if err := Summary(&buf, before(), 3); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"./app --input big.csv",
		"pid 42 · exit 0 · Apple M5",
		"10.0 J",
		"5.00 W average over 2.00 s",
		"Super 9.00 J (90.0%) · Efficiency 1.00 J (10.0%)",
		"Accounted  99.0% of what the kernel billed to the process\n",
		"Seen       50.0% of the 4.00 s of CPU time the command and its children used",
		"200 mJ of its own (2.0% on top), 1000 Hz, 9.00 µs pause per stack, 3 of 2003 ticks late",
		"… and 1 more function\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	// Rows are ordered by self energy and carry self %, total % and power.
	rows := regexp.MustCompile(`(?m)^\s+(\S+ \S+)\s+(\S+)\s+(\S+)\s+(\S+ ?\S*)\s{2,}(\S+)  (\S+)$`).FindAllStringSubmatch(out, -1)
	if len(rows) != 3 {
		t.Fatalf("want 3 function rows, got %d:\n%s", len(rows), out)
	}
	if rows[0][5] != "parse" || rows[0][1] != "6.00 J" || rows[0][2] != "60.0%" || rows[0][3] != "69.0%" || rows[0][4] != "6.00 W" {
		t.Errorf("first row = %q", rows[0][1:])
	}
	if rows[1][5] != "hash<a&b>" || rows[2][5] != "_platform_memmove" || rows[2][6] != "libsystem_platform.dylib" {
		t.Errorf("rows = %q / %q", rows[1][1:], rows[2][1:])
	}
}

// When threads end between readings, part of the energy is known only from
// the process total. The summary says how much was read directly.
func TestSummarySaysHowMuchWasReadPerThread(t *testing.T) {
	p := before()
	p.Meta.Processes[0].KernelEnergyNJ = 10_000_000_000
	p.Meta.Processes[0].ThreadsEnergyNJ = 6_200_000_000
	p.Meta.TreeCPUNs = 0
	var buf bytes.Buffer
	if err := Summary(&buf, p, 3); err != nil {
		t.Fatal(err)
	}
	if want := "Accounted  100.0% of what the kernel billed to the process (62.0% read thread by thread)\n"; !strings.Contains(buf.String(), want) {
		t.Errorf("summary lacks %q:\n%s", want, buf.String())
	}
	if strings.Contains(buf.String(), "Seen ") {
		t.Errorf("a coverage line without anything to compare with:\n%s", buf.String())
	}
}

// A command line may contain anything, including the template's own markers.
func TestHTMLSurvivesMarkersInTheData(t *testing.T) {
	p := makeProfile("./app __WATTFLAME_PROFILE__ __WATTFLAME_TITLE__", []spec{
		{6_000_000_000, 1_000_000_000, 0, []uint64{fHash + 8, fMain + 16}},
	})
	var buf bytes.Buffer
	if err := HTML(&buf, p); err != nil {
		t.Fatal(err)
	}
	page := buf.String()
	m := regexp.MustCompile(`(?s)<script id="profile" type="application/json">(.*?)</script>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no embedded profile")
	}
	back, err := profile.Read(strings.NewReader(m[1]))
	if err != nil {
		t.Fatalf("embedded profile does not parse: %v", err)
	}
	if got := strings.Join(back.Meta.Command, " "); got != "./app __WATTFLAME_PROFILE__ __WATTFLAME_TITLE__" {
		t.Errorf("command = %q", got)
	}
	if !strings.Contains(page, "<title>./app __WATTFLAME_PROFILE__ __WATTFLAME_TITLE__ · wattflame</title>") {
		t.Error("title was not written as plain text")
	}
	if !strings.HasSuffix(strings.TrimSpace(page), "</html>") {
		t.Error("page is truncated")
	}
}

func TestSummaryOfEmptyProfile(t *testing.T) {
	var buf bytes.Buffer
	p := makeProfile("./idle", nil)
	if err := Summary(&buf, p, 10); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Nothing was recorded") {
		t.Errorf("got:\n%s", buf.String())
	}
}

// A recording from a machine without energy counters has stacks and CPU time.
// The summary must still be a usable profile.
func TestSummaryWithoutEnergyFallsBackToCPUTime(t *testing.T) {
	p := makeProfile("./app", []spec{
		{0, 300_000_000, 0, []uint64{fParse + 8, fMain + 16}},
		{0, 100_000_000, 0, []uint64{fHash + 8, fMain + 16}},
	})
	var buf bytes.Buffer
	if err := Summary(&buf, p, 5); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"reports no per-thread energy", "CPU time profile", "300 ms", "75.0%", "parse", "hash<a&b>"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "parse") > strings.Index(out, "hash<a&b>") {
		t.Errorf("functions are not ordered by CPU time:\n%s", out)
	}
}

func TestHTMLEmbedsProfileSafely(t *testing.T) {
	p := makeProfile("./app </script><script>alert(1)</script> &amp;", []spec{
		{6_000_000_000, 1_000_000_000, 0, []uint64{fHash + 8, fMain + 16}},
	})
	var buf bytes.Buffer
	if err := HTML(&buf, p); err != nil {
		t.Fatal(err)
	}
	page := buf.String()
	if strings.Contains(page, "__WATTFLAME_") {
		t.Error("placeholder left in the page")
	}
	if strings.Contains(page, "<script>alert(1)") {
		t.Error("command line was written into the page unescaped")
	}

	// The data block must parse back to the same profile.
	m := regexp.MustCompile(`(?s)<script id="profile" type="application/json">(.*?)</script>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no embedded profile")
	}
	back, err := profile.Read(strings.NewReader(m[1]))
	if err != nil {
		t.Fatalf("embedded profile does not parse: %v", err)
	}
	a, _ := json.Marshal(p)
	b, _ := json.Marshal(back)
	if !bytes.Equal(a, b) {
		t.Error("embedded profile differs from the original")
	}
	if got := strings.Join(back.Meta.Command, " "); got != "./app </script><script>alert(1)</script> &amp;" {
		t.Errorf("command = %q", got)
	}
	// Exactly the viewer's own script blocks: data + code.
	if n := strings.Count(page, "<script"); n != 2 {
		t.Errorf("%d <script> tags in the page, want 2", n)
	}
	if !strings.Contains(page, "<title>./app &lt;/script&gt;") {
		t.Error("title is not HTML-escaped")
	}
}

func TestCompare(t *testing.T) {
	deltas := Compare(before(), after())
	got := map[string]FuncDelta{}
	for _, d := range deltas {
		got[d.Name] = d
	}
	if len(deltas) != 4 {
		t.Fatalf("got %d deltas: %+v", len(deltas), deltas)
	}
	if deltas[0].Name != "parse" || deltas[0].Delta() != -4_500_000_000 {
		t.Errorf("largest change = %+v", deltas[0])
	}
	if d := got["_platform_memmove"]; d.Before != 900_000_000 || d.After != 0 || d.Module != "libsystem_platform.dylib" {
		t.Errorf("removed function = %+v", d)
	}
	if d := got["hash<a&b>"]; d.Delta() != 0 {
		t.Errorf("unchanged function = %+v", d)
	}
	// Reversed, a function that only exists afterwards shows up as new.
	rev := Compare(after(), before())
	for _, d := range rev {
		if d.Name == "_platform_memmove" && (d.Before != 0 || d.After != 900_000_000) {
			t.Errorf("new function = %+v", d)
		}
	}
}

func TestDiff(t *testing.T) {
	var buf bytes.Buffer
	if err := Diff(&buf, before(), after(), 2); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"Energy", "10.0 J", "4.60 J", "−54.0%",
		"−4.50 J", "parse",
		"−900 mJ", "_platform_memmove",
		"… and 2 more functions",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hash<a&b>") {
		t.Errorf("unchanged function listed above changed ones:\n%s", out)
	}

	buf.Reset()
	empty := makeProfile("./idle", nil)
	if err := Diff(&buf, empty, empty, 5); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Neither profile contains any energy") {
		t.Errorf("got:\n%s", buf.String())
	}
}
