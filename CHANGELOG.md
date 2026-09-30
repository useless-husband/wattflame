# Changelog

## 0.1.0 (2026-10-01)

First version.

- `wattflame record`: run a program (or attach to a pid with sudo) and record a per-function energy profile from the kernel's per-thread energy counters and sampled call stacks. No root needed for launched programs.
- Child processes are followed across `fork` and `exec`. Processes that cannot be sampled (macOS system binaries, hardened apps, Intel binaries under Rosetta) are still counted, without stacks.
- Self-contained HTML flame graph: width by energy or CPU time, colour by origin of the code or by power, power-over-time chart, sortable function table, hottest source lines, English and Traditional Chinese.
- `wattflame report`: terminal summary, HTML, folded stacks for other flame graph tools.
- `wattflame diff`: compare two recordings.
- Rust v0 symbol demangling; C++ and Swift names come demangled from the system.
- Energy that a thread or process used after its last reading is recovered from the kernel's per-process total, so a profile adds up to what the kernel billed. Every recording reports that check (`Accounted`), and how much of the process tree's CPU time it saw (`Seen`).
- `examples/validate`: compares the attribution with per-phase energy read from the kernel by the program itself.
- On machines without per-thread energy counters (virtual machines), records a CPU time profile instead.
