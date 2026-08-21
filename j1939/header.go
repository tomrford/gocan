// Package j1939 identifies and passively reassembles SAE J1939 messages from
// raw classical CAN frames.
package j1939

import (
	"fmt"

	"github.com/tomrford/gocan"
)

const (
	// MaxPGN is the largest 18-bit J1939 parameter group number.
	MaxPGN PGN = 0x3ffff
	// GlobalAddress is the J1939 broadcast destination address.
	GlobalAddress Address = 0xff
)

// PGN identifies one J1939 parameter group.
type PGN uint32

// Address identifies one J1939 network address.
type Address uint8

// Header is the J1939 meaning of one 29-bit CAN identifier.
//
// Destination is the PDU-specific field for PDU1 PGNs. PDU2 PGNs are global
// and always use GlobalAddress; their PDU-specific field remains part of PGN.
type Header struct {
	Priority    uint8
	PGN         PGN
	Source      Address
	Destination Address
}

// ParseID decodes a 29-bit CAN identifier into its J1939 fields.
func ParseID(id uint32) (Header, error) {
	if id > gocan.MaxExtendedID {
		return Header{}, fmt.Errorf("J1939 identifier %#x exceeds %#x", id, gocan.MaxExtendedID)
	}

	pgn := PGN((id >> 8) & uint32(MaxPGN))
	destination := GlobalAddress
	if pgn.isPDU1() {
		destination = Address(pgn & 0xff)
		pgn &= 0x3ff00
	}
	return Header{
		Priority:    uint8(id >> 26),
		PGN:         pgn,
		Source:      Address(id),
		Destination: destination,
	}, nil
}

// ID encodes header as a 29-bit CAN identifier.
func (header Header) ID() (uint32, error) {
	if header.Priority > 7 {
		return 0, fmt.Errorf("J1939 priority %d exceeds 7", header.Priority)
	}
	if err := header.PGN.validate(); err != nil {
		return 0, err
	}

	pgn := header.PGN
	if pgn.isPDU1() {
		if pgn&0xff != 0 {
			return 0, fmt.Errorf("J1939 PDU1 PGN %#x includes a destination", pgn)
		}
		pgn |= PGN(header.Destination)
	} else if header.Destination != GlobalAddress {
		return 0, fmt.Errorf("J1939 PDU2 PGN %#x requires the global destination", pgn)
	}

	return uint32(header.Priority)<<26 |
		uint32(pgn)<<8 |
		uint32(header.Source), nil
}

func (pgn PGN) validate() error {
	if pgn > MaxPGN {
		return fmt.Errorf("J1939 PGN %#x exceeds %#x", pgn, MaxPGN)
	}
	if pgn.isPDU1() && pgn&0xff != 0 {
		return fmt.Errorf("J1939 PDU1 PGN %#x includes a destination", pgn)
	}
	return nil
}

func (pgn PGN) isPDU1() bool {
	return uint8(pgn>>8) < 0xf0
}
