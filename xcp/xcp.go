// Package xcp exchanges sequential XCP commands directly over CAN or CAN FD.
// It provides session discovery and byte-addressed memory reads without A2L.
// DAQ, events and service packets remain available through the bus's Capture;
// this package consumes no traffic on behalf of an idle client.
package xcp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tomrford/gocan"
)

var (
	ErrClosed                  = errors.New("XCP client is closed")
	ErrNotConnected            = errors.New("XCP session capabilities are not known; CONNECT required")
	ErrSynchronizationRequired = errors.New("XCP command outcome is uncertain; SYNCH required")
	ErrTimeout                 = errors.New("XCP response timeout")
	ErrInvalidResponse         = errors.New("invalid XCP response")
	ErrUnsupported             = errors.New("unsupported XCP operation")
	ErrSessionTerminated       = errors.New("XCP session terminated by ECU")
)

// Command is the first byte of a command packet. These are the commands with
// typed support here; Do can also send other sequential, single-response commands.
type Command byte

const (
	CommandConnect         Command = 0xff
	CommandDisconnect      Command = 0xfe
	CommandGetStatus       Command = 0xfd
	CommandSynch           Command = 0xfc
	CommandGetCommModeInfo Command = 0xfb
	CommandSetMTA          Command = 0xf6
	CommandUpload          Command = 0xf5
	CommandShortUpload     Command = 0xf4
)

// Config describes a single ECU using the same extended/FD format in both
// directions. Bit rates belong to the already-open bus. Reuse one Client per
// endpoint; multiple clients cannot distinguish replies on the same receive ID.
type Config struct {
	TransmitID uint32
	ReceiveID  uint32
	FrameFlags gocan.FrameFlags
	// TransmitDataLength is the maximum packet/frame size, including CONNECT
	// bootstrap padding. Zero selects 8; FD also allows 12, 16, 20, 24, 32, 48, 64.
	TransmitDataLength uint8
	// PadFrames implements the ECU's MAX_DLC_REQUIRED setting. FD packets are
	// always padded to a legal DLC. PaddingByte is transport fill, not data.
	PadFrames   bool
	PaddingByte byte
	// Timeout bounds each response wait. Zero selects a library default of one
	// second, not an ECU timing guarantee. EV_CMD_PENDING restarts this wait;
	// a caller deadline bounds the whole operation even if pending events repeat.
	Timeout time.Duration
}

// Capabilities contains CONNECT fields. Versions are the reported major bytes,
// not inferred minor versions. AddressGranularity is bytes per address element.
// Resources and CommunicationMode retain the ECU's raw capability bit fields.
type Capabilities struct {
	Resources          byte
	CommunicationMode  byte
	ByteOrder          binary.ByteOrder
	AddressGranularity uint8
	MaxCTO             uint8
	MaxDTO             uint16
	ProtocolVersion    uint8
	TransportVersion   uint8
}

type Request struct {
	Command Command
	// Data contains command parameters without the command byte or CAN padding.
	Data []byte
}

// Response retains every byte after RES, including possible CAN padding. Typed
// operations use command lengths/counts to select data; Do never trims zeros.
// SYNCH returns the bytes after ERR (starting with ERR_CMD_SYNCH) on success.
type Response struct{ Data []byte }

// NegativeResponseError associates the wire error with the outstanding command;
// XCP replies themselves carry no command echo or transaction identifier.
type NegativeResponseError struct {
	Command Command
	Code    byte
}

func (err *NegativeResponseError) Error() string {
	return fmt.Sprintf("XCP command %#02x returned error %#02x", err.Command, err.Code)
}

