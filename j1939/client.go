package j1939

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/tomrford/gocan"
)

// RequestPGN carries the three-byte PGN being requested.
const RequestPGN PGN = 0xea00

var (
	ErrClosed      = errors.New("J1939 client is closed")
	ErrAddressLost = errors.New("J1939 local address was lost")
	ErrBusy        = errors.New("J1939 transport is busy")
	ErrTimeout     = errors.New("J1939 transport timeout")
)

// Config identifies one local controller. Address must be 0..253 and NAME must
// be nonzero, with reserved and arbitrary-address-capable bits clear. This
// client uses the configured address only; address selection and commanded
// address changes are outside its supported subset.
type Config struct {
	Name    Name
	Address Address
}

// Client owns live address arbitration and classical TP handshakes on a Bus.
// Use exactly one client per local address, and do not send raw traffic from
// that address alongside it. The caller owns the Bus and Capture.
//
// One outgoing and one incoming TP transfer to different peers may run at once.
// Additional RTS transfers are rejected with Busy. Single-frame and BAM receive
// need no handshake. Read all payloads through Capture and Decoder; the client's
// CTS/EOMA transmissions are recorded there too. Decoder completion still means
// observed bytes, not acknowledged delivery or successful local arbitration.
// Flow-control boundaries follow Capture append order: a response recorded
// before its corresponding accepted transmission is rejected.
//
// Losing arbitration cancels transport without sending from the lost address.
// The client remains in Cannot Claim, answering address-claim requests;
// Send returns ErrAddressLost. Close and open a new client to change configuration.
// Background send failure, bus failure or capture cursor loss stops the client.
// Calls are safe concurrently. Native sends already accepted cannot be revoked.
type Client struct {
	bus      gocan.Bus
	config   Config
	ctx      context.Context
	cancel   context.CancelCauseFunc
	done     chan struct{}
	ready    chan error
	requests chan *sendRequest
	mu       sync.Mutex
	cursor   gocan.Cursor
	address  Address
	// Only the run goroutine accesses fields below.
	claimed  bool
	claimAt  time.Time
	cannotAt time.Time
	peers    [254]Name
	tx       *transmission
	rx       *reception
	// Ordinals establish actual flow-control boundaries in Capture append
	// order, including identical CTS frames from successive sessions.
	sent     uint64
	observed uint64
}

type sendRequest struct {
	ctx         context.Context
	priority    uint8
	pgn         PGN
	destination Address
	payload     []byte
	result      chan error
}

