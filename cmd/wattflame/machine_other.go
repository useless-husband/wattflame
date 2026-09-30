//go:build !darwin

package main

import "github.com/useless-husband/wattflame/internal/profile"

func machineInfo() profile.Machine { return profile.Machine{} }

func levelCores(level int) int { return 0 }
