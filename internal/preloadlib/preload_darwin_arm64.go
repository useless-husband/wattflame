//go:build darwin && arm64

// Package preloadlib carries the small library that wattflame injects into
// the programs it launches (see preload/preload.c).
package preloadlib

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

//go:generate clang -O2 -Wall -Wextra -arch arm64 -arch arm64e -arch x86_64 -mmacosx-version-min=13.0 -dynamiclib -o libwattflame_preload.dylib ../../preload/preload.c

//go:embed libwattflame_preload.dylib
var dylib []byte

// Path returns the location of the library on disk, writing it to the user's
// cache directory the first time. The file name contains a hash of the
// contents, so different wattflame versions never share a stale copy.
func Path() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "wattflame")
	sum := sha256.Sum256(dylib)
	path := filepath.Join(dir, "libwattflame_preload-"+hex.EncodeToString(sum[:6])+".dylib")
	if st, err := os.Stat(path); err == nil && st.Size() == int64(len(dylib)) {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("cannot create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "preload-*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(dylib); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}
