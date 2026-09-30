package report

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"html"
	"io"
	"os"

	"github.com/useless-husband/wattflame/internal/profile"
)

//go:embed viewer.html
var viewer []byte

var (
	titleMark   = []byte("__WATTFLAME_TITLE__")
	profileMark = []byte("__WATTFLAME_PROFILE__")
)

// HTML writes a single self-contained page: the viewer with the profile
// embedded. It needs no server and no network.
func HTML(w io.Writer, p *profile.Profile) error {
	// encoding/json escapes <, > and & by default, so the data cannot close
	// the <script> element it is placed in.
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	page := bytes.Replace(viewer, titleMark, []byte(html.EscapeString(truncate(Title(p), 80))), 1)
	i := bytes.Index(page, profileMark)
	if i < 0 {
		return io.ErrUnexpectedEOF
	}
	if _, err := w.Write(page[:i]); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = w.Write(page[i+len(profileMark):])
	return err
}

// SaveHTML writes the page to a file.
func SaveHTML(path string, p *profile.Profile) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := HTML(f, p); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
