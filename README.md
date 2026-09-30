# wattflame

An energy profiler for macOS on Apple Silicon. It shows which functions of a program the CPU's energy went to, as a flame graph whose widths are joules.

[繁體中文](README.zh-TW.md)

![Two threads run the same function for the same time. One draws 2.7 W, the other 0.24 W.](docs/img/cores-power.png)

The picture is the `cores` example: two threads run the same function for the same four seconds, one on performance cores and one on efficiency cores. Drawn by CPU time, as above, the two halves are equal, which is all a time profiler can tell you. Coloured by power, one half draws 2.7 W and the other 0.24 W. Switch the width to energy and the split becomes 92% to 8%:

![The same profile with widths proportional to energy.](docs/img/cores-energy.png)

Apple Silicon has two kinds of core and changes their clock speed constantly, so time and energy have stopped being the same question. wattflame answers the energy one, down to the function and the source line, for any native program, without changing or recompiling it and without root.

## Quick start

Needs macOS 13 or later on Apple Silicon, Go 1.26+ and the Xcode command line tools.

```console
$ git clone https://github.com/useless-husband/wattflame && cd wattflame
$ make build examples
$ ./wattflame record -- examples/bin/pipeline 3

  examples/bin/pipeline 3
  pid 27279 · exit 0 · Apple M5

  Energy     41.8 J   13.9 W average over 3.01 s
  CPU time   5.94 s   7.05 W while on a core
  Cores      Super 41.8 J (99.98%) · Efficiency 9.53 mJ (<0.1%)
  Accounted  100.0% of what the kernel billed to the process
  Profiler   250 mJ of its own (0.6% on top), 1000 Hz, 8.73 µs pause per stack

      SELF         TOTAL   POWER  FUNCTION
    15.3 J  36.5%  36.5%  8.54 W  log1p  libsystem_m.dylib
    7.21 J  17.2%  17.2%  7.98 W  _platform_memmove  libsystem_platform.dylib
    5.16 J  12.3%  16.2%  5.38 W  _qsort  libsystem_c.dylib
    3.24 J   7.7%   7.7%  8.82 W  DYLD-STUB$$log1p  pipeline
    2.87 J   6.8%  16.5%  5.77 W  __vfprintf  libsystem_c.dylib
    ...

  profile      wattflame.json
  flame graph  wattflame.html
```

Open `wattflame.html` in a browser (or pass `--open`). It is one file with no dependencies and no network access.

Reading the summary: `Energy` is the total and the average power over the run. `CPU time` is time actually spent on a core, with the average power while there. `Cores` splits the energy between performance and efficiency cores. `Accounted` sets the profile's total against the kernel's own total for the same processes. `Profiler` is what wattflame itself used, which is not part of the result. In the table, `SELF` is the function alone, `TOTAL` includes what it called, and `POWER` is its average power while on a core.

![The flame graph page for the pipeline example, with a tooltip open.](docs/img/pipeline.png)

## Commands

| Command | What it does |
|---|---|
| `wattflame record -- <command> [args]` | Run a program and profile it, together with every process it starts. |
| `wattflame record -p <pid> -d 10s` | Profile a process that is already running. Needs `sudo`. |
| `wattflame report <profile.json>` | Print the summary again; `-html out.html` rebuilds the page, `-folded out.txt` exports folded stacks for speedscope, inferno or `flamegraph.pl` (`-metric energy\|cpu\|samples`). |
| `wattflame diff <before.json> <after.json>` | Compare two recordings: totals, and the functions whose energy changed most. |

`record` flags: `-o` profile path, `-hz` sampling rate (default 1000), `-d` stop after a duration, `-depth` maximum stack depth, `-top` rows in the summary, `-open`, `-no-html`, `-q`. The exit code of `record` is the exit code of the program it ran.

The page has four controls: width by energy or by CPU time; colour by whose code it is (the program, other libraries, macOS) or by power relative to the run's average; threads and same-named processes merged or separate; and a search box. Click a block to zoom, Esc to reset. A table below lists every function with self and total energy, CPU time and power, and the hottest source lines appear in the tooltip when the binary has debug information. The interface is in English and Traditional Chinese.

## How it works

```
                    wattflame                                   your program
        ┌──────────────────────────────┐             ┌──────────────────────────────┐
        │ listener   ◄─── task port ───┼─────────────┼── injected library, runs     │
        │                              │             │   before main(), then idle   │
        │ sampler thread, every 1 ms:  │             │                              │
        │   read each thread's energy ─┼── kernel ───┼─► per-thread counters        │
        │   suspend, walk frame chain ─┼── Mach ─────┼─► thread registers, stack    │
        │   resume                     │             │                              │
        │ charge each stack the energy │             └──────────────────────────────┘
        │ used since the previous one  │
        │ symbolicate, build call tree │──► wattflame.json, wattflame.html
        └──────────────────────────────┘
```

