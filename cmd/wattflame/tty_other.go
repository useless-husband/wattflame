//go:build !(darwin || linux)

package main

func inForeground() bool { return false }
