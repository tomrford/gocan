package uds

import (
	"context"
	"errors"

	"github.com/tomrford/gocan/isotp"
)

// Functional sends UDS requests over one functionally addressed ISO-TP path.
// Functional requests are send-only: the typed operations suppress positive
// responses, and servers that answer do so on their own physical addresses.
type Functional struct {
	link *isotp.Functional
}

// NewFunctional binds a send-only UDS client to link.
func NewFunctional(link *isotp.Functional) (*Functional, error) {
	if link == nil {
		return nil, errors.New("UDS functional client requires an ISO-TP functional path")
	}
	return &Functional{link: link}, nil
}

// Send transmits one raw request to every server on the functional address.
// It is intended for a request whose service data already contains
// suppressPositiveResponse.
func (functional *Functional) Send(ctx context.Context, request Request) error {
	payload, err := request.payload()
	if err != nil {
		return err
	}
	return functional.link.Send(ctx, payload)
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
