package j1939

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/tomrford/gocan"
)

// Classical J1939-21 timers: T1 (consecutive DT), T2/T3 (first DT/control),
// T4 (CTS hold), and BAM spacing. See the pinned python-can-j1939 reference in
// .repos/.lock and Linux transport.c at 9f0346dcbea363787186c94ef94dd01aaa215afa.
const (
	consecutiveTimeout = 750 * time.Millisecond
	responseTimeout    = 1250 * time.Millisecond
	holdTimeout        = 1050 * time.Millisecond
	bamInterval        = 50 * time.Millisecond
)

type transmission struct {
	request  *sendRequest
	next     int
	sent     int // highest contiguous sequence accepted by the bus
	end      int // current CTS window; zero means waiting for control
	deadline time.Time
	due      time.Time
	barrier  uint64 // accepted RTS/DT must be observed before reverse control
}

type reception struct {
	source   Address
	pgn      PGN
	size     int
	packets  int
	next     int
	end      int
	window   int
	deadline time.Time
	barrier  uint64 // accepted CTS must be observed before DT
}

func (c *Client) startSend(r *sendRequest) {
	if err := context.Cause(r.ctx); err != nil {
		r.result <- err
		return
	}
	if !c.claimed {
		r.result <- ErrAddressLost
		return
	}
	if c.tx != nil && c.tx.request.destination == GlobalAddress {
		r.result <- ErrBusy
		return
	}
	if len(r.payload) <= 8 {
		r.result <- c.sendFrame(r, r.pgn, r.priority, r.payload)
		return
	}
	if c.tx != nil || c.rx != nil && c.rx.source == r.destination {
		r.result <- ErrBusy
		return
	}
	c.tx = &transmission{request: r, next: 1}
	data := controlBytes(transportRTS, r.pgn)
	binary.LittleEndian.PutUint16(data[1:3], uint16(len(r.payload)))
	data[3] = byte((len(r.payload) + 6) / 7)
	data[4] = data[3]
	if r.destination == GlobalAddress {
		data[0] = transportBAM
		data[4] = 0xff
	}
	if err := c.sendFrame(r, transportControlPGN, r.priority, data[:]); err != nil {
		c.finishSend(err)
		return
	}
	c.tx.barrier = c.sent
	c.tx.deadline = time.Now().Add(responseTimeout)
	if r.destination == GlobalAddress {
		c.tx.due = time.Now().Add(bamInterval)
	}
}

