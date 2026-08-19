// Package isotp transports complete payloads over normal-addressed CAN links.
// Physical links support classical CAN and CAN FD, including segmented
// transmission and reception. Functional send paths broadcast single-frame
// payloads and have no receive path.
//
// The package defines no application protocol. It moves opaque byte payloads
// and reports transport failures; UDS, OBD, and other diagnostic semantics
// belong in packages above it.
package isotp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tomrford/gocan"
)

const (
	defaultFlowControlTimeout      = time.Second
	defaultConsecutiveFrameTimeout = time.Second
	defaultMaximumPayloadLength    = 4095
	defaultTransmitDataLength      = 8
	defaultWaitFrameLimit          = 10
)

var (
	// ErrProtocol identifies malformed or unexpected ISO-TP traffic.
	ErrProtocol = errors.New("ISO-TP protocol error")
	// ErrExchangeClosed identifies an operation on a closed Exchange, and is the
	// error reported to a Next call that Close cancelled.
	ErrExchangeClosed = errors.New("ISO-TP exchange is closed")
	// ErrFlowControlTimeout indicates that a sender did not receive Flow Control
	// before its configured timeout.
	ErrFlowControlTimeout = errors.New("ISO-TP flow-control timeout")
	// ErrConsecutiveFrameTimeout indicates that a receiver did not receive the
	// next Consecutive Frame before its configured timeout.
	ErrConsecutiveFrameTimeout = errors.New("ISO-TP consecutive-frame timeout")
	// ErrFlowControlOverflow indicates that the peer rejected a transmitted PDU.
	ErrFlowControlOverflow = errors.New("ISO-TP flow-control overflow")
	// ErrWaitFrameLimit indicates that the peer sent more Wait frames than this
	// link permits.
	ErrWaitFrameLimit = errors.New("ISO-TP wait-frame limit reached")
	// ErrPayloadTooLarge indicates that a PDU exceeds MaximumPayloadLength.
	ErrPayloadTooLarge = errors.New("ISO-TP payload is too large")
)

// Config defines one physical, normal-addressing ISO-TP link.
type Config struct {
	TransmitID uint32
	ReceiveID  uint32
	FrameFlags gocan.FrameFlags

	// TransmitDataLength is the maximum CAN data length used for transmitted
	// frames. Zero selects 8. CAN FD additionally permits 12, 16, 20, 24, 32,
	// 48, or 64.
	TransmitDataLength uint8
	// PadFrames pads every transmitted frame to TransmitDataLength. CAN FD
	// frames are always padded as needed to reach a legal CAN FD data length.
	PadFrames bool
	// PaddingByte fills both requested padding and the padding required to reach
	// a legal CAN FD data length.
	PaddingByte byte

	// MaximumPayloadLength limits both transmitted and received PDU allocation.
	// Zero selects 4095 bytes. Values above 4095 use the ISO-TP 32-bit First
	// Frame length form.
	MaximumPayloadLength uint32

	AdvertisedBlockSize      uint8
	AdvertisedSeparationTime time.Duration
	FlowControlTimeout       time.Duration
	ConsecutiveFrameTimeout  time.Duration

	// WaitFrameLimit is how many consecutive Flow Control Wait frames a peer may
	// send before a transmission fails. Zero selects 10 to tolerate peers that
	// wait while erasing or programming memory.
	WaitFrameLimit uint8
}

