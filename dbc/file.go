package dbc

import (
	"bytes"
	"fmt"
	"os"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// ParseFile reads and parses a DBC file. It accepts UTF-8, with an optional
// byte-order mark, and falls back to Windows-1252 for legacy files.
func ParseFile(path string) (*Database, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	source = bytes.TrimPrefix(source, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(source) {
		source, err = charmap.Windows1252.NewDecoder().Bytes(source)
		if err != nil {
			return nil, fmt.Errorf("decode %q as Windows-1252: %w", path, err)
		}
	}

	return Parse(path, string(source))
}