// Open claims the configured address and waits 250 ms for competing claims
// before returning. ctx governs the client's whole lifetime, including sends
// and receive handshakes. Close stops it without closing the bus. The 250 ms
// wait is conservative even for addresses permitted to start immediately.
func Open(ctx context.Context, bus gocan.Bus, config Config) (*Client, error) {
	if bus == nil || bus.Capture() == nil {
		return nil, errors.New("J1939 requires a bus with a capture")
	}
	if config.Address >= NullAddress || config.Name == 0 {
		return nil, errors.New("J1939 requires a usable local address and nonzero NAME")
	}
	if config.Name.Reserved() != 0 || config.Name.ArbitraryAddressCapable() {
		return nil, fmt.Errorf("%w: reserved or arbitrary-address-capable NAME", ErrUnsupported)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	c := &Client{bus: bus, config: config, ctx: ctx, cancel: cancel, done: make(chan struct{}), ready: make(chan error, 1), requests: make(chan *sendRequest), cursor: bus.Capture().End(), address: NullAddress}
	go c.run()
	select {
	case err := <-c.ready:
		if err == nil {
			return c, nil
		}
		c.Close()
		return nil, err
	case <-c.done:
		return nil, c.Err()
	case <-ctx.Done():
		c.Close()
		return nil, context.Cause(ctx)
	}
}

func (c *Client) Close()                { c.cancel(ErrClosed); <-c.done }
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns the terminal cause after Done closes, including ErrClosed after Close.
func (c *Client) Err() error { return context.Cause(c.ctx) }

// Address is NullAddress while unclaimed or after loss/closure.
func (c *Client) Address() Address { c.mu.Lock(); defer c.mu.Unlock(); return c.address }

// RetentionCursor must be included in Capture.Prune while the client is live.
// Do not Clear a live capture: its initial zero cursor cannot detect Clear.
func (c *Client) RetentionCursor() gocan.Cursor { c.mu.Lock(); defer c.mu.Unlock(); return c.cursor }

// Send transmits an opaque PGN payload (0..1785 bytes). Single frames and BAM
// complete when the bus accepts their frames; directed TP requires matching
// EOMA. A caller deadline bounds the entire transfer, including repeated CTS
// pauses/retransmissions. Busy is returned without starting another TP session.
// PDU2 can be directed only when transported (more than eight bytes).
// Network-management and transport PGNs are owned by the client.
func (c *Client) Send(ctx context.Context, priority uint8, pgn PGN, destination Address, payload []byte) error {
	if err := activePGN(pgn); err != nil {
		return err
	}
	if pgn == AddressClaimPGN || pgn == RequestPGN {
		return fmt.Errorf("%w: use the client's network-management operations", ErrUnsupported)
	}
	return c.send(ctx, priority, pgn, destination, payload)
}

// Request sends a PGN request at priority 6. Responses remain in Capture; this
// does not infer how many responders exist or wait for an application response.
func (c *Client) Request(ctx context.Context, destination Address, pgn PGN) error {
	if err := activePGN(pgn); err != nil {
		return err
	}
	return c.send(ctx, 6, RequestPGN, destination, []byte{byte(pgn), byte(pgn >> 8), byte(pgn >> 16)})
}

func (c *Client) send(ctx context.Context, priority uint8, pgn PGN, destination Address, payload []byte) error {
	if priority > 7 || destination == NullAddress || destination == c.config.Address || len(payload) > maximumTransportPayload {
		return fmt.Errorf("%w: invalid priority, destination or payload size", ErrUnsupported)
	}
	if len(payload) <= 8 && !pgn.isPDU1() && destination != GlobalAddress {
		return fmt.Errorf("%w: single-frame PDU2 requires global destination", ErrUnsupported)
	}
	r := &sendRequest{ctx: ctx, priority: priority, pgn: pgn, destination: destination, payload: append([]byte(nil), payload...), result: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-c.done:
		return c.Err()
	case c.requests <- r:
	}
	// Once accepted, wait for cancellation cleanup and the definite native result.
	select {
	case err := <-r.result:
		return err
	case <-c.done:
		return c.Err()
	}
}

func activePGN(pgn PGN) error {
	if err := pgn.validate(); err != nil {
		return err
	}
	if pgn&0x20000 != 0 || pgn == transportControlPGN || pgn == transportDataPGN || pgn == 0xc700 || pgn == 0xc800 || pgn == 0xfed8 {
		return fmt.Errorf("%w: PGN %#x", ErrUnsupported, pgn)
	}
	return nil
}

func (c *Client) run() {
	defer func() {
		c.mu.Lock()
		c.address = NullAddress
		c.mu.Unlock()
		close(c.done)
	}()
	if err := c.claim(c.config.Address); err != nil {
		c.cancel(err)
		return
	}
	c.claimAt = time.Now().Add(250 * time.Millisecond)
	// Capture has no wildcard waiter. A small poll interval bounds handshake
	// latency without adding a second receive contract or a driver goroutine.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			c.shutdown()
			return
		case <-c.bus.Done():
			err := c.bus.Err()
			if err == nil {
				err = gocan.ErrBusClosed
			}
			c.cancel(err)
			return
		case r := <-c.requests:
			if err := c.poll(); err != nil {
				c.cancel(err)
				return
			}
			c.startSend(r)
		case <-ticker.C:
			if err := c.poll(); err != nil {
				c.cancel(err)
				return
			}
		}
		if err := c.tick(time.Now()); err != nil {
			c.cancel(err)
			return
		}
	}
}

