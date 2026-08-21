package j1939

import "testing"

func TestHeaderIdentifierRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		id   uint32
		want Header
	}{
		{
			name: "PDU1",
			id:   0x18ea2a80,
			want: Header{Priority: 6, PGN: 0xea00, Source: 0x80, Destination: 0x2a},
		},
		{
			name: "PDU2",
			id:   0x18feee80,
			want: Header{Priority: 6, PGN: 0xfeee, Source: 0x80, Destination: GlobalAddress},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header, err := ParseID(test.id)
			if err != nil {
				t.Fatalf("ParseID: %v", err)
			}
			if header != test.want {
				t.Fatalf("ParseID(%#x) = %#v, want %#v", test.id, header, test.want)
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
	tests := []Header{
		{Priority: 8, PGN: 0xfeee, Destination: GlobalAddress},
		{Priority: 6, PGN: MaxPGN + 1, Destination: GlobalAddress},
		{Priority: 6, PGN: 0xea2a, Destination: 0x2a},
		{Priority: 6, PGN: 0xfeee, Destination: 0x2a},
	}
	for _, header := range tests {
		if _, err := header.ID(); err == nil {
			t.Fatalf("Header.ID(%#v) succeeded", header)
		}
	}
}
