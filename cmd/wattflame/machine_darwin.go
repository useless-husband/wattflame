//go:build darwin

package main

import (
	"encoding/binary"
	"fmt"
	"strings"
	"syscall"

	"github.com/useless-husband/wattflame/internal/profile"
)

func sysctlString(name string) string {
	s, err := syscall.Sysctl(name)
	if err != nil {
		return ""
	}
	return strings.TrimRight(s, "\x00")
}

// sysctlUint32 reads an integer sysctl. syscall.Sysctl drops one trailing NUL
// byte, which for a small little-endian integer is part of the value, so pad
// back to four bytes.
func sysctlUint32(name string) uint32 {
	s, err := syscall.Sysctl(name)
	if err != nil {
		return 0
	}
	b := []byte(s)
	for len(b) < 4 {
		b = append(b, 0)
	}
	return binary.LittleEndian.Uint32(b)
}

func machineInfo() profile.Machine {
	m := profile.Machine{
		Model: sysctlString("hw.model"),
		Chip:  sysctlString("machdep.cpu.brand_string"),
	}
	if v := sysctlString("kern.osproductversion"); v != "" {
		m.OS = "macOS " + v
		if build := sysctlString("kern.osversion"); build != "" {
			m.OS += " (" + build + ")"
		}
	}
	return m
}

func levelCores(level int) int {
	return int(sysctlUint32(fmt.Sprintf("hw.perflevel%d.physicalcpu", level)))
}