func (c *Client) poll() error {
	cutoff := time.Now()
	events, next, err := c.bus.Capture().FramesSince(c.RetentionCursor())
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.Bus != c.bus.ID() || event.Frame.Flags != gocan.FrameExtended {
			continue
		}
		h, _ := ParseID(event.Frame.ID)
		if event.Direction == gocan.DirectionTransmit {
			if h.Source == c.config.Address || h.Source == NullAddress && h.PGN == AddressClaimPGN && event.Frame.DataLength() == 8 && Name(binary.LittleEndian.Uint64(event.Frame.Data[:8])) == c.config.Name {
				c.observed++
			}
			continue
		}
		if h.EDP() != 0 {
			continue
		}
		if err := c.handle(event, h); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.cursor = next
	c.mu.Unlock()
	if !c.claimAt.IsZero() && !cutoff.Before(c.claimAt) {
		c.claimAt = time.Time{}
		c.claimed = true
		c.mu.Lock()
		c.address = c.config.Address
		c.mu.Unlock()
		c.ready <- nil
	}
	// Sends while processing this batch may have allowed new responses into
	// Capture. Only expire through the instant preceding this snapshot.
	return c.expireTransport(cutoff)
}

func (c *Client) handle(event gocan.FrameEvent, h Header) error {
	data := event.Frame.Data[:event.Frame.DataLength()]
	if h.PGN == AddressClaimPGN && h.Destination == GlobalAddress && h.Source != GlobalAddress {
		name, err := ParseName(data)
		if err != nil || name == 0 || name.Reserved() != 0 {
			return nil
		}
		if h.Source == c.config.Address && name != c.config.Name && (!c.claimAt.IsZero() || c.claimed) {
			if name < c.config.Name {
				c.loseAddress()
			} else {
				return c.claim(c.config.Address)
			}
		}
		for addr, old := range c.peers {
			if old == name && Address(addr) != h.Source {
				c.peers[addr] = 0
				c.invalidatePeer(Address(addr))
			}
		}
		if h.Source < NullAddress && c.peers[h.Source] != name {
			c.peers[h.Source] = name
			c.invalidatePeer(h.Source)
		}
		return nil
	}
	if h.PGN == RequestPGN && (h.Destination == GlobalAddress || h.Destination == c.config.Address) && len(data) == 3 && (h.Source < NullAddress || h.Source == NullAddress && h.Destination == GlobalAddress) {
		pgn := PGN(data[0]) | PGN(data[1])<<8 | PGN(data[2])<<16
		if pgn == AddressClaimPGN {
			if c.claimed || !c.claimAt.IsZero() {
				return c.claim(c.config.Address)
			}
			if c.cannotAt.IsZero() {
				c.scheduleCannotClaim()
			}
		}
	}
	if !c.claimed || h.Source >= NullAddress || h.Destination != c.config.Address || h.Source == c.config.Address {
		return nil
	}
	return c.handleTransport(event, h, data)
}

func (c *Client) claim(address Address) error {
	var data [8]byte
	binary.LittleEndian.PutUint64(data[:], uint64(c.config.Name))
	return c.frame(c.ctx, 6, AddressClaimPGN, address, GlobalAddress, data[:])
}

func (c *Client) loseAddress() {
	c.claimed = false
	c.claimAt = time.Time{}
	c.mu.Lock()
	c.address = NullAddress
	c.mu.Unlock()
	c.finishSend(ErrAddressLost)
	c.rx = nil
	c.scheduleCannotClaim()
}

func (c *Client) scheduleCannotClaim() {
	// J1939-81 spreads Cannot Claim responses over 0..153 ms.
	c.cannotAt = time.Now().Add(time.Duration(rand.IntN(154)) * time.Millisecond)
}

func (c *Client) tick(now time.Time) error {
	if !c.cannotAt.IsZero() && !now.Before(c.cannotAt) {
		c.cannotAt = time.Time{}
		if err := c.claim(NullAddress); err != nil {
			return err
		}
		select {
		case c.ready <- ErrAddressLost:
		default:
		}
	}
	return c.tickTransport(now)
}

func (c *Client) frame(ctx context.Context, priority uint8, pgn PGN, source, destination Address, data []byte) error {
	id, err := (Header{priority, pgn, source, destination}).ID()
	if err != nil {
		return err
	}
	frame, err := gocan.NewFrame(id, data, gocan.FrameExtended)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	err = c.bus.Send(ctx, frame)
	if err == nil {
		c.sent++
	}
	return err
}
