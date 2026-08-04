//go:build windows

package pcan

import (
	"os"
	"strconv"
	"testing"
)

// testChannel returns the PCAN channel handle selected by the named
// environment variable, skipping the test when the variable is not set.
func testChannel(tb testing.TB, name string) Channel {
	tb.Helper()
	value := os.Getenv(name)
	if value == "" {
		tb.Skipf("%s is not set", name)
	}
	channel, err := strconv.ParseUint(value, 0, 16)
	if err != nil || channel == 0 {
		tb.Fatalf("%s=%q is not a nonzero 16-bit PCAN handle", name, value)
	}
	return Channel(channel)
}
