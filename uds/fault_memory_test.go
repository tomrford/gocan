package uds_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/tomrford/gocan/uds"
)

// These are independently authored wire vectors, not encoder round trips.
// Byte order and field widths for 0x19 follow AUTOSAR AP R22-11,
// STS_DIAG_00008 steps 3–8 (pp. 90–91):
// https://www.autosar.org/fileadmin/standards/R22-11/AP/AUTOSAR_TR_AdaptivePlatformSystemTests.pdf
// Status bit values follow AUTOSAR CP R22-11 SWS_Dem_00928; selection follows
// SWS_Dcm_00008 and Dem_SetDTCFilter's documented status-mask rule:
// https://www.autosar.org/fileadmin/standards/R22-11/CP/AUTOSAR_SWS_DiagnosticEventManager.pdf
// https://www.autosar.org/fileadmin/standards/R22-11/CP/AUTOSAR_SWS_DiagnosticCommunicationManager.pdf
func TestFaultMemoryLifecycle(t *testing.T) {
	client, server, ctx := newSemanticPair(t)
	serverResult := make(chan error, 1)
	go func() {
		for _, exchange := range []struct{ request, response []byte }{
			// Count has a nonzero high byte; format 0x9a is deliberately opaque.
			{[]byte{0x19, 0x01, 0x09}, []byte{0x59, 0x01, 0xff, 0x9a, 0x01, 0x23}},
			// Three records force an ISO-TP segmented response. Preserve zero,
			// unknown numbers, order and status bits beyond the request mask.
			{[]byte{0x19, 0x02, 0x09}, []byte{0x59, 0x02, 0xff, 0xab, 0xcd, 0xef, 0xa9, 0x00, 0x00, 0x00, 0x08, 0x12, 0x34, 0x56, 0x01}},
			{[]byte{0x14, 0x12, 0x34, 0x56}, []byte{0x54}},
			{[]byte{0x14, 0xff, 0xff, 0xff}, []byte{0x54}},
			{[]byte{0x19, 0x02, 0x09}, []byte{0x59, 0x02, 0xff}},
			{[]byte{0x19, 0x01, 0x09}, []byte{0x59, 0x01, 0xff, 0x01, 0x00, 0x00}},
			// Zero and unsupported-only masks are valid requests, with no list.
			{[]byte{0x19, 0x02, 0x00}, []byte{0x59, 0x02, 0xff}},
			{[]byte{0x19, 0x02, 0x80}, []byte{0x59, 0x02, 0x7f}},
		} {
			if err := receiveRequest(ctx, server, exchange.request); err != nil {
				serverResult <- err
				return
			}
			if err := server.Send(ctx, exchange.response); err != nil {
				serverResult <- err
				return
			}
		}
		serverResult <- nil
	}()

	mask := uds.DTCStatusTestFailed | uds.DTCStatusConfirmedDTC
	count, err := client.ReadDTCCountByStatusMask(ctx, mask)
	if err != nil || count.Count != 0x123 || count.FormatIdentifier != 0x9a || count.StatusAvailabilityMask != 0xff ||
		!bytes.Equal(count.Raw, []byte{0x01, 0xff, 0x9a, 0x01, 0x23}) {
		t.Fatalf("count = %#v, %v", count, err)
	}
	list, err := client.ReadDTCByStatusMask(ctx, mask)
	if err != nil || list.StatusAvailabilityMask != 0xff || len(list.Records) != 3 ||
		!bytes.Equal(list.Raw, []byte{0x02, 0xff, 0xab, 0xcd, 0xef, 0xa9, 0, 0, 0, 0x08, 0x12, 0x34, 0x56, 0x01}) {
		t.Fatalf("list = %#v, %v", list, err)
	}
	if list.Records[0] != (uds.DTCRecord{Number: 0xabcdef, Status: uds.DTCStatusTestFailed | uds.DTCStatusConfirmedDTC | uds.DTCStatusTestFailedSinceLastClear | uds.DTCStatusWarningIndicatorRequested}) ||
		list.Records[1] != (uds.DTCRecord{Number: 0, Status: uds.DTCStatusConfirmedDTC}) ||
		list.Records[2] != (uds.DTCRecord{Number: 0x123456, Status: uds.DTCStatusTestFailed}) {
		t.Fatalf("records = %#v", list.Records)
	}
	for _, group := range []uint32{0x123456, uds.AllDTCs} {
		if err := client.ClearDiagnosticInformation(ctx, group); err != nil {
			t.Fatal(err)
		}
	}
	cleared, err := client.ReadDTCByStatusMask(ctx, mask)
	if err != nil || len(cleared.Records) != 0 {
		t.Fatalf("cleared list = %#v, %v", cleared, err)
	}
	count, err = client.ReadDTCCountByStatusMask(ctx, mask)
	if err != nil || count.Count != 0 {
		t.Fatalf("cleared count = %#v, %v", count, err)
	}
	for _, emptyMask := range []uds.DTCStatus{0, uds.DTCStatusWarningIndicatorRequested} {
		list, err := client.ReadDTCByStatusMask(ctx, emptyMask)
		if err != nil || len(list.Records) != 0 {
			t.Fatalf("empty list = %#v, %v", list, err)
		}
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

// SWS_Dcm_00588 permits complete zero-filled trailing records when faults
// disappear during paged transmission. These wire vectors exercise the suffix
// independently of the codec, including a real zero-number DTC before padding.
func TestFaultMemoryPagedResponses(t *testing.T) {
	for _, test := range []struct {
		name     string
		response []byte
		want     []uds.DTCRecord
	}{
		{"reported regression", []byte{0x59, 0x02, 0xff, 0x12, 0x34, 0x56, 0x01, 0, 0, 0, 0}, []uds.DTCRecord{{Number: 0x123456, Status: 0x01}}},
		{"multiple padding records after zero-number fault", []byte{0x59, 0x02, 0xff, 0, 0, 0, 0x08, 0, 0, 0, 0, 0, 0, 0, 0}, []uds.DTCRecord{{Number: 0, Status: 0x08}}},
		{"only padding remains", []byte{0x59, 0x02, 0xff, 0, 0, 0, 0}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, server, ctx := newSemanticPair(t)
			done := make(chan error, 1)
			go func() {
				if err := receiveRequest(ctx, server, []byte{0x19, 0x02, 0x09}); err != nil {
					done <- err
					return
				}
				done <- server.Send(ctx, test.response)
			}()
			result, err := client.ReadDTCByStatusMask(ctx, 0x09)
			if err != nil || result.StatusAvailabilityMask != 0xff || !slices.Equal(result.Records, test.want) || !bytes.Equal(result.Raw, test.response[1:]) {
				t.Fatalf("result = %#v, %v; want records %#v and raw %x", result, err, test.want, test.response[1:])
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFaultMemoryRejectsMalformedResponses(t *testing.T) {
	for _, test := range []struct {
		name string
		sub  byte
		data []byte
		kind error
	}{
		{"count empty", 1, nil, uds.ErrInvalidResponse},
		{"count truncated", 1, []byte{1, 0xff, 1, 0}, uds.ErrInvalidResponse},
		{"count extra byte", 1, []byte{1, 0xff, 1, 0, 1, 0}, uds.ErrInvalidResponse},
		{"count wrong echo", 1, []byte{7, 0xff, 1, 0, 1}, uds.ErrUnexpectedResponse},
		{"list response to count request", 1, []byte{2, 0xff}, uds.ErrUnexpectedResponse},
		{"list empty", 2, nil, uds.ErrInvalidResponse},
		{"list missing mask", 2, []byte{2}, uds.ErrInvalidResponse},
		{"list one-byte fragment", 2, []byte{2, 0xff, 0x12}, uds.ErrInvalidResponse},
		{"list two-byte fragment", 2, []byte{2, 0xff, 0x12, 0x34}, uds.ErrInvalidResponse},
		{"list missing status", 2, []byte{2, 0xff, 0x12, 0x34, 0x56}, uds.ErrInvalidResponse},
		{"list trailing zero", 2, []byte{2, 0xff, 0x12, 0x34, 0x56, 1, 0}, uds.ErrInvalidResponse},
		{"list wrong echo", 2, []byte{0x0a, 0xff}, uds.ErrUnexpectedResponse},
		{"count response to list request", 2, []byte{1, 0xff, 1, 0, 1}, uds.ErrUnexpectedResponse},
		{"list does not match mask", 2, []byte{2, 0xff, 0x12, 0x34, 0x56, 0x04}, uds.ErrInvalidResponse},
		{"list only unavailable match", 2, []byte{2, 0xfe, 0x12, 0x34, 0x56, 1}, uds.ErrInvalidResponse},
		{"list interior zero record", 2, []byte{2, 0xff, 0, 0, 0, 0, 0x12, 0x34, 0x56, 1}, uds.ErrInvalidResponse},
		{"list two-byte zero suffix", 2, []byte{2, 0xff, 0x12, 0x34, 0x56, 1, 0, 0}, uds.ErrInvalidResponse},
		{"list three-byte zero suffix", 2, []byte{2, 0xff, 0x12, 0x34, 0x56, 1, 0, 0, 0}, uds.ErrInvalidResponse},
		{"list invalid status before padding", 2, []byte{2, 0xff, 0x12, 0x34, 0x56, 0x04, 0, 0, 0, 0}, uds.ErrInvalidResponse},
		{"bad second record preserves all raw data", 2, []byte{2, 0xff, 0x12, 0x34, 0x56, 1, 0xab, 0xcd, 0xef, 0}, uds.ErrInvalidResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, server, ctx := newSemanticPair(t)
			done := make(chan error, 1)
			go func() {
				if err := receiveRequest(ctx, server, []byte{0x19, test.sub, 0x09}); err != nil {
					done <- err
					return
				}
				done <- server.Send(ctx, append([]byte{0x59}, test.data...))
			}()
			var raw []byte
			var err error
			if test.sub == 1 {
				var result uds.DTCCountResponse
				result, err = client.ReadDTCCountByStatusMask(ctx, 0x09)
				raw = result.Raw
				if result.Count != 0 || result.FormatIdentifier != 0 || result.StatusAvailabilityMask != 0 {
					t.Fatalf("failed decode returned fields: %#v", result)
				}
			} else {
				var result uds.DTCListResponse
				result, err = client.ReadDTCByStatusMask(ctx, 0x09)
				raw = result.Raw
				if result.Records != nil || result.StatusAvailabilityMask != 0 {
					t.Fatalf("failed decode returned fields: %#v", result)
				}
			}
			if !errors.Is(err, test.kind) || !bytes.Equal(raw, test.data) {
				t.Fatalf("raw = %x, error = %v; want %x, %v", raw, err, test.data, test.kind)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFaultMemoryClearValidationAndServerRejection(t *testing.T) {
	// A nil client proves invalid requests are rejected before transport access.
	var absent *uds.Client
	if err := absent.ClearDiagnosticInformation(context.Background(), 0x1000000); !errors.Is(err, uds.ErrInvalidRequest) {
		t.Fatalf("oversized group = %v", err)
	}
	for _, test := range []struct {
		name              string
		request, response []byte
		call              func(context.Context, *uds.Client) error
		kind              error
		negative          uds.ResponseCode
	}{
		{"extra clear byte", []byte{0x14, 0, 0, 0}, []byte{0x54, 0}, func(ctx context.Context, c *uds.Client) error { return c.ClearDiagnosticInformation(ctx, 0) }, uds.ErrInvalidResponse, 0},
		{"unsupported count", []byte{0x19, 1, 0xff}, []byte{0x7f, 0x19, 0x12}, func(ctx context.Context, c *uds.Client) error {
			_, err := c.ReadDTCCountByStatusMask(ctx, 0xff)
			return err
		}, nil, 0x12},
		{"clear denied", []byte{0x14, 0xff, 0xff, 0xff}, []byte{0x7f, 0x14, 0x33}, func(ctx context.Context, c *uds.Client) error { return c.ClearDiagnosticInformation(ctx, uds.AllDTCs) }, nil, 0x33},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, server, ctx := newSemanticPair(t)
			done := make(chan error, 1)
			go func() {
				if err := receiveRequest(ctx, server, test.request); err != nil {
					done <- err
					return
				}
				done <- server.Send(ctx, test.response)
			}()
			err := test.call(ctx, client)
			if test.negative != 0 {
				var negative *uds.NegativeResponseError
				if !errors.As(err, &negative) || negative.Code != test.negative || negative.Service != uds.ServiceID(test.request[0]) {
					t.Fatalf("negative response = %v", err)
				}
			} else if !errors.Is(err, test.kind) {
				t.Fatalf("error = %v, want %v", err, test.kind)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
