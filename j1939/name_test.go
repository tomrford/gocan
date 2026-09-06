package j1939_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/j1939"
)

func TestObservedNameLifecycle(t *testing.T) {
	// Independent NAME/claim fixture: python-can-j1939 01aba95cca43847bc272ec7ddb12048f587e6554,
	// test/test_ca.py, test_addr_claim_fixed and test_addr_claim_fixed_veto_lose.
	payload := []byte{135, 214, 82, 83, 130, 201, 254, 82}
	name, err := j1939.ParseName(payload)
	if err != nil {
		t.Fatal(err)
	}
	if name != 0x52fec9825352d687 || name.IdentityNumber() != 1234567 || name.ManufacturerCode() != 666 ||
		name.ECUInstance() != 2 || name.FunctionInstance() != 16 || name.Function() != 201 || name.Reserved() != 0 ||
		name.VehicleSystem() != 127 || name.VehicleSystemInstance() != 2 || name.IndustryGroup() != 5 || name.ArbitraryAddressCapable() {
		t.Fatalf("incorrect NAME fields: %#x", name)
	}
	var decoder j1939.Decoder
	claim := frameEvent(t, 0, 0x18eeff80, payload)
	if _, complete, err := decoder.Push(claim); err != nil || !complete {
		t.Fatalf("claim: %v, %v", complete, err)
	}
	if got := decoder.Names(1, 0x80); !reflect.DeepEqual(got, []j1939.Name{name}) {
		t.Fatalf("names=%v", got)
	}
	otherBus := claim
	otherBus.Bus = 2
	decoder.Push(otherBus)
	// Two claims for the same address stay visible until the losing NAME moves
	// or announces Cannot Claim. Observing a lower NAME alone is not proof.
	challenger := frameEvent(t, 1, 0x18eeff80, []byte{135, 214, 82, 83, 130, 111, 254, 82})
	messages, diagnostics := decoder.PushBatch([]gocan.FrameEvent{challenger})
	if len(messages) != 1 || len(diagnostics) != 1 || !errors.Is(diagnostics[0], j1939.ErrAmbiguous) {
		t.Fatalf("competition: %v, %v", messages, diagnostics)
	}
	if got := decoder.Names(1, 0x80); !reflect.DeepEqual(got, []j1939.Name{0x52fe6f825352d687, name}) {
		t.Fatalf("competing names=%v", got)
	}
	claim.Frame.ID = 0x18eefffe
	claim.Timestamp = challenger.Timestamp
	if _, _, err := decoder.Push(claim); err != nil {
		t.Fatal(err)
	}
	if got := decoder.Names(1, 0x80); !reflect.DeepEqual(got, []j1939.Name{0x52fe6f825352d687}) {
		t.Fatalf("winner=%v", got)
	}
	if got := decoder.Names(2, 0x80); !reflect.DeepEqual(got, []j1939.Name{name}) {
		t.Fatalf("other bus=%v", got)
	}
	// Moving the known sender invalidates its partial payload, while a transfer
	// from an unrelated sender on the same bus remains usable.
	for _, source := range []uint32{0x80, 0x82} {
		_, diagnostics := decoder.PushBatch([]gocan.FrameEvent{
			frameEvent(t, 2, 0x18ecff00|source, []byte{32, 9, 0, 2, 255, 202, 254, 0}),
			frameEvent(t, 2, 0x18ebff00|source, []byte{1, 1, 2, 3, 4, 5, 6, 7}),
		})
		if len(diagnostics) != 0 {
			t.Fatal(diagnostics)
		}
	}
	challenger.Frame.ID = 0x18eeff81
	challenger.Timestamp = challenger.Timestamp.Add(2 * time.Millisecond)
	if _, complete, err := decoder.Push(challenger); !complete || !errors.Is(err, j1939.ErrProtocol) {
		t.Fatalf("moving an active sender: complete=%v, err=%v", complete, err)
	}
	if len(decoder.Names(1, 0x80)) != 0 || len(decoder.Names(1, 0x81)) != 1 {
		t.Fatal("NAME move retained old address")
	}
	last := frameEvent(t, 4, 0x18ebff80, []byte{2, 8, 9, 255, 255, 255, 255, 255})
	if _, complete, err := decoder.Push(last); complete || !errors.Is(err, j1939.ErrProtocol) {
		t.Fatalf("completed payload across NAME move: %v, %v", complete, err)
	}
	last.Frame.ID = 0x18ebff82
	if message, complete, err := decoder.Push(last); !complete || err != nil || !reflect.DeepEqual(message.Payload, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}) {
		t.Fatalf("unrelated transfer lost: %v, %v, %v", message, complete, err)
	}
	malformed := challenger
	malformed.Frame.DLC = 7
	if _, _, err := decoder.Push(malformed); !errors.Is(err, j1939.ErrProtocol) {
		t.Fatalf("short NAME: %v", err)
	}
	decoder.Reset()
	if len(decoder.Names(1, 0x81)) != 0 || len(decoder.Names(2, 0x80)) != 0 {
		t.Fatal("reset retained identities")
	}
}
