package virtual

import (
	"context"
	"testing"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/conformance"
)

func TestConformance(t *testing.T) {
	open := func(t *testing.T, capture *gocan.Capture) (gocan.Bus, gocan.Bus) {
		var network Network
		a, err := network.Open(context.Background(), capture, Config{ID: 1, Name: "conformance-a"})
		if err != nil {
			t.Fatalf("Open a: %v", err)
		}
		t.Cleanup(func() { _ = a.Close() })
		b, err := network.Open(context.Background(), capture, Config{ID: 2, Name: "conformance-b"})
		if err != nil {
			t.Fatalf("Open b: %v", err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return a, b
	}

	conformance.Run(t, open, conformance.Capabilities{
		FD:           true,
		RemoteFrames: true,
		InduceOverrun: func(_ *testing.T, _, target gocan.Bus) {
			target.(*Bus).fail(gocan.ErrReceiveOverrun)
		},
	})
}
