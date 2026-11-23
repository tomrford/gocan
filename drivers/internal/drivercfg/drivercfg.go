// Package drivercfg holds configuration types and sentinel errors shared
// between package drivers and its internal vendor backends.
package drivercfg

import "errors"

// ErrInvalidConfig identifies an invalid or unsupported channel or bus
// configuration rejected before opening hardware.
var ErrInvalidConfig = errors.New("drivers: invalid configuration")

// Termination selects the physical bus termination resistor.
type Termination uint8

const (
	// TerminationDefault leaves the native driver's default setting.
	TerminationDefault Termination = iota
	// TerminationOff disables the termination resistor.
	TerminationOff
	// TerminationOn enables the termination resistor.
	TerminationOn
)

// Valid reports whether termination is one of the defined choices.
func (termination Termination) Valid() bool {
	return termination == TerminationDefault || termination == TerminationOff || termination == TerminationOn
}