1. **Energy comes from the kernel, per thread.** Since macOS 13 the kernel keeps a running total of the energy each thread's cores spent executing it, in nanojoules, separately for each kind of core. It is readable for your own processes with `proc_pidinfo(PROC_PIDTHREADCOUNTS)`. These are measurements from the chip's own energy counters; wattflame does not estimate power from a model.
2. **Stacks come from outside the process.** A small library injected with `DYLD_INSERT_LIBRARIES` sends the program's Mach task port to wattflame before `main()` runs, then does nothing further. With that port the sampler suspends each runnable thread for a few microseconds, copies its registers, walks the frame-pointer chain, and resumes it. Symbols come from CoreSymbolication, the framework behind Apple's `sample` and `atos`.
3. **Each stack gets the energy used since the one before it.** The kernel brings a thread's energy total up to date when the thread leaves its core. Suspending a thread to read its stack takes it off its core, so every sample comes with a fresh reading, and the difference from the previous reading is charged to that stack. At the default rate that is one reading per millisecond per running thread.
4. **What happens between samples is handled separately.** When a thread stops right after a sample, the energy of that last stretch goes to the stack it was last seen on. When a whole burst of work falls between two samples, the energy is held until a sample does catch the thread working, which happens in proportion to how long its bursts are. When a thread exits, its last moments are gone from its own counters but not from the kernel's total for the process; the difference is paid to the stack the thread was last seen running on.
5. **Processes that cannot be sampled are still counted.** macOS system binaries (the compiler and linker among them), hardened apps and Intel binaries under Rosetta never hand over a task port. They are found by walking the process tree, and their energy is read from the same per-thread counters, which need no task port. They appear as a block labelled `[no stacks]`. What any process uses while the system loads it, before its own code starts, is labelled `[process startup]`.

[docs/DESIGN.md](docs/DESIGN.md) covers the details: the link-register trick for leaf functions, what happens across `fork` and `exec`, and why the injected library must be built for three architectures. A plain-language walkthrough in Traditional Chinese is in [docs/導讀.zh-TW.md](docs/導讀.zh-TW.md).

## Does it measure correctly?

`examples/validate` runs four phases one after another and, around each phase, asks the kernel for the whole process's energy. That is ground truth obtained without sampling. `make -C examples validate` records the program and compares what wattflame attributed to each phase function:

```console
$ make -C examples validate
PHASE                    KERNEL    WATTFLAME DIFFERENCE
phase_float           6571.8 mJ    6571.8 mJ     -0.0%
phase_integer         3549.4 mJ    3549.4 mJ     +0.0%
phase_background       261.5 mJ     261.5 mJ     +0.0%
phase_bursty            19.2 mJ      19.2 mJ     -0.0%
all phases           10402.0 mJ   10402.0 mJ     +0.0%
```

Over repeated runs the three long phases agree to within 0.1% and the bursty one to within 1%.

Every recording also reports two checks of its own:

- `Accounted` is the profile's total energy as a share of what the kernel billed to the processes wattflame tracked. It should be 100%. When threads ended between readings, it also says how much was read thread by thread and how much had to come from the process totals.
- `Seen` appears when the recording holds noticeably less CPU time than the kernel reports for the command and all its children, which happens when child processes were too short-lived to be noticed or ran as another user.

The test suite checks the same thing under stress: 100 threads that live for 4 ms each, 150 `fork`-then-`exec` chains, 300 processes that exit at once. Each time the records are compared with the kernel's own totals.

The phases above also show why this is worth measuring. `phase_integer` and `phase_background` run the same loop for the same 1.5 s; on efficiency cores it costs 262 mJ instead of 3549 mJ.

## Overhead

Measured on an Apple M5 (4 performance + 6 efficiency cores), macOS 27.0, while other jobs were running on the machine, so treat these as indicative.

| Workload | Profiler's own energy | Pause per stack |
|---|---|---|
| `examples/bin/pipeline 3` (2 busy threads, 13 W) | 0.6% of the program's | 10 µs |
| `examples/bin/cores 4` (2 busy threads, 2.8 W) | 2.8% of the program's | 14 µs |
| `go build` of this repository with a cold cache (555 processes) | 1.5% of the build's | 75 µs |

