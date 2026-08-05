package uds

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"
)

const suppressPositiveResponse byte = 0x80

const (
	ServiceDiagnosticSessionControl ServiceID = 0x10
	ServiceECUReset                 ServiceID = 0x11
	ServiceReadDataByIdentifier     ServiceID = 0x22
	ServiceSecurityAccess           ServiceID = 0x27
	ServiceWriteDataByIdentifier    ServiceID = 0x2e
	ServiceRoutineControl           ServiceID = 0x31
	ServiceRequestDownload          ServiceID = 0x34
	ServiceTransferData             ServiceID = 0x36
	ServiceRequestTransferExit      ServiceID = 0x37
	ServiceTesterPresent            ServiceID = 0x3e
)

// Session identifies a diagnostic session-control subfunction.
type Session uint8

const (
	SessionDefault     Session = 0x01
	SessionProgramming Session = 0x02
	SessionExtended    Session = 0x03
	SessionSafety      Session = 0x04
)

// SessionTiming is the server timing reported after a session change.
type SessionTiming struct {
	P2ServerMax     time.Duration
	P2StarServerMax time.Duration
}

// SessionControlResponse reports the acknowledged session. Timing is nil when
// the server uses the legacy response without timing parameters.
type SessionControlResponse struct {
	Session Session
	Timing  *SessionTiming
}

// ApplySessionTiming uses timing for subsequent exchanges. Callers can clamp
// values or add transport margin before applying server timing.
func (client *Client) ApplySessionTiming(timing SessionTiming) error {
	if timing.P2ServerMax <= 0 {
		return fmt.Errorf("UDS P2 server maximum must be positive")
	}
	if timing.P2StarServerMax <= 0 {
		return fmt.Errorf("UDS P2* server maximum must be positive")
	}
	client.timingMu.Lock()
	client.p2Timeout = timing.P2ServerMax
	client.p2StarTimeout = timing.P2StarServerMax
	client.timingMu.Unlock()
	return nil
}

// ResetType identifies an ECU-reset subfunction.
type ResetType uint8

const (
	ResetHard              ResetType = 0x01
	ResetKeyOffOn          ResetType = 0x02
	ResetSoft              ResetType = 0x03
	ResetEnableRapidPower  ResetType = 0x04
	ResetDisableRapidPower ResetType = 0x05
)

// SecurityLevel identifies an odd request-seed subfunction. SendKey uses the
// following even subfunction.
type SecurityLevel uint8

// RoutineControlType identifies a routine-control subfunction.
type RoutineControlType uint8

const (
	RoutineStart          RoutineControlType = 0x01
	RoutineStop           RoutineControlType = 0x02
	RoutineRequestResults RoutineControlType = 0x03
)

// DataFormatIdentifier describes the compression and encryption applied to a
// download. Zero means neither compression nor encryption.
type DataFormatIdentifier uint8

// MemoryLocation describes the address and size encoded by RequestDownload.
// AddressLength and SizeLength are explicit byte widths from one through eight.
type MemoryLocation struct {
	Address       uint64
	Size          uint64
	AddressLength uint8
	SizeLength    uint8
}

// DiagnosticSessionControl requests session and returns its response data. It
// does not change the Client's configured P2 or P2* timeouts.
func (client *Client) DiagnosticSessionControl(ctx context.Context, session Session) (SessionControlResponse, error) {
	if err := validateSubfunction(byte(session), "diagnostic session"); err != nil {
		return SessionControlResponse{}, err
	}
	data, err := client.do(ctx, ServiceDiagnosticSessionControl, []byte{byte(session)})
	if err != nil {
		return SessionControlResponse{}, err
	}
	if len(data) != 1 && len(data) != 5 {
		return SessionControlResponse{}, invalidServiceResponse(ServiceDiagnosticSessionControl, "has %d data bytes, want 1 or 5", len(data))
	}
	if data[0] != byte(session) {
		return SessionControlResponse{}, unexpectedServiceResponse(
			ServiceDiagnosticSessionControl,
			"session echo is %#02x, want %#02x",
			data[0], session,
		)
	}
	result := SessionControlResponse{Session: session}
	if len(data) == 5 {
		result.Timing = &SessionTiming{
			P2ServerMax:     time.Duration(binary.BigEndian.Uint16(data[1:3])) * time.Millisecond,
			P2StarServerMax: time.Duration(binary.BigEndian.Uint16(data[3:5])) * 10 * time.Millisecond,
		}
	}
	return result, nil
}