func (c *Client) sendFrame(r *sendRequest, pgn PGN, priority uint8, data []byte) error {
	if err := context.Cause(c.ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(r.ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	return c.frame(ctx, priority, pgn, c.config.Address, r.destination, data)
}

func (c *Client) finishSend(err error) {
	if c.tx != nil {
		c.tx.request.result <- err
		c.tx = nil
	}
}

func controlBytes(kind byte, pgn PGN) [8]byte {
	return [8]byte{kind, 0xff, 0xff, 0xff, 0xff, byte(pgn), byte(pgn >> 8), byte(pgn >> 16)}
}

func (c *Client) control(destination Address, data [8]byte) error {
	return c.frame(c.ctx, 7, transportControlPGN, c.config.Address, destination, data[:])
}

func (c *Client) abort(destination Address, pgn PGN, reason byte) error {
	data := controlBytes(transportAbort, pgn)
	data[1] = reason
	return c.control(destination, data)
}

func (c *Client) failSend(err error, reason byte) error {
	r := c.tx.request
	var abortErr error
	if r.destination != GlobalAddress {
		abortErr = c.abort(r.destination, r.pgn, reason)
	}
	c.finishSend(err)
	return abortErr
}

func (c *Client) failReceive(reason byte) error {
	r := c.rx
	c.rx = nil
	return c.abort(r.source, r.pgn, reason)
}

func (c *Client) invalidatePeer(peer Address) {
	// A changed known NAME or NAME move breaks transport identity. Do not send
	// an abort to an address that may now belong to a different controller.
	if c.tx != nil && c.tx.request.destination == peer {
		c.finishSend(fmt.Errorf("%w: transport peer address changed", ErrProtocol))
	}
	if c.rx != nil && c.rx.source == peer {
		c.rx = nil
	}
}

func (c *Client) handleTransport(event gocan.FrameEvent, h Header, data []byte) error {
	now := event.Timestamp
	// Arrival time decides whether a queued frame was timely.
	if err := c.expireTransport(now); err != nil {
		return err
	}
	if h.PGN == transportDataPGN {
		r := c.rx
		if r == nil || r.source != h.Source {
			return nil
		}
		if c.observed < r.barrier || len(data) != 8 || int(data[0]) != r.next || r.next > r.end {
			return c.failReceive(7)
		}
		r.next++
		if r.next > r.packets {
			ack := controlBytes(transportEOMA, r.pgn)
			binary.LittleEndian.PutUint16(ack[1:3], uint16(r.size))
			ack[3] = byte(r.packets)
			c.rx = nil
			return c.control(r.source, ack)
		}
		if r.next > r.end {
			return c.grant()
		}
		r.deadline = now.Add(consecutiveTimeout)
		return nil
	}
	if h.PGN != transportControlPGN {
		return nil
	}
	if len(data) > 0 && data[0] == transportBAM && c.rx != nil && c.rx.source == h.Source {
		return c.failReceive(2)
	}
	if len(data) != 8 {
		if c.rx != nil && c.rx.source == h.Source {
			return c.failReceive(2)
		}
		if c.tx != nil && c.tx.request.destination == h.Source {
			return c.failSend(fmt.Errorf("%w: short TP.CM", ErrProtocol), 2)
		}
		return nil
	}
	pgn, err := controlPGN(data)
	if err != nil {
		if c.rx != nil && c.rx.source == h.Source && data[0] == transportRTS {
			return c.failReceive(2)
		}
		return nil
	}
	switch data[0] {
	case transportRTS:
		// DT has no PGN. A replacement, even malformed, cannot leave an old
		// same-source receive session accepting bytes from the replacement.
		if c.rx != nil && c.rx.source == h.Source {
			c.rx = nil
			return c.abort(h.Source, pgn, 1)
		}
		if c.rx != nil || c.tx != nil && c.tx.request.destination == h.Source {
			return c.abort(h.Source, pgn, 1)
		}
		size := int(binary.LittleEndian.Uint16(data[1:3]))
		packets := int(data[3])
		if size <= 8 || size > maximumTransportPayload || packets != (size+6)/7 || data[4] == 0 || activePGN(pgn) != nil || pgn == AddressClaimPGN || pgn == RequestPGN {
			return c.abort(h.Source, pgn, 2)
		}
		c.rx = &reception{source: h.Source, pgn: pgn, size: size, packets: packets, next: 1, window: int(data[4])}
		return c.grant()
	case transportAbort:
		if data[2] != 0xff || data[3] != 0xff || data[4] != 0xff {
			return nil
		}
		if c.rx != nil && c.rx.source == h.Source && c.rx.pgn == pgn {
			c.rx = nil
		}
		if c.tx != nil && c.tx.request.destination == h.Source && c.tx.request.pgn == pgn {
			c.finishSend(fmt.Errorf("%w: peer aborted TP with reason %d", ErrProtocol, data[1]))
		}
	case transportCTS, transportEOMA:
		t := c.tx
		if t == nil || t.request.destination != h.Source || t.request.pgn != pgn {
			return nil
		}
		if c.observed < t.barrier {
			return c.failSend(fmt.Errorf("%w: control precedes outgoing RTS or DT window completion", ErrProtocol), 4)
		}
		packets := (len(t.request.payload) + 6) / 7
		if data[0] == transportEOMA {
			if t.sent != packets || t.end != 0 || int(binary.LittleEndian.Uint16(data[1:3])) != len(t.request.payload) || int(data[3]) != packets || data[4] != 0xff {
				return c.failSend(fmt.Errorf("%w: invalid or premature EOMA", ErrProtocol), 2)
			}
			c.finishSend(nil)
			return nil
		}
		count, next := int(data[1]), int(data[2])
		if t.end != 0 {
			return c.failSend(fmt.Errorf("%w: CTS during DT window", ErrProtocol), 4)
		}
		if data[3] != 0xff || data[4] != 0xff || count > packets || count != 0 && (next < 1 || next > t.sent+1 || next+count-1 > packets) {
			return c.failSend(fmt.Errorf("%w: invalid CTS window", ErrProtocol), 2)
		}
		if count == 0 {
			t.deadline = now.Add(holdTimeout)
			return nil
		}
		t.next = next
		t.end = next + count - 1
		t.due = now
	}
	return nil
}

func (c *Client) grant() error {
	r := c.rx
	count := min(r.window, r.packets-r.next+1)
	data := controlBytes(transportCTS, r.pgn)
	data[1] = byte(count)
	data[2] = byte(r.next)
	r.end = r.next + count - 1
	if err := c.control(r.source, data); err != nil {
		return err
	}
	r.barrier = c.sent
	r.deadline = time.Now().Add(responseTimeout)
	return nil
}

func (c *Client) expireTransport(at time.Time) error {
	if c.tx != nil && context.Cause(c.tx.request.ctx) != nil {
		if err := c.failSend(context.Cause(c.tx.request.ctx), 2); err != nil {
			return err
		}
	}
	if c.rx != nil && !at.Before(c.rx.deadline) {
		if err := c.failReceive(3); err != nil {
			return err
		}
	}
	if c.tx != nil && c.tx.due.IsZero() && !at.Before(c.tx.deadline) {
		return c.failSend(ErrTimeout, 3)
	}
	return nil
}

func (c *Client) tickTransport(now time.Time) error {
	t := c.tx
	if t == nil {
		return nil
	}
	if err := context.Cause(t.request.ctx); err != nil {
		return c.failSend(err, 2)
	}
	if t.due.IsZero() {
		return nil
	}
	if now.Before(t.due) {
		return nil
	}
	r := t.request
	if r.destination == GlobalAddress && now.Sub(t.due) > 150*time.Millisecond {
		return c.failSend(ErrTimeout, 3)
	}
	data := [8]byte{byte(t.next), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	copy(data[1:], r.payload[(t.next-1)*7:])
	if err := c.sendFrame(r, transportDataPGN, 7, data[:]); err != nil {
		return c.failSend(err, 2)
	}
	// A native send can finish after its context expires. Check acceptance
	// timing too, including the final packet, before reporting BAM success.
	if r.destination == GlobalAddress && time.Since(t.due) > 150*time.Millisecond {
		return c.failSend(ErrTimeout, 3)
	}
	t.barrier = c.sent
	t.sent = max(t.sent, t.next)
	t.next++
	if r.destination == GlobalAddress {
		if t.next > (len(r.payload)+6)/7 {
			c.finishSend(nil)
		} else {
			t.due = time.Now().Add(bamInterval)
		}
	} else if t.next > t.end {
		t.end = 0
		t.due = time.Time{}
		t.deadline = time.Now().Add(responseTimeout)
	} else {
		t.due = time.Now()
	}
	return nil
}

func (c *Client) shutdown() {
	if !c.claimed {
		return
	}
	// Cancellation cannot use the cancelled client context to send an abort.
	// Bound best-effort cleanup independently; never close the caller's bus.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	abort := func(peer Address, pgn PGN) {
		data := controlBytes(transportAbort, pgn)
		data[1] = 2
		_ = c.frame(ctx, 7, transportControlPGN, c.config.Address, peer, data[:])
	}
	if c.tx != nil && c.tx.request.destination != GlobalAddress {
		abort(c.tx.request.destination, c.tx.request.pgn)
	}
	if c.rx != nil {
		abort(c.rx.source, c.rx.pgn)
	}
}