// Client owns command sequencing, not the Bus or Capture. Complete memory reads
// and Do calls share one cancellable gate. Close cancels active and queued calls
// without disconnecting the ECU or closing the caller's bus; use Disconnect
// explicitly when an acknowledged protocol disconnect is wanted.
//
// After an accepted command has no trustworthy final response, ordinary commands
// fail with ErrSynchronizationRequired. Synchronize must receive ERR_CMD_SYNCH
// before they can resume. Recovery relies on an ECU obeying sequential XCP and
// ordering its old replies before that marker on the configured receive ID.
// An unanswered CONNECT or DISCONNECT may instead be recovered with CONNECT;
// that recovery includes a SYNCH barrier and a fresh CONNECT for capabilities.
// Once a SYNCH send is accepted, further CONNECT retries are blocked until
// synchronisation succeeds, so an old marker cannot fence a newer CONNECT.
type Client struct {
	bus     gocan.Bus
	capture *gocan.Capture
	config  Config
	key     gocan.FrameKey
	gate    chan struct{}
	ctx     context.Context
	cancel  context.CancelCauseFunc
	// Session fields are protected by gate.
	capabilities Capabilities
	connected    bool
	pending      Command // zero, or the last accepted command lacking a trusted reply
	// Retention can be read while a command owns gate.
	mu        sync.Mutex
	cursor    gocan.Cursor
	retaining bool
}

