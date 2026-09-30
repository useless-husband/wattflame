# Design

This document explains how wattflame works, which problems turned out to be hard, and which alternatives were rejected. File names are relative to the repository root.

## The goal

Answer "which functions did the energy go to?" for an unmodified native program on Apple Silicon, without root, with numbers that can be checked against an independent source.

Three things are needed: a source of energy that is specific to the program, stacks that say what the program was doing, and a way to join the two.

## Layout

| Path | Role |
|---|---|
| `preload/preload.c` | The library injected into launched programs. Sends the task port, then stays out of the way. |
| `internal/sampler/*.c`, `wf.h` | The native sampler: counters, stack walking, process tracking, the handshake listener, symbolication. |
| `internal/sampler/*.go` | cgo bindings. Everything above this line is macOS on arm64 only; other platforms get a stub. |
| `internal/profile` | The data model: a call tree with weights, built from sampler records. Pure Go. |
| `internal/demangle` | Rust symbol demangling. |
| `internal/report` | Terminal summary, diff, and the HTML page (`viewer.html`, one file of HTML, CSS and JavaScript). |
| `cmd/wattflame` | The command line. |
| `examples/` | Workloads, and `validate`, which compares attribution with ground truth. |

## Where the energy numbers come from

Since macOS 13 the XNU kernel's `recount` subsystem keeps, for every thread and separately for each performance level (kind of core), the instructions retired, cycles, user and system time, and energy in nanojoules. `proc_pidinfo` with the private flavor `PROC_PIDTHREADCOUNTS` (34) returns them for a thread of any process owned by the same user. The energy field is filled from the CPU's own energy counters at context switch. A per-process total is available from `proc_pid_rusage` as `ri_energy_nj`.

These are measurements, not a model. They are also the only energy figures macOS attributes below the process level, which is why wattflame covers CPU energy and nothing else.

One property of the counters shapes the rest of the design: a thread's energy total only moves when the thread leaves its core. Read passively, a thread that never blocks shows new energy only when its quantum expires, every 10 ms on an M5 (cycles and time move about every 5 ms). A thread that blocks often is updated at each block.

Rejected: `powermetrics` needs root and reports per process at best. IOReport's energy channels are per core cluster, so they cannot separate two programs sharing a cluster. Estimating power from a model of instruction counts and frequency would be a guess where a measurement exists.

## Getting stacks without root

Reading another process's registers and memory needs its Mach task port. `task_for_pid` refuses unprivileged callers.

