package drivers

import (
	"context"

	"github.com/tomrford/gocan"
)

type socketcanPrepared struct {
	id    gocan.BusID
	name  string
	iface string
	fd    bool
}

var socketcanDriver = driverDescriptor{
	name: "socketcan",
	formatIdentifier: func(channel Channel) string {
		return "socketcan:" + channel.nativeName
	},
	prepare: func(channel Channel, prepared preparedConfig) (preparedOpen, error) {
		if prepared.config.Vendor != nil {
			return nil, rejectVendorOptions(channel, prepared.config.Vendor)
		}
		native := socketcanPrepared{
			id:    prepared.config.ID,
			name:  prepared.config.Name,
			iface: channel.nativeName,
			fd:    channel.supportsFD,
		}
		return func(ctx context.Context, capture *gocan.Capture) (gocan.Bus, error) {
			return openSocketCAN(ctx, capture, native)
		}, nil
	},
	discover: discoverSocketCAN,
}