Wall time for the build, alternating runs without and with wattflame: 2.42, 2.89, 2.68 s against 3.20, 3.61, 2.78 s. Most of the difference is per-process work: every new process is met with a handshake and a symbol table. For that build the summary read `Accounted 100.3% ... (97.9% read thread by thread)` and `Seen 96.8%`.

The profiler's own energy is measured the same way as the target's, from its sampler thread's counters, and is printed with every recording. It is not included in the profile.

## What can be profiled

| Program | Result |
|---|---|
| Native binaries you built. Tested with C, C++, Swift, Rust and Go | Full stacks, readable function names, source lines with debug info |
| Homebrew-built tools and interpreters | Full stacks of the native code |
| Node.js and other JIT runtimes | Native frames resolved; JIT-compiled code shows as `[unknown]` |
| Child processes, `fork` and `exec` | Followed automatically |
| macOS system binaries, Apple's compiler and linker, hardened or notarised apps (python.org's Python among them), Intel binaries under Rosetta | Energy per process and thread, no stacks (`[no stacks]`) |
| Children that run as another user (setuid programs) | Not counted; the kernel does not let their counters be read |

## Limitations

- **CPU energy only.** GPU, memory, display, storage and radios are not included; the kernel does not attribute those to threads.
- **Resolution is the sampling interval.** Energy is read once per sample, so code that alternates faster than that (1 ms by default) shares each reading in proportion to how often it is caught. `-hz` raises the rate at the cost of more pauses.
- **Very short processes that cannot announce themselves** (system binaries that live for less than about 10 ms) are found by polling and are mostly missed. The `Seen` line shows how much that amounts to.
- **If wattflame itself is killed with SIGKILL** while it has a thread paused (about 10 µs out of every millisecond per running thread), that thread stays paused and the program has to be killed as well. Ctrl-C, SIGTERM, SIGHUP and SIGQUIT are handled and leave nothing behind.
- **Frame pointers are required** for stacks. That is the default on arm64 macOS; code built with `-fomit-frame-pointer` yields truncated stacks.
- **No JIT symbolication**, no kernel stacks, and inlined functions are reported under their caller.
- **Attach mode (`-p`) has not been tested by me**: it needs root, which I did not have while developing. Launch mode is what the tests cover.
- The per-thread energy interface is private kernel API (`PROC_PIDTHREADCOUNTS`), as is CoreSymbolication. Both have been stable since macOS 13 and are loaded defensively, but Apple can change them.
- Where the kernel reports no per-thread energy, wattflame still records stacks and CPU time and energy reads zero. I expect virtual machines, such as hosted CI runners, to be such a case but have not confirmed it; the tests skip their energy assertions there.

## Related work

To my knowledge, as of September 2026, no other tool produces a per-function energy profile of an arbitrary native process on macOS. The closest work I found:

- [samply](https://github.com/mstange/samply) with the [Firefox Profiler](https://profiler.firefox.com) is the best time profiler on the platform and the model for wattflame's injected-library handshake. The Firefox Profiler can show a power track for Firefox itself, as a per-process line next to the samples; its call tree is weighted by samples, time or bytes, not energy, and samply does not record power on macOS.
- Apple's Instruments has a Power Profiler for iPhone and iPad apps that reports power impact per subsystem, to be correlated with a CPU profile by hand. `powermetrics`, [asitop](https://github.com/tlkh/asitop) and [macpow](https://github.com/k06a/macpow) report power per machine or per process.
- [zeus-apple-silicon](https://github.com/ml-energy/zeus-apple-silicon) measures a region of code you mark by hand. [CodeGreen](https://github.com/SMART-Dal/codegreen) attributes energy to functions by instrumenting source code. [JoularJX](https://github.com/joular/joularjx) does it for JVM methods.
- [PowerMetricsKit](https://github.com/Androp0v/PowerMetricsKit) reads the same per-thread kernel counters from inside an app, without stacks.
- The idea of energy-weighted profiling is old: PowerScope (Flinn and Satyanarayanan, 1999) correlated stack samples with a multimeter, and eprof (Pathak et al., 2012) did it for phone apps. What is new here is that the kernel now supplies exact per-thread energy, so no external meter or power model is needed.

## Build and test

```console
$ make build      # builds the injected library, then the wattflame binary
$ make test       # unit and integration tests
$ make race       # the same under the race detector
$ make lint       # gofmt, go vet, staticcheck
$ make bench      # accuracy check, then the profiler's cost on the examples
```

The integration tests compile a small C program and profile it for real. `wattflame report` and `wattflame diff` are plain Go: CI runs their tests on Linux and checks that they compile for Windows.

## Licence

MIT. See [LICENSE](LICENSE).