// Link binds one ISO-TP transmit and receive identifier to a raw CAN bus.
//
// A Link has exactly one receive position, established at New and advanced by
// Receive and Exchange.Next. Begin repositions it to the present, discarding
// unread payloads, because ISO-TP has no transaction identifier and a request
// must not be answered by traffic that predates it. A multi-frame Send
// repositions it for the same reason: it has to recognise its own Flow Control.
// Clearing the underlying capture during an operation discards that position,
// so the operation fails with gocan.ErrCursorStale rather than reading traffic
// from before the reset.
//
// Link serialises Send calls and Receive calls independently. Begin owns both
// paths until its Exchange closes. A server must finish Receive before calling
// Send on the same Link because segmented reception can transmit Flow Control
// without taking the independent send path. Use one Link as a client (Begin) or
// as a server (alternating Receive and Send), not as both at once. Mixing roles
// makes it unpredictable which caller consumes an incoming payload.
//
// Reuse one Link for each logical endpoint: separately constructed Links with
// an overlapping bus and receive address cannot distinguish their traffic.
type Link struct {
	bus     gocan.Bus
	capture *gocan.Capture

	transmitter
	receiveKey gocan.FrameKey

	blockSize               uint8
	separationTime          uint8
	flowControlTimeout      time.Duration
	consecutiveFrameTimeout time.Duration
	waitFrameLimit          uint8

	// sending and receiving are one-token channels. Holding sending grants
	// exclusive transmission; holding receiving grants exclusive use of cursor.
	// Begin takes sending before receiving, and nothing takes them in the other
	// order.
	sending   chan struct{}
	receiving chan struct{}
	cursor    gocan.Cursor
}

// Exchange is one payload sent by Begin together with the payloads that arrive
// after it. It owns its Link until Close.
//
// Next may be called repeatedly; each call returns one complete ISO-TP payload
// and does not interpret its application-level meaning. A protocol above this
// one decides how many payloads an exchange contains.
type Exchange struct {
	link   *Link
	ctx    context.Context
	cancel context.CancelCauseFunc

	mu     sync.Mutex
	closed bool
}

// New validates config and prepares a physical, normal-addressing ISO-TP Link.
func New(bus gocan.Bus, config Config) (*Link, error) {
	if bus == nil {
		return nil, errors.New("ISO-TP link requires a bus")
	}
	capture := bus.Capture()
	if capture == nil {
		return nil, errors.New("ISO-TP bus requires a capture")
	}
	transmitter, err := newTransmitter(config.TransmitID, config.FrameFlags, config.TransmitDataLength, config.PadFrames, config.PaddingByte)
	if err != nil {
		return nil, err
	}
	if err := validateID(config.ReceiveID, config.FrameFlags); err != nil {
		return nil, fmt.Errorf("ISO-TP receive ID: %w", err)
	}

	separationTime, err := encodeSeparationTime(config.AdvertisedSeparationTime)
	if err != nil {
		return nil, err
	}
	flowControlTimeout, err := configuredTimeout(config.FlowControlTimeout, defaultFlowControlTimeout, "flow-control")
	if err != nil {
		return nil, err
	}
	consecutiveFrameTimeout, err := configuredTimeout(config.ConsecutiveFrameTimeout, defaultConsecutiveFrameTimeout, "consecutive-frame")
	if err != nil {
		return nil, err
	}
	transmitter.maximumPayloadLength = config.MaximumPayloadLength
	if transmitter.maximumPayloadLength == 0 {
		transmitter.maximumPayloadLength = defaultMaximumPayloadLength
	}
	waitFrameLimit := config.WaitFrameLimit
	if waitFrameLimit == 0 {
		waitFrameLimit = defaultWaitFrameLimit
	}

	link := &Link{
		bus:                     bus,
		capture:                 capture,
		transmitter:             transmitter,
		receiveKey:              gocan.FrameKey{Bus: bus.ID(), ID: config.ReceiveID, Direction: gocan.DirectionReceive, Extended: config.FrameFlags.Has(gocan.FrameExtended)},
		blockSize:               config.AdvertisedBlockSize,
		separationTime:          separationTime,
		flowControlTimeout:      flowControlTimeout,
		consecutiveFrameTimeout: consecutiveFrameTimeout,
		waitFrameLimit:          waitFrameLimit,
		sending:                 make(chan struct{}, 1),
		receiving:               make(chan struct{}, 1),
		cursor:                  capture.End(),
	}
	link.sending <- struct{}{}
	link.receiving <- struct{}{}
	return link, nil
}

