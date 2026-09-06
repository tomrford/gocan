package xcp

import (
	"context"
	"fmt"
)

// Address is a protocol address, not a host pointer. Extension is ECU-defined
// and is sent unchanged. Typed reads currently require byte-addressed memory.
type Address struct {
	Extension byte
	Value     uint32
}

// Read sets the ECU's memory transfer address and issues sequential UPLOADs.
// Returned bytes contain exactly the successfully received prefix, including on
// error. There is no implicit retry or snapshot guarantee across packets.
func (client *Client) Read(ctx context.Context, address Address, length int) ([]byte, error) {
	return client.read(ctx, address, length, false)
}

// ShortUpload reads one response's worth of memory using an address-bearing
// SHORT_UPLOAD. It returns ErrUnsupported if the count needs multiple replies;
// use Read for larger ranges. ECU support for this optional command is required.
func (client *Client) ShortUpload(ctx context.Context, address Address, length int) ([]byte, error) {
	return client.read(ctx, address, length, true)
}

func (client *Client) read(ctx context.Context, address Address, length int, short bool) ([]byte, error) {
	// A transfer ending at 2^32 would wrap the ECU's incremented MTA as well.
	if length < 0 || uint64(address.Value)+uint64(length) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("XCP read address/count overflow")
	}
	ctx, finish, err := client.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	if client.pending != 0 {
		return nil, ErrSynchronizationRequired
	}
	if !client.connected {
		return nil, ErrNotConnected
	}
	if client.capabilities.AddressGranularity != 1 {
		return nil, fmt.Errorf("%w: memory reads require byte address granularity", ErrUnsupported)
	}
	maximum := int(client.capabilities.MaxCTO) - 1
	if short && length > maximum {
		return nil, fmt.Errorf("%w: SHORT_UPLOAD count exceeds one response", ErrUnsupported)
	}
	if length == 0 {
		return []byte{}, nil
	}
	parameters := make([]byte, 7)
	parameters[2] = address.Extension
	client.capabilities.ByteOrder.PutUint32(parameters[3:], address.Value)
	if short {
		parameters[0] = byte(length)
		response, err := client.command(ctx, Request{Command: CommandShortUpload, Data: parameters})
		if err != nil {
			return nil, err
		}
		return response.Data[:length], nil
	}
	if _, err := client.command(ctx, Request{Command: CommandSetMTA, Data: parameters}); err != nil {
		return nil, err
	}
	// Grow only as replies arrive: a large requested range does not allocate
	// memory before the ECU has supplied it.
	var result []byte
	for len(result) < length {
		count := min(maximum, length-len(result))
		response, err := client.command(ctx, Request{Command: CommandUpload, Data: []byte{byte(count)}})
		if err != nil {
			return result, err
		}
		result = append(result, response.Data[:count]...)
	}
	return result, nil
}
