package profile

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWeightsJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		w    Weights
		json string
	}{
		{"zero", Weights{}, "[0,0]"},
		{"samples only", Weights{Samples: 3}, "[3,0]"},
		{"one level", Weights{Samples: 2, EnergyNJ: [MaxLevels]uint64{10}, CPUNs: [MaxLevels]uint64{20}, Cycles: [MaxLevels]uint64{30}, Instr: [MaxLevels]uint64{40}},
			"[2,1,10,20,30,40]"},
		{"second level only", Weights{Samples: 1, EnergyNJ: [MaxLevels]uint64{0, 5}},
			"[1,2,0,5,0,0,0,0,0,0]"},
		{"all levels", Weights{Samples: 9, EnergyNJ: [MaxLevels]uint64{1, 2, 3, 4}, CPUNs: [MaxLevels]uint64{5, 6, 7, 8},
			Cycles: [MaxLevels]uint64{9, 10, 11, 12}, Instr: [MaxLevels]uint64{13, 14, 15, 1 << 60}},
			"[9,4,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,1152921504606846976]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.w)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tt.json {
				t.Errorf("marshal = %s, want %s", b, tt.json)
			}
			var back Weights
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatal(err)
			}
			if back != tt.w {
				t.Errorf("round trip = %+v, want %+v", back, tt.w)
			}
		})
	}
}

func TestWeightsUnmarshalRejectsBadInput(t *testing.T) {
	for _, in := range []string{`[1]`, `[1,2,3]`, `[1,5,0,0,0,0]`, `"x"`, `[1,1,1,1,1]`} {
		var w Weights
		if err := json.Unmarshal([]byte(in), &w); err == nil {
			t.Errorf("Unmarshal(%s) succeeded: %+v", in, w)
		}
	}
	var w Weights
	if err := json.Unmarshal([]byte(`[]`), &w); err != nil || !w.IsZero() {
		t.Errorf("empty array: %v %+v", err, w)
	}
}

func TestWeightsDerived(t *testing.T) {
	w := Weights{EnergyNJ: [MaxLevels]uint64{3e9, 1e9}, CPUNs: [MaxLevels]uint64{1e9, 1e9}, Cycles: [MaxLevels]uint64{5, 6}, Instr: [MaxLevels]uint64{7, 8}}
	if w.Energy() != 4e9 || w.CPU() != 2e9 || w.TotalCycles() != 11 || w.TotalInstr() != 15 {
		t.Errorf("sums wrong: %d %d %d %d", w.Energy(), w.CPU(), w.TotalCycles(), w.TotalInstr())
	}
	if got := w.Watts(); got != 2 {
		t.Errorf("Watts = %v, want 2", got)
	}
	if got := (&Weights{EnergyNJ: [MaxLevels]uint64{5}}).Watts(); got != 0 {
		t.Errorf("Watts with no CPU time = %v", got)
	}
}

func TestNodeJSONOmitsEmptyWeights(t *testing.T) {
	b, _ := json.Marshal(Node{Parent: 3, Frame: 4})
	if string(b) != `{"p":3,"f":4}` {
		t.Errorf("got %s", b)
	}
	b, _ = json.Marshal(Node{Parent: -1, Frame: -1, Self: Weights{Samples: 1}})
	if string(b) != `{"p":-1,"f":-1,"w":[1,0]}` {
		t.Errorf("got %s", b)
	}
	var n Node
	if err := json.Unmarshal([]byte(`{"p":3,"f":4}`), &n); err != nil || n.Parent != 3 || n.Frame != 4 || !n.Self.IsZero() {
		t.Errorf("unmarshal: %v %+v", err, n)
	}
}

