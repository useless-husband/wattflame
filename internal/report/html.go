package report

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
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
	// Both markers are located in the template itself, before anything is
	// substituted, so text from the profile can never be mistaken for one.
	ti := bytes.Index(viewer, titleMark)
	pi := bytes.Index(viewer, profileMark)
	if ti < 0 || pi < ti {
		return errors.New("viewer template is missing its markers")
	}
	for _, part := range [][]byte{
		viewer[:ti],
		[]byte(html.EscapeString(truncate(Title(p), 80))),
		viewer[ti+len(titleMark) : pi],
		data,
		viewer[pi+len(profileMark):],
	} {
		if _, err := w.Write(part); err != nil {
			return err
		}
	}
	return nil
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
