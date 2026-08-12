package dbc

import "os"

// ParseFile reads and parses a DBC file. Parse documents the accepted
// character encodings.
func ParseFile(path string) (*Database, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(path, string(source))
}