// Send transmits one complete ISO-TP payload and returns when the peer has
// accepted all of it. The Link is free again once Send returns.
//
// A segmented payload additionally repositions the receive position so that
// only Flow Control sent in reply to this payload can satisfy it.
func (link *Link) Send(ctx context.Context, payload []byte) error {
	transmission, err := link.prepareTransmission(payload)
	if err != nil {
		return err
	}
	if err := link.acquire(ctx, link.sending); err != nil {
		return err
	}
	defer link.release(link.sending)

	if transmission.multiFrame {
		if err := link.acquire(ctx, link.receiving); err != nil {
			return err
		}
		defer link.release(link.receiving)
		link.cursor = link.capture.End()
	}

	operationContext, cancel := link.operationContext(ctx)
	defer cancel()
	return withCause(operationContext, link.transmit(operationContext, transmission))
}

// Receive waits for and reassembles the next complete ISO-TP payload sent to
// this link, advancing its receive position.
func (link *Link) Receive(ctx context.Context) ([]byte, error) {
	if err := link.acquire(ctx, link.receiving); err != nil {
		return nil, err
	}
	defer link.release(link.receiving)

	operationContext, cancel := link.operationContext(ctx)
	defer cancel()
	payload, err := link.receive(operationContext, 0)
	return payload, withCause(operationContext, err)
}

// Begin transmits one complete ISO-TP payload and returns an Exchange scoped to
// the payloads that arrive after it. The caller must Close the Exchange.
//
// ISO-TP has no transaction identifier, so Begin repositions the link's receive
// position immediately before its first frame reaches the bus and discards any
// unread payload. For the life of the Exchange the endpoint must not carry
// unrelated traffic on its receive address.
func (link *Link) Begin(ctx context.Context, payload []byte) (*Exchange, error) {
	transmission, err := link.prepareTransmission(payload)
	if err != nil {
		return nil, err
	}
	if err := link.acquire(ctx, link.sending); err != nil {
		return nil, err
	}
	if err := link.acquire(ctx, link.receiving); err != nil {
		link.release(link.sending)
		return nil, err
	}

	exchange := link.newExchange()
	// Keep this boundary short: validation, first-frame construction, and bus
	// lifecycle wiring all happen before the receive frontier is captured.
	link.cursor = link.capture.End()
	operationContext, cancel := exchange.operationContext(ctx)
	defer cancel()
	if err := link.transmit(operationContext, transmission); err != nil {
		// Resolve the cause before Close cancels the exchange context, or the
		// real transport failure is replaced by ErrExchangeClosed.
		failure := withCause(operationContext, err)
		exchange.Close()
		return nil, failure
	}
	return exchange, nil
}

// Next waits for and reassembles the next complete ISO-TP payload of this
// exchange. firstFrameTimeout limits only the wait for its Single or First
// Frame; zero applies no additional limit. The caller's context and the Link's
// consecutive-frame timeout govern the remainder. Concurrent calls are
// serialised.
func (exchange *Exchange) Next(ctx context.Context, firstFrameTimeout time.Duration) ([]byte, error) {
	if firstFrameTimeout < 0 {
		return nil, errors.New("ISO-TP first-frame timeout must not be negative")
	}
	exchange.mu.Lock()
	defer exchange.mu.Unlock()
	if exchange.closed {
		return nil, ErrExchangeClosed
	}

	operationContext, cancel := exchange.operationContext(ctx)
	defer cancel()
	payload, err := exchange.link.receive(operationContext, firstFrameTimeout)
	return payload, withCause(operationContext, err)
}

// Close releases the Link for another operation. It is safe to call more than
// once. A Next already in progress is cancelled and reports ErrExchangeClosed,
// so Close waits only for that call to unwind rather than for a protocol
// timeout.
func (exchange *Exchange) Close() {
	exchange.cancel(ErrExchangeClosed)
	exchange.mu.Lock()
	defer exchange.mu.Unlock()
	if exchange.closed {
		return
	}
	exchange.closed = true
	exchange.link.release(exchange.link.receiving)
	exchange.link.release(exchange.link.sending)
}