The route taken is the one [samply](https://github.com/mstange/samply) uses. wattflame starts the program with `DYLD_INSERT_LIBRARIES` pointing at a tiny library. Its constructor, which runs before `main`, looks up a Mach service that wattflame registered under a random name passed in an environment variable, and sends it `mach_task_self()`. The library then waits for a one-word reply before returning, so the program cannot run ahead of the sampler. wattflame checks the sender with `pid_for_task` rather than believing the pid in the message.

Because of that wait, nothing of the program's own code runs unobserved. A test (`TestProgramWaitsForStart`) creates a file as the first statement of `main` and asserts the file does not exist until `Start` is called.

### Three ways the injected library could break other programs

The environment variable is inherited by every child process, including ones that wattflame never meant to look at. While profiling a `go build`, that took down the linker's helper. Three separate causes had to be handled; each is now covered by a test or a documented check.

1. **Immovable task ports.** For Apple's own binaries (the compiler, the linker and `dsymutil` from the command line tools are "platform binaries") and for hardened-runtime apps, the kernel marks the task port immovable. Sending it raises a fatal `EXC_GUARD`. The library therefore asks `csops` for the process's code-signing flags and stays silent if `CS_PLATFORM_BINARY` or `CS_RUNTIME` is set, or if the flags cannot be read.
2. **Architecture.** dyld aborts a process when an inserted library has no slice for its architecture ("terminating because inserted dylib ... could not be loaded"). The library is therefore built for arm64, arm64e and x86_64. Only the arm64 slices contain code; the Intel slice exists so that a Rosetta process can start.
3. **Blocking.** The constructor waits for a reply. If wattflame has exited, the service lookup fails and the constructor returns at once; if wattflame is present but stuck, the wait times out after five seconds.

Processes in the first two groups still inherit the variable and pass it on, so their children are profiled if they can be.

## Walking stacks

For each runnable thread (`walk_stack` in `sampler_darwin_arm64.c`): `thread_suspend`, `thread_get_state` for pc, fp and lr, then follow the frame-pointer chain, reading the target's stack with `mach_vm_read_overwrite`, then `thread_resume`. One read fetches from the frame record to the end of its 16 KB page, which usually contains the next dozen frames, so a typical stack costs one or two reads. A thread is suspended for about 10 µs.

Return addresses are masked to 47 bits to strip pointer authentication codes. The walk stops if a frame pointer is misaligned or does not point to a higher address than the one before, which prevents loops on corrupt or foreign frame records.

### The caller of a leaf function

A frame-pointer walk starts at the frame record that `fp` points to. While a function is in its prologue or epilogue, or when it is a leaf that never sets up a frame, `fp` still belongs to its caller, so the walk yields the caller's caller and the caller itself is missing. In exactly those states the link register holds the return address into the caller.

The sampler records `lr` with every stack, and the profile builder (`leafCaller` in `internal/profile/builder.go`) inserts it as the second frame unless one of two things is true: `lr` equals the first return address the walk found (the function has a frame and has not called anything yet), or `lr` points back into the function the pc is in (a leftover from the last call it made). Deciding the second case needs the bounds of the current function, which come from the symbol table. When a function's bounds are unknown, as in a stripped binary, `lr` is ignored.

`TestFramelessLeafKeepsItsCaller` profiles a real frameless leaf and checks both that the situation occurs and that the caller is recovered.

Rejected: parsing compact unwind information would be exact but is a large amount of code for the last frame of a stack.

### Stub islands

System libraries in the dyld shared cache call each other through small stubs that live in the cache but belong to no library. CoreSymbolication has no owner for those addresses. They are recognised by range (the shared cache is mapped at the same address in every process, so wattflame's own mapping gives the range), labelled `[shared cache stub]`, and treated as frameless, so their caller comes from `lr`.

## Joining energy and stacks

Per thread, the sampler keeps `prev` (last counter values read), `acc` (deltas not yet attributed), a list of pending stacks, and a copy of the last stack taken.

Each tick, for each thread:

1. Read the counters; add the deltas to `acc`.
2. If the thread is runnable (`TH_STATE_RUNNING`), take a stack and append it to the pending list.
3. If `acc` holds energy and there are pending stacks, split `acc` evenly across them and emit one record per stack. The integer remainder goes to the last stack, so totals are preserved exactly.

The first design assumed that about ten stacks would share each 10 ms energy delta. Measuring showed otherwise: suspending a thread to read its stack takes it off its core, the kernel folds its energy in at that moment, and the next read of the counters already contains it. Over thousands of flushes the average number of stacks sharing one delta was 1.00. So in practice each stack is charged the energy used since the previous stack, and the resolution is the sampling interval. The even split remains as the rule for the rare reading that arrives late.

Three situations fall outside that loop.

### The end of a run

A thread that is sampled at tick k and blocks before tick k+1 has energy at k+1 but is no longer runnable. That energy is the tail of the run just sampled, so it is added to the last stack (if that stack was taken while the thread ran, within the last three ticks). An earlier version let it wait for the thread's next run, which moved up to a millisecond of energy from the end of one phase of a program to the start of the next; the per-phase validation showed it as errors of 1 to 3% on short phases and is exact since.

### A burst between two ticks

A thread that wakes, works for 200 µs and blocks again is almost never runnable at a tick and has no recent stack. The first version took a stack wherever such a thread was waiting and charged the energy there. Profiling the Go toolchain with that version put 45% of the energy on `__psynch_cvwait`, which is where Go's scheduler threads sleep, not where they work.

The fix is to wait. The energy is held until a tick does catch the thread running, and then goes to that stack. A thread is caught with probability proportional to how long its bursts are, so over many bursts the energy lands on the code that ran, in proportion to time, which is the assumption every sampling profiler makes. If a thread is not caught for 50 ms, a stack is taken where it waits and used. On the same workload `__psynch_cvwait` dropped to about 5%.

### A thread that exits

A thread's counters disappear with it. Whatever it used after its last reading can never be read from the thread, and for a thread that lives 4 ms that is a third of its energy. The kernel's total for the process still contains it. When a thread ends, the stack it was last seen running on is queued; the difference between the process total and everything the thread counters ever showed (the residual) is then paid out to the queued stacks. A payout waits three ticks for the kernel to book the dead thread, then takes the smallest residual seen over three more, because a counter read an instant before its thread folded energy in looks like a residual for one tick.

When a process ends, the same is done with its final total. For wattflame's own child that total is exact, because a zombie's `proc_pid_rusage` still answers until the process is reaped, and wattflame reaps it only afterwards. If nothing is queued to take a residual, it is emitted as `[unsampled tail]`.

### What the records carry

Each record is a stack plus four weights per performance level: energy, CPU time, cycles, instructions. The profile therefore knows, for every call-tree node, how much ran on which kind of core, and can report power (energy divided by on-core time) per function. Records that add to a stack already emitted are flagged so that sample counts stay honest.

## Processes

A **full** target has a task port: stacks and energy. An **opaque** target is known only by pid: energy from the per-thread counters, which need no port, and no stacks.

- The launched program is entered as opaque the moment it is spawned and upgraded in place when its handshake arrives. The energy it used before that, while dyld was loading it, is kept and labelled `[process startup]`. On a build that starts hundreds of processes this is a visible share (7% of a `go build`).
- The program is not spawned suspended. A program that can be sampled parks itself in the handshake; one that cannot is counted from its first instruction because its counters start at zero. Starting it suspended would add nothing and would leave it stopped for good if wattflame died at the wrong moment.
- Every 10 ms the sampler lists the children of every tracked process (`proc_listpids(PROC_PPID_ONLY)`). Unknown ones become opaque targets; those that can announce themselves are upgraded. Children whose counters cannot be read at all (setuid programs running as another user) are left out.
- `fork`: the child's counters start at zero (verified by experiment), so a process that started during the recording is counted from zero and nothing is lost between its birth and its discovery. The library also registers a `pthread_atfork` handler so a forked child announces itself.
- `exec`: the new image keeps the pid, gets a new task and new thread ids, and the kernel copies the calling thread's counters into the new thread (also verified by experiment). The entry for the old image is closed and hands its last counter values, together with the process total read at the same tick, to the entry that replaces it. Starting the new entry from zero would bill the old image's energy a second time; starting it from the current values would drop what the process used between the old image's last reading and the first look at the new one.
- The handshake listener accepts a port only if it can list the task's threads with it. `pid_for_task` alone is not enough: it also answers for a task *name* port, which any process of the same user can get for any other, and which would otherwise let a stranger have a live entry closed by announcing a fake `exec`.

Symbolicators are created after the process has been released from its handshake, not before, and freed a few drains after the process has ended.

## Symbols

CoreSymbolication (`symbolicate_darwin_arm64.c`) resolves addresses to function, module, file and line. It is a private framework, loaded with `dlopen` so that its absence degrades to raw addresses. It understands the shared cache and dSYMs and keeps answering after the target has exited, which matters because the last samples of a program are symbolicated after it is gone. New addresses are resolved every 50 ms while recording, so most lookups happen while the task is alive.

C++ and Swift names arrive demangled. Rust's v0 names do not, so `internal/demangle` implements RFC 2603. Its output is compared with the `rustc-demangle` crate on 704 symbols from real binaries (`testdata/rust_v0.tsv`), and it is fuzzed.

## Checking the result

- **Accounted.** Every recording reports its total energy as a share of the kernel's per-process totals (`ri_energy_nj`) for the processes it tracked, and how much of that the per-thread counters showed directly. The process totals and the thread sums are always read in the same tick; comparing a final total with counters read a few milliseconds earlier produced figures above 100%.
- **Seen.** In launch mode `wait4` returns the CPU time of the program and every descendant it reaped. If the recording holds clearly less than that, processes were missed, and the summary says how much. The two figures come from different kernel accounts and differ by a few percent on their own, so the line only appears below 97%.
- **Ground truth per phase.** `examples/validate.c` runs four phases and reads the process total around each one from inside the program. `examples/validate.sh` compares those with the energy wattflame attributed to each phase function.
- **Conservation under stress.** The sampler tests run short-lived threads, fork-then-exec chains and hundreds of short processes, and require the records to add up to the kernel's totals.

## What review found

Before release the code was given to an independent reviewer with instructions to break it. The confirmed findings, all fixed and covered by tests in `internal/sampler/sampler_darwin_arm64_test.go` and `cmd/wattflame`:

- Short-lived processes lost almost all their energy (their startup was dropped and their end never read): 34 ms of 769 ms of CPU time for a thousand empty programs. Now about 95%.
- Short-lived threads lost their last quantum: 1.36 J of 2.00 J for 150 threads of 4 ms. Now paid from the process total to the threads' own stacks.
- The process table was a fixed 4096 entries and overflowed silently.
- A setuid child was taken for dead and found again every 10 ms.
- A forged handshake carrying a task name port closed a live entry.
- A SIGINT sent to wattflame alone never reached the program; SIGQUIT killed wattflame on the spot.
- A command line containing the page template's marker broke the HTML.
- The program was spawned suspended, so a wattflame killed in the first milliseconds left it stopped.

One finding is not fixed: if wattflame is killed with SIGKILL during the roughly 10 µs in which it has a thread suspended, that thread stays suspended. The kernel offers a self-releasing suspension only for whole tasks (`task_suspend2`), and stopping every thread of a program for each sample would distort what is being measured more than the risk is worth. It is listed under Limitations.

## Output

A profile is one JSON document: metadata, a frame table, and tree nodes each with a parent index, a frame index and self weights (`internal/profile/profile.go`). Nodes are stored parents-first, so inclusive totals are one reverse pass. Weights are written as a short array trimmed to the performance levels that hold data, and omitted for nodes that were never a leaf.

The HTML page embeds the JSON in a `<script type="application/json">` element. `encoding/json` escapes `<`, `>` and `&`, so a function name or command line cannot close the element, and every string is inserted into the page with `textContent`. The flame graph is drawn on a canvas; the table below it carries the same data for screen readers and keyboard users.

## What was left out

- Attach to a hardened or system process: not possible with System Integrity Protection on, even as root.
- GPU and other non-CPU energy: no per-thread attribution exists to read.
- JIT symbol maps (`perf-<pid>.map` and the like).
- Kernel stacks.