// tree builds a profile from "parent/child" edges. Frames are functions
// unless the name starts with "T:" (thread) or "P:" (process).
func tree(nodes []struct {
	parent int32
	name   string
	energy uint64
}) *Profile {
	p := &Profile{Format: formatName, Version: formatVersion, Nodes: []Node{{Parent: -1, Frame: -1}}}
	idx := map[string]int32{}
	for _, n := range nodes {
		f, ok := idx[n.name]
		if !ok {
			f = int32(len(p.Frames))
			fr := Frame{Name: n.name}
			switch {
			case strings.HasPrefix(n.name, "T:"):
				fr.Kind, fr.Name = KindThread, n.name[2:]
			case strings.HasPrefix(n.name, "P:"):
				fr.Kind, fr.Name, fr.PID = KindProcess, n.name[2:], 42
			}
			p.Frames = append(p.Frames, fr)
			idx[n.name] = f
		}
		node := Node{Parent: n.parent, Frame: f}
		node.Self.EnergyNJ[0] = n.energy
		node.Self.CPUNs[0] = n.energy * 2
		if n.energy > 0 {
			node.Self.Samples = 1
		}
		p.Nodes = append(p.Nodes, node)
	}
	return p
}

type edge = struct {
	parent int32
	name   string
	energy uint64
}

func TestInclusiveAndTotal(t *testing.T) {
	p := tree([]edge{
		{0, "main", 1000}, // 1
		{1, "a", 2000},    // 2
		{1, "b", 3000},    // 3
		{2, "c", 4000},    // 4
	})
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	inc := p.Inclusive()
	want := []uint64{10000, 10000, 6000, 3000, 4000}
	for i, w := range want {
		if got := inc[i].Energy(); got != w {
			t.Errorf("inclusive[%d] = %d, want %d", i, got, w)
		}
	}
	if got := p.Total(); got.Energy() != 10000 || got.Samples != 4 {
		t.Errorf("total = %+v", got)
	}
}

func TestFunctionsCountsRecursionOnce(t *testing.T) {
	// main -> fib -> fib -> fib, plus fib called again from helper.
	p := tree([]edge{
		{0, "P:app", 0},    // 1
		{1, "T:main", 0},   // 2
		{2, "main", 1000},  // 3
		{3, "fib", 1000},   // 4
		{4, "fib", 2000},   // 5
		{5, "fib", 4000},   // 6
		{3, "helper", 500}, // 7
		{7, "fib", 250},    // 8
	})
	stats := map[string]FuncStat{}
	for _, f := range p.Functions() {
		stats[p.Frames[f.Frame].Name] = f
	}
	if len(stats) != 3 {
		t.Fatalf("got %d functions, want 3 (threads and processes are not functions): %v", len(stats), stats)
	}
	check := func(name string, self, total uint64) {
		t.Helper()
		s := stats[name]
		if s.Self.Energy() != self || s.Total.Energy() != total {
			t.Errorf("%s: self %d total %d, want self %d total %d", name, s.Self.Energy(), s.Total.Energy(), self, total)
		}
	}
	check("main", 1000, 8750)
	check("fib", 7250, 7250) // 7000 under main + 250 under helper, never double counted
	check("helper", 500, 750)

	order := p.Functions()
	if p.Frames[order[0].Frame].Name != "fib" || p.Frames[order[1].Frame].Name != "main" {
		t.Errorf("not sorted by self energy: %s, %s", p.Frames[order[0].Frame].Name, p.Frames[order[1].Frame].Name)
	}
}

func TestWriteFolded(t *testing.T) {
	p := tree([]edge{
		{0, "P:app", 0},            // 1
		{1, "T:main thread", 0},    // 2
		{1, "T:worker", 0},         // 3
		{2, "main", 1_000_000},     // 4
		{4, "work", 2_500_000},     // 5
		{3, "main", 500_000},       // 6  same stack on another thread
		{6, "work", 4_000_000},     // 7
		{6, "a;b", 3_000_000},      // 8  name containing the separator
		{6, "tiny", 999},           // 9  rounds to zero µJ
		{2, "only_samples", 0},     // 10
		{10, "never_a_leaf", 1000}, // 11
	})

	var buf bytes.Buffer
	if err := p.WriteFolded(&buf, MetricEnergy, false); err != nil {
		t.Fatal(err)
	}
	want := "" +
		"app [42];main 1500\n" +
		"app [42];main;a:b 3000\n" +
		"app [42];main;work 6500\n" +
		"app [42];only_samples;never_a_leaf 1\n"
	if buf.String() != want {
		t.Errorf("merged threads:\n%s\nwant:\n%s", buf.String(), want)
	}

	buf.Reset()
	if err := p.WriteFolded(&buf, MetricEnergy, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "app [42];main thread;main;work 2500\n") ||
		!strings.Contains(buf.String(), "app [42];worker;main;work 4000\n") {
		t.Errorf("with threads:\n%s", buf.String())
	}

	buf.Reset()
	if err := p.WriteFolded(&buf, MetricSamples, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "app [42];main;work 2\n") {
		t.Errorf("samples:\n%s", buf.String())
	}

	buf.Reset()
	if err := p.WriteFolded(&buf, MetricCPU, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "app [42];main;work 13000\n") {
		t.Errorf("cpu:\n%s", buf.String())
	}
}

