package cdd_test

import (
	"bytes"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tomrford/gocan/cdd"
)

func TestDIDCodecLifecycle(t *testing.T) {
	database, err := cdd.ParseFile(filepath.Join("testdata", "records.cdd"))
	if err != nil {
		t.Fatal(err)
	}

	thermal, ok := database.DIDByName("ThermalStatus")
	if !ok {
		t.Fatal("ThermalStatus DID was not resolved")
	}
	payload, err := thermal.Read.Encode(cdd.Values{
		"Heater":  "On",
		"Coolant": 25.0,
		"Cycles":  uint8(7),
	})
	if err != nil {
		t.Fatalf("Encode ThermalStatus: %v", err)
	}
	if want := []byte{0x01, 0x00, 0x82, 0x00, 0x07}; !bytes.Equal(payload, want) {
		t.Fatalf("thermal payload = %x, want %x", payload, want)
	}
	values, err := thermal.Read.Decode(payload)
	if err != nil {
		t.Fatalf("Decode ThermalStatus: %v", err)
	}
	if values["Heater"] != uint64(1) || values["Coolant"] != 25.0 || values["Cycles"] != uint64(7) {
		t.Fatalf("thermal values = %#v", values)
	}

	nameplate, ok := database.DIDByName("Nameplate")
	if !ok {
		t.Fatal("Nameplate DID was not resolved")
	}
	nameplateValues := cdd.Values{
		"SerialNumber": "SN1234567890",
		"Coefficients": []uint8{1, 2, 3, 4},
		"Gain":         0.25,
		"ExactCounter": uint64(1<<53) + 1,
	}
	payload, err = nameplate.Read.Encode(nameplateValues)
	if err != nil {
		t.Fatalf("Encode Nameplate: %v", err)
	}
	wantNameplate := append([]byte("SN1234567890"),
		1, 2, 3, 4,
		0x3e, 0x80, 0x00, 0x00,
		0x00, 0x20, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
	)
	if !bytes.Equal(payload, wantNameplate) {
		t.Fatalf("nameplate payload = %x, want %x", payload, wantNameplate)
	}
	values, err = nameplate.Read.Decode(payload)
	if err != nil {
		t.Fatalf("Decode Nameplate: %v", err)
	}
	if values["SerialNumber"] != "SN1234567890" ||
		!reflect.DeepEqual(values["Coefficients"], []uint64{1, 2, 3, 4}) ||
		values["Gain"] != 0.25 || values["ExactCounter"] != uint64(1<<53)+1 {
		t.Fatalf("nameplate values = %#v", values)
	}

	buffer, ok := database.DIDByName("UploadBuffer")
	if !ok {
		t.Fatal("UploadBuffer DID was not resolved")
	}
	payload, err = buffer.Read.Encode(cdd.Values{
		"BlockNumber": uint16(0x1234),
		"Buffer":      []uint8{0xaa, 0xbb, 0xcc},
	})
	if err != nil {
		t.Fatalf("Encode UploadBuffer: %v", err)
	}
	if want := []byte{0x12, 0x34, 0xaa, 0xbb, 0xcc}; !bytes.Equal(payload, want) {
		t.Fatalf("buffer payload = %x, want %x", payload, want)
	}
	values, err = buffer.Read.Decode(payload)
	if err != nil {
		t.Fatalf("Decode UploadBuffer: %v", err)
	}
	if values["BlockNumber"] != uint64(0x1234) || !reflect.DeepEqual(values["Buffer"], []uint64{0xaa, 0xbb, 0xcc}) {
		t.Fatalf("buffer values = %#v", values)
	}

	writable, ok := database.DIDByName("WritableSettings")
	if !ok || writable.Read != nil || writable.Write == nil {
		t.Fatalf("unexpected writable DID: %#v", writable)
	}
	payload, err = writable.Write.Encode(cdd.Values{"Setting": uint8(0x2a)})
	if err != nil || !bytes.Equal(payload, []byte{0x2a}) {
		t.Fatalf("encode writable record = %x, %v", payload, err)
	}

	asymmetric, ok := database.DIDByName("AsymmetricReadWrite")
	if !ok || asymmetric.Read == nil || asymmetric.Write == nil {
		t.Fatalf("unexpected asymmetric DID: %#v", asymmetric)
	}
	values, err = asymmetric.Read.Decode([]byte{0x07})
	if err != nil || values["Counter"] != uint64(7) {
		t.Fatalf("decode asymmetric read record = %#v, %v", values, err)
	}
	payload, err = asymmetric.Write.Encode(cdd.Values{"Counter": []uint8{1, 2, 3, 4}})
	if err != nil || !bytes.Equal(payload, []byte{1, 2, 3, 4}) {
		t.Fatalf("encode asymmetric write record = %x, %v", payload, err)
	}
}
