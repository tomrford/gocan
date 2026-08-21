// Package uds exchanges raw and typed Unified Diagnostic Services requests and
// responses over an ISO-TP link. Typed operations encode one standard service;
// callers retain session, security, and workflow policy.
package uds

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tomrford/gocan/isotp"
)

const (
	defaultP2Timeout     = 100 * time.Millisecond
	defaultP2StarTimeout = 5 * time.Second
	responsePending      = ResponseCode(0x78)
)

var (
	// ErrInvalidResponse identifies a malformed UDS response.
	ErrInvalidResponse = errors.New("invalid UDS response")
	// ErrUnexpectedResponse identifies a response for a different service.
	ErrUnexpectedResponse = errors.New("unexpected UDS response")
	// ErrP2Timeout indicates that the first response did not arrive in time.
	ErrP2Timeout = errors.New("UDS P2 timeout")
	// ErrP2StarTimeout indicates that a response following ResponsePending did
	// not arrive in time.
	ErrP2StarTimeout = errors.New("UDS P2* timeout")
)

// ServiceID identifies a UDS service.
type ServiceID uint8

// ResponseCode identifies a UDS negative-response code.
type ResponseCode uint8

// Request is one raw UDS request. Data contains every byte after Service.
type Request struct {
	Service ServiceID
	Data    []byte
}

// Response is one positive UDS response. Service is the requested service,
// and Data contains every byte after its positive-response service ID.
type Response struct {
	Service ServiceID
	Data    []byte
}

// NegativeResponseError reports a final negative response from the server.
type NegativeResponseError struct {
	Service ServiceID
	Code    ResponseCode
}

func (err *NegativeResponseError) Error() string {
	return fmt.Sprintf("UDS service %#02x returned negative response %#02x", err.Service, err.Code)
}

// Config sets the UDS application-response timeouts. Zero values select 100
// milliseconds for P2 and five seconds for P2*.
type Config struct {
	P2Timeout     time.Duration
	P2StarTimeout time.Duration
}

// Client exchanges raw UDS requests over one ISO-TP link.
//
// A Client logically owns its Link. Do and Send may be called concurrently,
// but callers must not operate the Link independently or construct another
// Client around it.
type Client struct {
	link          *isotp.Link
	p2Timeout     time.Duration
	p2StarTimeout time.Duration
}

// New validates config and binds a raw UDS client to link.
func New(link *isotp.Link, config Config) (*Client, error) {
	if link == nil {
		return nil, errors.New("UDS client requires an ISO-TP link")
	}
	p2Timeout, err := configuredTimeout(config.P2Timeout, defaultP2Timeout, "P2")
	if err != nil {
		return nil, err
	}
	p2StarTimeout, err := configuredTimeout(config.P2StarTimeout, defaultP2StarTimeout, "P2*")
	if err != nil {
		return nil, err
	}
	return &Client{
		link:          link,
		p2Timeout:     p2Timeout,
		p2StarTimeout: p2StarTimeout,
	}, nil
}

// Do sends request and waits for its final response. ResponsePending restarts
// the wait using P2*. The caller's context bounds the complete exchange.
func (client *Client) Do(ctx context.Context, request Request) (Response, error) {
	payload, err := request.payload()
	if err != nil {
		return Response{}, err
	}

	exchange, err := client.link.Begin(ctx, payload)
	if err != nil {
		return Response{}, err
	}
	defer exchange.Close()

	timeout := client.p2Timeout
	timeoutError := ErrP2Timeout
	for {
		payload, err := nextWithTimeout(ctx, exchange, timeout, timeoutError)
		if err != nil {
			return Response{}, err
		}
		response, negative, err := parseResponse(request.Service, payload)
		if err != nil {
			return Response{}, err
		}
		if negative == nil {
			return response, nil
		}
		if negative.Code != responsePending {
			return Response{}, negative
		}
		timeout = client.p2StarTimeout
		timeoutError = ErrP2StarTimeout
	}
}

// Send transmits request without waiting for a response. It is intended for a
// request whose service data already contains suppressPositiveResponse.
func (client *Client) Send(ctx context.Context, request Request) error {
	payload, err := request.payload()
	if err != nil {
		return err
	}
	return client.link.Send(ctx, payload)
}

func (request Request) payload() ([]byte, error) {
	if request.Service&0x40 != 0 {
		return nil, fmt.Errorf("UDS request service %#02x cannot have a positive response service ID", request.Service)
	}
	payload := make([]byte, 1+len(request.Data))
	payload[0] = byte(request.Service)
	copy(payload[1:], request.Data)
	return payload, nil
}

func parseResponse(service ServiceID, payload []byte) (Response, *NegativeResponseError, error) {
	if len(payload) == 0 {
		return Response{}, nil, fmt.Errorf("%w: payload is empty", ErrInvalidResponse)
	}
	if payload[0] == 0x7f {
		if len(payload) != 3 {
			return Response{}, nil, fmt.Errorf("%w: negative response has %d bytes, want 3", ErrInvalidResponse, len(payload))
		}
		code := ResponseCode(payload[2])
		// ResponsePending is classified before the service echo is validated:
		// several fielded ECU stacks echo a stale service ID on 0x78. A pending
		// carries no data and the exchange scope leaves only one request it can
		// belong to, so leniency here cannot misattribute a response. Final
		// responses keep the strict echo check below.
		if code == responsePending {
			return Response{}, &NegativeResponseError{Service: service, Code: code}, nil
		}
		if ServiceID(payload[1]) != service {
			return Response{}, nil, fmt.Errorf("%w: negative response identifies service %#02x, want %#02x", ErrUnexpectedResponse, payload[1], service)
		}
		return Response{}, &NegativeResponseError{Service: service, Code: code}, nil
	}

	expected := byte(service) + 0x40
	if payload[0] != expected {
		return Response{}, nil, fmt.Errorf("%w: positive response identifies service %#02x, want %#02x", ErrUnexpectedResponse, payload[0], expected)
	}
	return Response{Service: service, Data: payload[1:]}, nil, nil
}

func nextWithTimeout(ctx context.Context, exchange *isotp.Exchange, timeout time.Duration, timeoutError error) ([]byte, error) {
	payload, err := exchange.Next(ctx, timeout)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return nil, timeoutError
	}
	return payload, err
}

func configuredTimeout(value, defaultValue time.Duration, name string) (time.Duration, error) {
	if value < 0 {
		return 0, fmt.Errorf("UDS %s timeout must not be negative", name)
	}
	if value == 0 {
		return defaultValue, nil
	}
	return value, nil
}