func TestParseMetric(t *testing.T) {
	for in, want := range map[string]Metric{"energy": MetricEnergy, "": MetricEnergy, "CPU": MetricCPU, "time": MetricCPU, "samples": MetricSamples} {
		got, err := ParseMetric(in)
		if err != nil || got != want {
			t.Errorf("ParseMetric(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseMetric("watts"); err == nil {
		t.Error("ParseMetric(watts) succeeded")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	b := NewBuilder(newFake())
	b.SetProcess(0, "app", "/work/app")
	r := rec(1, 1234, aWork+0x30, aLeaf+0x08, aMain+0x10, aStart+0x10)
	r.W[0][1] = 77
	r.TimeNs = 25 * 1000 * 1000
	b.Add(r)
	b.Add(rec(7, 99, 0, aMemcpy+0x10, aWork+0x10))
	meta := Meta{
		Tool:       "wattflame test",
		Command:    []string{"./app", "--flag", "</script>"},
		PID:        42,
		Started:    time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
		DurationNs: 3e9,
		IntervalUs: 1000,
		Exit:       "exit 0",
		Machine:    Machine{Model: "Mac17,2", Chip: "Apple M5", OS: "macOS 27.0"},
		Processes:  []Process{{PID: 42, Name: "app", KernelEnergyNJ: 1500, ThreadsEnergyNJ: 1410}},
		Sampler:    SamplerStats{Ticks: 3000, Samples: 2, SelfEnergyNJ: 12},
	}
	p := b.Finish(meta, []Level{{Name: "Super", Cores: 4}, {Name: "Efficiency", Cores: 6}})

	path := filepath.Join(t.TempDir(), "p.json")
	if err := p.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p, back) {
		a, _ := json.Marshal(p)
		c, _ := json.Marshal(back)
		t.Errorf("round trip differs:\n%s\n%s", a, c)
	}
}

func TestReadRejectsForeignAndBrokenFiles(t *testing.T) {
	tests := map[string]string{
		"not json":       `hello`,
		"other format":   `{"format":"speedscope","version":1,"nodes":[{"p":-1,"f":-1}]}`,
		"newer version":  `{"format":"wattflame-profile","version":99,"nodes":[{"p":-1,"f":-1}]}`,
		"no nodes":       `{"format":"wattflame-profile","version":1,"nodes":[]}`,
		"root parent":    `{"format":"wattflame-profile","version":1,"nodes":[{"p":0,"f":-1}]}`,
		"forward parent": `{"format":"wattflame-profile","version":1,"frames":[{"name":"a"}],"nodes":[{"p":-1,"f":-1},{"p":2,"f":0},{"p":0,"f":0}]}`,
		"self parent":    `{"format":"wattflame-profile","version":1,"frames":[{"name":"a"}],"nodes":[{"p":-1,"f":-1},{"p":1,"f":0}]}`,
		"bad frame":      `{"format":"wattflame-profile","version":1,"frames":[{"name":"a"}],"nodes":[{"p":-1,"f":-1},{"p":0,"f":5}]}`,
		"too many levels": `{"format":"wattflame-profile","version":1,"levels":[{"name":"a"},{"name":"b"},{"name":"c"},{"name":"d"},{"name":"e"}],` +
			`"nodes":[{"p":-1,"f":-1}]}`,
	}
	for name, in := range tests {
		if _, err := Read(strings.NewReader(in)); err == nil {
			t.Errorf("%s: Read succeeded", name)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("Load of a missing file succeeded")
	}
}
