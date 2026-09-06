package j1939

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/tomrford/gocan"
)

// AddressClaimPGN carries a controller's NAME, including Cannot Claim (SA 254).
const AddressClaimPGN PGN = 0xee00

// ErrAmbiguous indicates competing observed identities. A passive capture does
// not establish that the lower NAME won arbitration or that a loser stopped.
var ErrAmbiguous = errors.New("ambiguous J1939 identity")

// Name is the 64-bit J1939 controller identity. Its numeric value determines
// address-claim priority: lower values win. Names are transmitted little-endian.
type Name uint64

// ParseName reads an eight-byte NAME without losing reserved bits.
func ParseName(payload []byte) (Name, error) {
	if len(payload) != 8 {
		return 0, fmt.Errorf("%w: NAME has %d bytes, want 8", ErrProtocol, len(payload))
	}
	return Name(binary.LittleEndian.Uint64(payload)), nil
}

// IdentityNumber returns the manufacturer's 21-bit identity number.
func (name Name) IdentityNumber() uint32 { return uint32(name & 0x1fffff) }

// ManufacturerCode returns the 11-bit manufacturer code.
func (name Name) ManufacturerCode() uint16 { return uint16(name >> 21 & 0x7ff) }

// ECUInstance distinguishes ECUs performing the same function.
func (name Name) ECUInstance() uint8 { return uint8(name >> 32 & 7) }

// FunctionInstance distinguishes instances of the same function.
func (name Name) FunctionInstance() uint8 { return uint8(name >> 35 & 31) }

// Function returns the function code, interpreted in its industry/system context.
func (name Name) Function() uint8 { return uint8(name >> 40) }

// Reserved returns the reserved NAME bit.
func (name Name) Reserved() uint8 { return uint8(name >> 48 & 1) }

// VehicleSystem returns the vehicle system code within the industry group.
func (name Name) VehicleSystem() uint8 { return uint8(name >> 49 & 127) }

// VehicleSystemInstance distinguishes vehicle systems of the same kind.
func (name Name) VehicleSystemInstance() uint8 { return uint8(name >> 56 & 15) }

// IndustryGroup returns the three-bit industry group.
func (name Name) IndustryGroup() uint8 { return uint8(name >> 60 & 7) }

// ArbitraryAddressCapable reports whether the controller can select another address.
func (name Name) ArbitraryAddressCapable() bool { return name>>63 != 0 }

type nameKey struct {
	bus  gocan.BusID
	name Name
}

// Names returns the identities observed claiming address on bus, in numeric
// order. Empty means unknown; multiple entries mean unresolved competition.
// These are observed claims, not proof of successful arbitration or liveness.
// A NAME moving address or announcing Cannot Claim removes its old association.
func (decoder *Decoder) Names(bus gocan.BusID, address Address) []Name {
	var names []Name
	for key, claimed := range decoder.names {
		if key.bus == bus && claimed == address {
			names = append(names, key.name)
		}
	}
	slices.Sort(names)
	return names
}

func (decoder *Decoder) observeClaim(event gocan.FrameEvent, header Header) error {
	name, err := ParseName(event.Frame.Data[:event.Frame.DataLength()])
	if err != nil {
		return err
	}
	if header.Destination != GlobalAddress || header.Source == GlobalAddress || name == 0 {
		return fmt.Errorf("%w: invalid address claim source, destination or zero NAME", ErrProtocol)
	}
	if name.Reserved() != 0 {
		return fmt.Errorf("%w: reserved NAME bit is set", ErrUnsupported)
	}
	key := nameKey{event.Bus, name}
	previous, known := decoder.names[key]
	delete(decoder.names, key)
	if header.Source != NullAddress {
		if decoder.names == nil {
			decoder.names = make(map[nameKey]Address)
		}
		decoder.names[key] = header.Source
	}
	competing := len(decoder.Names(event.Bus, header.Source)) > 1
	// Address changes and competing claims make an in-flight payload's source
	// or destination identity uncertain. Do not join bytes across that boundary.
	if competing || known && previous != header.Source {
		for sessionKey := range decoder.sessions {
			if sessionKey.bus == event.Bus && (sessionKey.source == header.Source || sessionKey.destination == header.Source ||
				known && (sessionKey.source == previous || sessionKey.destination == previous)) {
				delete(decoder.sessions, sessionKey)
				err = fmt.Errorf("%w: address claim invalidated incomplete TP", ErrProtocol)
			}
		}
	}
	if competing {
		err = errors.Join(err, fmt.Errorf("%w: address %#02x on bus %d", ErrAmbiguous, header.Source, event.Bus))
	}
	return err
}
