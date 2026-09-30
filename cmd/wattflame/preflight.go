package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// protectedDirs are covered by System Integrity Protection. Binaries there
// ignore DYLD_INSERT_LIBRARIES and refuse task_for_pid even to root, so they
// cannot be profiled. /usr/local is the one exception under /usr.
var protectedDirs = []string{"/bin/", "/sbin/", "/usr/bin/", "/usr/sbin/", "/usr/libexec/", "/System/"}

func isProtected(path string) bool {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	for _, dir := range protectedDirs {
		if strings.HasPrefix(path, dir) {
			return true
		}
	}
	return false
}

// interpreter returns the program a script's "#!" line names, resolving
// "/usr/bin/env name" through PATH. It returns "" for anything that is not a
// script.
func interpreter(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	line, _ := bufio.NewReaderSize(f, 512).ReadString('\n')
	if !strings.HasPrefix(line, "#!") {
		return ""
	}
	fields := strings.Fields(line[2:])
	if len(fields) == 0 {
		return ""
	}
	if filepath.Base(fields[0]) != "env" {
		return fields[0]
	}
	for _, arg := range fields[1:] {
		if strings.HasPrefix(arg, "-") || strings.Contains(arg, "=") {
			continue
		}
		if resolved, err := exec.LookPath(arg); err == nil {
			return resolved
		}
		return arg
	}
	return ""
}

// whyNoStacks explains, as well as can be told from the outside, why a
// program was recorded without stacks.
func whyNoStacks(path string) string {
	if isProtected(path) {
		return fmt.Sprintf("%s is a macOS system binary, and System Integrity Protection keeps profilers out of those", path)
	}
	if interp := interpreter(path); interp != "" && isProtected(interp) {
		return fmt.Sprintf("%s is a script run by %s, a macOS system binary that cannot be sampled", path, interp)
	}
	return fmt.Sprintf("%s did not load the profiling library; it is probably signed with the hardened runtime or built for Intel", path)
}