func New(bus gocan.Bus, config Config) (*Client, error) {
	if bus == nil || bus.Capture() == nil {
		return nil, errors.New("XCP requires a bus with a capture")
	}
	if config.FrameFlags & ^(gocan.FrameExtended|gocan.FrameFD|gocan.FrameBitRateSwitch) != 0 {
		return nil, errors.New("invalid XCP frame flags")
	}
	if config.TransmitDataLength == 0 {
		config.TransmitDataLength = 8
	}
	if config.TransmitDataLength < 8 {
		return nil, errors.New("XCP frame capacity must be at least 8 bytes")
	}
	for _, id := range []uint32{config.TransmitID, config.ReceiveID} {
		if _, err := gocan.NewFrame(id, make([]byte, config.TransmitDataLength), config.FrameFlags); err != nil {
			return nil, err
		}
	}
	if config.TransmitID == config.ReceiveID {
		return nil, errors.New("XCP transmit and receive IDs must differ")
	}
	if config.Timeout < 0 {
		return nil, errors.New("XCP timeout must not be negative")
	}
	if config.Timeout == 0 {
		config.Timeout = time.Second
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	client := &Client{bus: bus, capture: bus.Capture(), config: config, gate: make(chan struct{}, 1), ctx: ctx, cancel: cancel,
		key: gocan.FrameKey{ID: config.ReceiveID, Bus: bus.ID(), Direction: gocan.DirectionReceive, Extended: config.FrameFlags.Has(gocan.FrameExtended)}}
	client.gate <- struct{}{}
	return client, nil
}

// Close is idempotent. An already accepted native send cannot be revoked.
func (client *Client) Close() { client.cancel(ErrClosed) }

// RetentionCursor protects active receive progress and an unanswered SYNCH.
// Pass it alongside other readers' cursors to Capture.Prune. Otherwise idle
// clients retain no unsolicited traffic.
// An unplaceable active cursor fails the command visibly. Capture's zero cursor
// (an initially empty capture) survives Clear, so loss at that initial frontier
// cannot be detected. Avoid clearing captures during protocol operations.
func (client *Client) RetentionCursor() gocan.Cursor {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.retaining && client.ctx.Err() == nil {
		return client.cursor
	}
	return client.capture.End()
}

func (client *Client) begin(parent context.Context) (context.Context, func(), error) {
	ctx, cancel := context.WithCancelCause(parent)
	stop := context.AfterFunc(client.ctx, func() { cancel(ErrClosed) })
	cancelBus := func() {
		err := client.bus.Err()
		if err == nil {
			err = gocan.ErrBusClosed
		}
		cancel(err)
	}
	go func() {
		select {
		case <-client.bus.Done():
			cancelBus()
		case <-ctx.Done():
		}
	}()
	cleanup := func() { stop(); cancel(context.Canceled) }
	select {
	case <-ctx.Done():
		cleanup()
		return nil, nil, context.Cause(ctx)
	case <-client.gate:
	}
	finish := func() {
		client.mu.Lock()
		client.retaining = client.pending == CommandSynch
		client.mu.Unlock()
		client.gate <- struct{}{}
		cleanup()
	}
	if err := context.Cause(client.ctx); err != nil {
		finish()
		return nil, nil, err
	}
	// Acquiring the gate can win the race with the bus watcher. Observe an
	// already-closed bus before inspecting the previous command's state.
	select {
	case <-client.bus.Done():
		cancelBus()
	default:
	}
	if err := context.Cause(ctx); err != nil {
		finish()
		return nil, nil, err
	}
	return ctx, finish, nil
}

// Do sends one sequential command and waits for RES/ERR. It does not implement
// block transfers, interleaved commands, response suppression, or asynchronous
// completion. The caller must not select these modes in raw parameters.
// Supported session/read commands are validated and update the cached session.
// Other commands invalidate capabilities after an accepted send, since their
// effects are not modelled; CONNECT is required before further ordinary calls.
// CONNECT recovery may send SYNCH and a fresh CONNECT as described on Client.
func (client *Client) Do(ctx context.Context, request Request) (Response, error) {
	ctx, finish, err := client.begin(ctx)
	if err != nil {
		return Response{}, err
	}
	defer finish()
	// A public SYNCH must not strand a possibly disconnected bootstrap. The
	// private CONNECT recovery can send its barrier after observing a reply.
	if request.Command == CommandSynch && !client.connected && (client.pending == 0 || client.pending == CommandConnect || client.pending == CommandDisconnect) {
		return Response{}, ErrNotConnected
	}
	return client.command(ctx, request)
}

func (client *Client) Connect(ctx context.Context) (Capabilities, error) {
	ctx, finish, err := client.begin(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	defer finish()
	_, err = client.command(ctx, Request{Command: CommandConnect, Data: []byte{0}})
	if err != nil {
		return Capabilities{}, err
	}
	return client.capabilities, nil
}

func (client *Client) Disconnect(ctx context.Context) error {
	_, err := client.Do(ctx, Request{Command: CommandDisconnect})
	return err
}

// Synchronize discards late RES/ERR packets until ERR_CMD_SYNCH is observed.
// Failure leaves ordinary commands blocked; it never retries an earlier request.
// Retrying Synchronize resumes waiting for the same accepted SYNCH, retaining
// its capture cursor until the acknowledgement arrives or the client closes.
// It rejects known-disconnected sessions and unanswered CONNECT/DISCONNECTs
// without sending: a disconnected ECU need not answer SYNCH. Use Connect instead.
func (client *Client) Synchronize(ctx context.Context) error {
	_, err := client.Do(ctx, Request{Command: CommandSynch})
	return err
}

// Status preserves session and resource-protection flags without policy about
// unlocking, calibration, or DAQ. ConfigurationID uses the negotiated byte order.
type Status struct {
	Session         byte
	Protection      byte
	State           byte
	ConfigurationID uint16
}

func (client *Client) Status(ctx context.Context) (Status, error) {
	ctx, finish, err := client.begin(ctx)
	if err != nil {
		return Status{}, err
	}
	defer finish()
	response, err := client.command(ctx, Request{Command: CommandGetStatus})
	if err != nil {
		return Status{}, err
	}
	data := response.Data
	return Status{data[0], data[1], data[2], client.capabilities.ByteOrder.Uint16(data[3:5])}, nil
}

// CommunicationInfo reports optional modes; the client continues using standard
// sequential transfers regardless of these advertised capabilities.
type CommunicationInfo struct {
	Mode              byte
	MaxBlockSize      byte
	MinSeparationTime byte // units of 100 microseconds
	QueueSize         byte
	DriverVersion     byte
}

func (client *Client) CommunicationInfo(ctx context.Context) (CommunicationInfo, error) {
	response, err := client.Do(ctx, Request{Command: CommandGetCommModeInfo})
	if err != nil {
		return CommunicationInfo{}, err
	}
	data := response.Data
	return CommunicationInfo{data[1], data[3], data[4], data[5], data[6]}, nil
}
