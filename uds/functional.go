package uds

import (
	"context"
	"errors"
	"time"

	"github.com/tomrford/gocan/isotp"
)

// FunctionalConfig sets send-only UDS timing. Zero P3Client selects no extra
// gap, matching historical behavior.
type FunctionalConfig struct {
	// P3Client is the ISO 14229-2 minimum time after a completed request
	// before this client starts another. After a successful transmit, Send
	// waits any remaining gap before returning so a following physical
	// request is also spaced.
	P3Client time.Duration
}

// Functional sends UDS requests over one functionally addressed ISO-TP path.
// Functional requests are send-only: the typed operations suppress positive
// responses, and servers that answer do so on their own physical addresses.
type Functional struct {
	link *isotp.Functional
	p3   requestGap
}

// NewFunctional validates config and binds a send-only UDS client to link.
func NewFunctional(link *isotp.Functional, config FunctionalConfig) (*Functional, error) {
	if link == nil {
		return nil, errors.New("UDS functional client requires an ISO-TP functional path")
	}
	p3Client, err := configuredDuration(config.P3Client, "P3 client")
	if err != nil {
		return nil, err
	}
	return &Functional{link: link, p3: requestGap{gap: p3Client}}, nil
}

// Send transmits one raw request to every server on the functional address.
// It is intended for a request whose service data already contains
// suppressPositiveResponse. After a successful transmit it waits any remaining
// P3Client gap before returning.
func (functional *Functional) Send(ctx context.Context, request Request) error {
	payload, err := request.payload()
	if err != nil {
		return err
	}
	if err := functional.p3.wait(ctx); err != nil {
		return err
	}
	if err := functional.link.Send(ctx, payload); err != nil {
		return err
	}
	functional.p3.finish(ctx)
	return nil
}

// SendTesterPresent broadcasts tester present with its positive response
// suppressed.
func (functional *Functional) SendTesterPresent(ctx context.Context) error {
	return functional.Send(ctx, Request{Service: ServiceTesterPresent, Data: []byte{suppressPositiveResponse}})
}

// SendCommunicationControl broadcasts controlType for the messages selected by
// communicationType with its positive response suppressed.
func (functional *Functional) SendCommunicationControl(ctx context.Context, controlType CommunicationControlType, communicationType CommunicationType) error {
	if err := validateCommunicationControl(controlType, communicationType); err != nil {
		return err
	}
	return functional.Send(ctx, Request{
		Service: ServiceCommunicationControl,
		Data:    []byte{byte(controlType) | suppressPositiveResponse, byte(communicationType)},
	})
}

// SendControlDTCSetting broadcasts settingType with an optional control option
// record and its positive response suppressed.
func (functional *Functional) SendControlDTCSetting(ctx context.Context, settingType DTCSettingType, optionRecord []byte) error {
	if err := validateSubfunction(byte(settingType), "DTC setting type"); err != nil {
		return err
	}
	data := make([]byte, 1+len(optionRecord))
	data[0] = byte(settingType) | suppressPositiveResponse
	copy(data[1:], optionRecord)
	return functional.Send(ctx, Request{Service: ServiceControlDTCSetting, Data: data})
}