// newExchange wires bus loss into one context that lives as long as the
// exchange, so an operation only has to watch that single context.
func (link *Link) newExchange() *Exchange {
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		select {
		case <-link.bus.Done():
			cancel(link.busError())
		case <-ctx.Done():
		}
	}()
	return &Exchange{link: link, ctx: ctx, cancel: cancel}
}

// nextFrame reads the next matching frame and advances the link's receive
// position. Callers must hold the receiving token.
func (link *Link) nextFrame(ctx context.Context) (gocan.FrameEvent, error) {
	frame, cursor, err := link.capture.Next(ctx, link.receiveKey, link.cursor)
	if err != nil {
		return gocan.FrameEvent{}, err
	}
	link.cursor = cursor
	return frame, nil
}

func (link *Link) acquire(ctx context.Context, token chan struct{}) error {
	select {
	case <-token:
		return nil
	case <-link.bus.Done():
		return link.busError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (link *Link) release(token chan struct{}) {
	token <- struct{}{}
}

func (link *Link) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	return watchedContext(parent, link.bus.Done(), link.busError)
}

func (exchange *Exchange) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	return watchedContext(parent, exchange.ctx.Done(), func() error { return context.Cause(exchange.ctx) })
}

// watchedContext derives a cancellable child of parent that also ends when done
// is closed, reporting cause as the cancellation cause.
func watchedContext(parent context.Context, done <-chan struct{}, cause func() error) (context.Context, context.CancelFunc) {
	ctx, cancelCause := context.WithCancelCause(parent)
	go func() {
		select {
		case <-done:
			cancelCause(cause())
		case <-ctx.Done():
		}
	}()
	return ctx, func() { cancelCause(context.Canceled) }
}

// withCause replaces a cancellation error with why the operation context ended,
// so callers see bus loss or exchange closure instead of context.Canceled.
//
// Only cancellation errors are replaced. A protocol, framing, or provider error
// reported as a deadline elapses is the more useful of the two, and this
// package's own timeout sentinels already describe themselves.
func withCause(operationContext context.Context, err error) error {
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if cause := context.Cause(operationContext); cause != nil {
		return cause
	}
	return err
}

func (link *Link) busError() error {
	if err := link.bus.Err(); err != nil {
		return err
	}
	return gocan.ErrBusClosed
}

func configuredTimeout(value, defaultValue time.Duration, name string) (time.Duration, error) {
	if value < 0 {
		return 0, fmt.Errorf("ISO-TP %s timeout must not be negative", name)
	}
	if value == 0 {
		return defaultValue, nil
	}
	return value, nil
}

func validateID(id uint32, flags gocan.FrameFlags) error {
	if flags.Has(gocan.FrameExtended) {
		if id > gocan.MaxExtendedID {
			return fmt.Errorf("extended identifier %#x exceeds %#x", id, gocan.MaxExtendedID)
		}
		return nil
	}
	if id > gocan.MaxStandardID {
		return fmt.Errorf("standard identifier %#x exceeds %#x", id, gocan.MaxStandardID)
	}
	return nil
}

func validateTransmitDataLength(length int, fd bool) error {
	valid := length == 8
	if fd {
		switch length {
		case 8, 12, 16, 20, 24, 32, 48, 64:
			valid = true
		}
	}
	if !valid {
		return fmt.Errorf("ISO-TP transmit data length %d is invalid for this CAN frame type", length)
	}
	return nil
}

func encodeSeparationTime(value time.Duration) (uint8, error) {
	if value < 0 {
		return 0, errors.New("ISO-TP advertised separation time must not be negative")
	}
	if value == 0 {
		return 0, nil
	}
	if value%time.Millisecond == 0 {
		milliseconds := value / time.Millisecond
		if milliseconds <= 0x7f {
			return uint8(milliseconds), nil
		}
	}
	if value%(100*time.Microsecond) == 0 {
		units := value / (100 * time.Microsecond)
		if units >= 1 && units <= 9 {
			return 0xf0 + uint8(units), nil
		}
	}
	return 0, fmt.Errorf("ISO-TP advertised separation time %s cannot be encoded", value)
}
