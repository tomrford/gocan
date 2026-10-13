package gocan_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestREADMEUsageSnippets builds every Go program in README.md and runs the
// programs that do not open a physical channel. The physical snippet is
// compiled only: Discover can see a host CAN interface, and opening it during
// the suite would share that interface with the SocketCAN tests.
func TestREADMEUsageSnippets(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	snippets := readmeGoPrograms(string(readme))
	if len(snippets) != 6 {
		t.Fatalf("README has %d Go programs, want 6", len(snippets))
	}

	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for index, snippet := range snippets {
		if !strings.HasPrefix(snippet, "package main\n") {
			t.Fatalf("snippet %d is not a complete program", index+1)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(snippet), 0o644); err != nil {
			t.Fatal(err)
		}
		goMod := "module example.com/readme\n\ngo 1.25.0\n\nrequire github.com/tomrford/gocan v0.0.0\n\nreplace github.com/tomrford/gocan => " + root + "\n"
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
			t.Fatal(err)
		}

		formatted := exec.Command("gofmt", "-l", "main.go")
		formatted.Dir = dir
		if output, err := formatted.Output(); err != nil {
			t.Fatalf("snippet %d gofmt: %v", index+1, err)
		} else if len(output) > 0 {
			t.Fatalf("snippet %d is not formatted with gofmt", index+1)
		}

		tidy := exec.Command("go", "mod", "tidy")
		tidy.Dir = dir
		if output, err := tidy.CombinedOutput(); err != nil {
			t.Fatalf("snippet %d tidy: %v\n%s", index+1, err, output)
		}
		build := exec.Command("go", "build", "-o", filepath.Join(dir, "snippet"), ".")
		build.Dir = dir
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("snippet %d build: %v\n%s", index+1, err, output)
		}
		if strings.Contains(snippet, "drivers.Open(") {
			continue
		}

		run := exec.Command(filepath.Join(dir, "snippet"))
		run.Dir = dir
		if output, err := run.CombinedOutput(); err != nil {
			t.Fatalf("snippet %d run: %v\n%s", index+1, err, output)
		}
	}
}

func readmeGoPrograms(readme string) []string {
	var snippets []string
	for rest := readme; ; {
		start := strings.Index(rest, "```go\n")
		if start < 0 {
			return snippets
		}
		rest = rest[start+len("```go\n"):]
		end := strings.Index(rest, "```")
		if end < 0 {
			return snippets
		}
		snippets = append(snippets, rest[:end])
		rest = rest[end+len("```"):]
	}
}