// ECUReset requests resetType and returns its optional response parameter record.
func (client *Client) ECUReset(ctx context.Context, resetType ResetType) ([]byte, error) {
	if err := validateSubfunction(byte(resetType), "ECU reset type"); err != nil {
		return nil, err
	}
	return client.doEchoed(ctx, ServiceECUReset, []byte{byte(resetType)}, []byte{byte(resetType)}, "reset type", false)
}

// ReadDataByIdentifier reads one data identifier and returns its data record.
func (client *Client) ReadDataByIdentifier(ctx context.Context, identifier uint16) ([]byte, error) {
	request := make([]byte, 2)
	binary.BigEndian.PutUint16(request, identifier)
	return client.doEchoed(ctx, ServiceReadDataByIdentifier, request, request, "data identifier", false)
}

// WriteDataByIdentifier writes one data record and validates the identifier echo.
func (client *Client) WriteDataByIdentifier(ctx context.Context, identifier uint16, record []byte) error {
	request := make([]byte, 2+len(record))
	binary.BigEndian.PutUint16(request, identifier)
	copy(request[2:], record)
	_, err := client.doEchoed(ctx, ServiceWriteDataByIdentifier, request, request[:2], "data identifier", true)
	return err
}

// RequestSeed requests a seed for level and returns the seed record.
func (client *Client) RequestSeed(ctx context.Context, level SecurityLevel, record []byte) ([]byte, error) {
	if err := validateSecurityLevel(level); err != nil {
		return nil, err
	}
	request := append([]byte{byte(level)}, record...)
	return client.doEchoed(ctx, ServiceSecurityAccess, request, request[:1], "security level", false)
}

// SendKey sends a key for level and validates its exact response.
func (client *Client) SendKey(ctx context.Context, level SecurityLevel, key []byte) error {
	if err := validateSecurityLevel(level); err != nil {
		return err
	}
	subfunction := byte(level) + 1
	request := append([]byte{subfunction}, key...)
	_, err := client.doEchoed(ctx, ServiceSecurityAccess, request, request[:1], "security level", true)
	return err
}

// RoutineControl invokes one routine operation and returns its status record.
func (client *Client) RoutineControl(ctx context.Context, controlType RoutineControlType, identifier uint16, optionRecord []byte) ([]byte, error) {
	if err := validateSubfunction(byte(controlType), "routine control type"); err != nil {
		return nil, err
	}
	request := make([]byte, 3+len(optionRecord))
	request[0] = byte(controlType)
	binary.BigEndian.PutUint16(request[1:3], identifier)
	copy(request[3:], optionRecord)
	return client.doEchoed(ctx, ServiceRoutineControl, request, request[:3], "routine", false)
}

// RequestDownload begins a download and returns the largest complete
// TransferData request length accepted by the server.
func (client *Client) RequestDownload(ctx context.Context, dataFormat DataFormatIdentifier, location MemoryLocation) (uint64, error) {
	if err := validateMemoryLocation(location); err != nil {
		return 0, err
	}
	request := make([]byte, 2+int(location.AddressLength)+int(location.SizeLength))
	request[0] = byte(dataFormat)
	request[1] = location.SizeLength<<4 | location.AddressLength
	putBigEndianWidth(request[2:2+location.AddressLength], location.Address)
	putBigEndianWidth(request[2+location.AddressLength:], location.Size)

	data, err := client.do(ctx, ServiceRequestDownload, request)
	if err != nil {
		return 0, err
	}
	if len(data) < 1 {
		return 0, invalidServiceResponse(ServiceRequestDownload, "has no length-format identifier")
	}
	if data[0]&0x0f != 0 {
		return 0, invalidServiceResponse(ServiceRequestDownload, "length-format identifier low nibble is %#x, want 0", data[0]&0x0f)
	}
	width := int(data[0] >> 4)
	if width < 1 || width > 8 {
		return 0, invalidServiceResponse(ServiceRequestDownload, "maximum block-length width is %d, want 1 through 8", width)
	}
	if len(data) != width+1 {
		return 0, invalidServiceResponse(ServiceRequestDownload, "has %d data bytes for a %d-byte maximum block length", len(data), width)
	}
	maximum := readBigEndianWidth(data[1:])
	if maximum < 2 {
		return 0, invalidServiceResponse(ServiceRequestDownload, "maximum block length %d cannot contain service and sequence bytes", maximum)
	}
	return maximum, nil
}

