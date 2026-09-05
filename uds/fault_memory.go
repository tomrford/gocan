package uds

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrInvalidRequest identifies invalid parameters supplied to a typed
// fault-memory operation, before any request is sent.
var ErrInvalidRequest = errors.New("invalid UDS request")

// DTCStatus is the UDS status byte, or a mask selecting its bits. A status-mask
// request selects DTCs with at least one requested, supported status bit set.
// All eight bits are defined; zero is a valid request mask.
type DTCStatus uint8

const (
	DTCStatusTestFailed                         DTCStatus = 0x01
	DTCStatusTestFailedThisOperationCycle       DTCStatus = 0x02
	DTCStatusPendingDTC                         DTCStatus = 0x04
	DTCStatusConfirmedDTC                       DTCStatus = 0x08
	DTCStatusTestNotCompletedSinceLastClear     DTCStatus = 0x10
	DTCStatusTestFailedSinceLastClear           DTCStatus = 0x20
	DTCStatusTestNotCompletedThisOperationCycle DTCStatus = 0x40
	DTCStatusWarningIndicatorRequested          DTCStatus = 0x80
)

// AllDTCs selects all DTC groups for ClearDiagnosticInformation. It is never
// selected implicitly: callers must supply the group to clear.
const AllDTCs uint32 = 0xffffff

// DTCRecord is one three-byte DTC number and its unmodified status byte.
// Number is the wire number, without translation into an OBD or vendor label.
type DTCRecord struct {
	Number uint32
	Status DTCStatus
}

// DTCCountResponse describes a reportNumberOfDTCByStatusMask response.
type DTCCountResponse struct {
	StatusAvailabilityMask DTCStatus
	// FormatIdentifier is retained even when its meaning is unknown to gocan.
	FormatIdentifier uint8
	Count            uint16
	// Raw contains all bytes after the positive response SID, including the
	// subfunction echo. On a service decoding error only Raw is populated.
	Raw []byte
}

// DTCListResponse describes a reportDTCByStatusMask response. This subfunction
// does not report a DTC format identifier. Records retain response order and
// unknown numbers; callers associate them with an appropriate catalog.
type DTCListResponse struct {
	StatusAvailabilityMask DTCStatus
	Records                []DTCRecord
	// Raw has the same convention as DTCCountResponse.Raw.
	Raw []byte
}

// ReadDTCCountByStatusMask sends ReadDTCInformation subfunction 0x01 for
// primary memory. It implements the ISO 14229-1:2013 form documented by
// AUTOSAR CP R22-11, SWS Dcm section 7.6.2.5.1.
func (client *Client) ReadDTCCountByStatusMask(ctx context.Context, mask DTCStatus) (DTCCountResponse, error) {
	data, err := client.do(ctx, ServiceReadDTCInformation, []byte{0x01, byte(mask)})
	result := DTCCountResponse{Raw: data}
	if err != nil {
		return result, err
	}
	if len(data) > 0 && data[0] != 0x01 {
		return result, unexpectedServiceResponse(ServiceReadDTCInformation, "subfunction echo is %#02x, want 0x01", data[0])
	}
	if len(data) != 5 {
		return result, invalidServiceResponse(ServiceReadDTCInformation, "count response has %d data bytes, want 5", len(data))
	}
	result.StatusAvailabilityMask = DTCStatus(data[1])
	result.FormatIdentifier = data[2]
	result.Count = binary.BigEndian.Uint16(data[3:5])
	return result, nil
}

// ReadDTCByStatusMask sends ReadDTCInformation subfunction 0x02 for primary
// memory (ISO 14229-1:2013; AUTOSAR CP R22-11, SWS Dcm section 7.6.2.5.2).
// It validates the echo, complete four-byte records and status selection.
// It does not discard zero DTC numbers or strip apparent payload padding.
// Other ReadDTCInformation subfunctions require different layouts and remain
// available through Do; this method does not attempt to decode them.
func (client *Client) ReadDTCByStatusMask(ctx context.Context, mask DTCStatus) (DTCListResponse, error) {
	data, err := client.do(ctx, ServiceReadDTCInformation, []byte{0x02, byte(mask)})
	result := DTCListResponse{Raw: data}
	if err != nil {
		return result, err
	}
	if len(data) > 0 && data[0] != 0x02 {
		return result, unexpectedServiceResponse(ServiceReadDTCInformation, "subfunction echo is %#02x, want 0x02", data[0])
	}
	if len(data) < 2 || (len(data)-2)%4 != 0 {
		return result, invalidServiceResponse(ServiceReadDTCInformation, "list response has %d data bytes, want 2 + 4*n", len(data))
	}
	availability := DTCStatus(data[1])
	var records []DTCRecord
	for offset := 2; offset < len(data); offset += 4 {
		status := DTCStatus(data[offset+3])
		if status&mask&availability == 0 {
			return result, invalidServiceResponse(ServiceReadDTCInformation, "DTC record %d status %#02x does not match requested supported mask %#02x", (offset-2)/4+1, status, mask&availability)
		}
		records = append(records, DTCRecord{
			Number: uint32(data[offset])<<16 | uint32(data[offset+1])<<8 | uint32(data[offset+2]),
			Status: status,
		})
	}
	result.StatusAvailabilityMask = availability
	result.Records = records
	return result, nil
}

// ClearDiagnosticInformation requests clearance of a three-byte groupOfDTC in
// primary memory and requires an empty positive response parameter record.
// This is the ISO 14229-1:2013 form (AUTOSAR CP R22-11, SWS Dcm 7.6.2.4).
// The server determines which groups it supports. Memory selection extensions
// are outside this method's contract; callers can use Do for other forms.
func (client *Client) ClearDiagnosticInformation(ctx context.Context, group uint32) error {
	if group > 0xffffff {
		return fmt.Errorf("%w: groupOfDTC %#x exceeds 24 bits", ErrInvalidRequest, group)
	}
	data, err := client.do(ctx, ServiceClearDiagnosticInformation, []byte{byte(group >> 16), byte(group >> 8), byte(group)})
	if err != nil {
		return err
	}
	if len(data) != 0 {
		return invalidServiceResponse(ServiceClearDiagnosticInformation, "has unexpected response data %x, want no data", data)
	}
	return nil
}
