package j1939_test

import (
	"testing"

	"github.com/tomrford/gocan/j1939"
)

func TestHeaderIdentifierRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		id   uint32
		want j1939.Header
		edp  uint8
	}{
		{
			name: "PDU1",
			id:   0x18ea2a80,
			want: j1939.Header{Priority: 6, PGN: 0xea00, Source: 0x80, Destination: 0x2a},
		},
		{
			name: "PDU2",
			id:   0x18feee80,
			want: j1939.Header{Priority: 6, PGN: 0xfeee, Source: 0x80, Destination: j1939.GlobalAddress},
		},
		{
			name: "both data pages",
			id:   0x0ff00401,
			want: j1939.Header{Priority: 3, PGN: 0x3f004, Source: 1, Destination: j1939.GlobalAddress},
			edp:  1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header, err := j1939.ParseID(test.id)
			if err != nil {
				t.Fatalf("ParseID: %v", err)
			}
			if header != test.want {
				t.Fatalf("ParseID(%#x) = %#v, want %#v", test.id, header, test.want)
			}
			if header.EDP() != test.edp {
				t.Fatalf("identifier fields lost for %#x", test.id)
			}
			id, err := header.ID()
			if err != nil {
				t.Fatalf("ID: %v", err)
			}
			if id != test.id {
				t.Fatalf("ID = %#x, want %#x", id, test.id)
			}
		})
	}
}

func TestHeaderRejectsInvalidFields(t *testing.T) {
	tests := []j1939.Header{
		{Priority: 8, PGN: 0xfeee, Destination: j1939.GlobalAddress},
		{Priority: 6, PGN: j1939.MaxPGN + 1, Destination: j1939.GlobalAddress},
		{Priority: 6, PGN: 0xea2a, Destination: 0x2a},
		{Priority: 6, PGN: 0xfeee, Destination: 0x2a},
	}
	for _, header := range tests {
		if _, err := header.ID(); err == nil {
			t.Fatalf("Header.ID(%#v) succeeded", header)
		}
	}
}