// TransferData transfers one block and returns its response parameter record.
func (client *Client) TransferData(ctx context.Context, sequence uint8, block []byte) ([]byte, error) {
	request := append([]byte{sequence}, block...)
	return client.doEchoed(ctx, ServiceTransferData, request, request[:1], "block sequence counter", false)
}

// RequestTransferExit ends a transfer and returns its response parameter record.
func (client *Client) RequestTransferExit(ctx context.Context, record []byte) ([]byte, error) {
	return client.do(ctx, ServiceRequestTransferExit, record)
}

// TesterPresent sends a response-waiting tester-present request.
func (client *Client) TesterPresent(ctx context.Context) error {
	_, err := client.doEchoed(ctx, ServiceTesterPresent, []byte{0}, []byte{0}, "subfunction", true)
	return err
}

// SendTesterPresent sends a tester-present request with its positive response
// suppressed and does not wait for a response.
func (client *Client) SendTesterPresent(ctx context.Context) error {
	return client.Send(ctx, Request{Service: ServiceTesterPresent, Data: []byte{suppressPositiveResponse}})
}

func (client *Client) do(ctx context.Context, service ServiceID, data []byte) ([]byte, error) {
	response, err := client.Do(ctx, Request{Service: service, Data: data})
	if err != nil {
		return nil, err
	}
	return response.Data, nil
}

func (client *Client) doEchoed(
	ctx context.Context,
	service ServiceID,
	request, echo []byte,
	name string,
	exact bool,
) ([]byte, error) {
	data, err := client.do(ctx, service, request)
	if err != nil {
		return nil, err
	}
	if len(data) < len(echo) || exact && len(data) != len(echo) {
		comparison := "at least"
		if exact {
			comparison = "exactly"
		}
		return nil, invalidServiceResponse(service, "has %d data bytes, want %s %d for %s echo", len(data), comparison, len(echo), name)
	}
	for index, want := range echo {
		if data[index] != want {
			return nil, unexpectedServiceResponse(service, "%s echo byte %d is %#02x, want %#02x", name, index, data[index], want)
		}
	}
	return data[len(echo):], nil
}

func validateSubfunction(value byte, name string) error {
	if value&suppressPositiveResponse != 0 {
		return fmt.Errorf("UDS %s %#02x sets suppressPositiveResponse", name, value)
	}
	if value == 0 {
		return fmt.Errorf("UDS %s must not be zero", name)
	}
	if value == 0x7f {
		return fmt.Errorf("UDS %s %#02x is reserved", name, value)
	}
	return nil
}

func validateSecurityLevel(level SecurityLevel) error {
	if err := validateSubfunction(byte(level), "security level"); err != nil {
		return err
	}
	if level&1 == 0 {
		return fmt.Errorf("UDS security level %#02x must be an odd request-seed subfunction", level)
	}
	return nil
}

func validateMemoryLocation(location MemoryLocation) error {
	if location.AddressLength < 1 || location.AddressLength > 8 {
		return fmt.Errorf("UDS memory address length %d must be 1 through 8 bytes", location.AddressLength)
	}
	if location.SizeLength < 1 || location.SizeLength > 8 {
		return fmt.Errorf("UDS memory size length %d must be 1 through 8 bytes", location.SizeLength)
	}
	if location.Size == 0 {
		return fmt.Errorf("UDS memory size must not be zero")
	}
	if !fitsWidth(location.Address, location.AddressLength) {
		return fmt.Errorf("UDS memory address %#x does not fit %d bytes", location.Address, location.AddressLength)
	}
	if !fitsWidth(location.Size, location.SizeLength) {
		return fmt.Errorf("UDS memory size %#x does not fit %d bytes", location.Size, location.SizeLength)
	}
	return nil
}

func fitsWidth(value uint64, width uint8) bool {
	return width == 8 || value < uint64(1)<<(width*8)
}

func putBigEndianWidth(destination []byte, value uint64) {
	for index := len(destination) - 1; index >= 0; index-- {
		destination[index] = byte(value)
		value >>= 8
	}
}

func readBigEndianWidth(source []byte) uint64 {
	var value uint64
	for _, octet := range source {
		value = value<<8 | uint64(octet)
	}
	return value
}

func invalidServiceResponse(service ServiceID, format string, arguments ...any) error {
	return fmt.Errorf("%w: UDS service %#02x response %s", ErrInvalidResponse, service, fmt.Sprintf(format, arguments...))
}

func unexpectedServiceResponse(service ServiceID, format string, arguments ...any) error {
	return fmt.Errorf("%w: UDS service %#02x response %s", ErrUnexpectedResponse, service, fmt.Sprintf(format, arguments...))
}
