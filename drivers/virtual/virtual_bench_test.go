package virtual

import (
	"context"
	"testing"

	"github.com/tomrford/gocan"
)

// BenchmarkSendToPeerCapture measures Send through the peer's captured reception.
func BenchmarkSendToPeerCapture(b *testing.B) {
	const clearInterval = 1 << 18

	ctx := context.Background()
	capture := gocan.NewCapture()
	var network Network

	sender, err := network.Open(ctx, capture, Config{ID: 1, Name: "sender"})
	if err != nil {
		b.Fatalf("Open sender: %v", err)
	}
	defer sender.Close()
	receiver, err := network.Open(ctx, capture, Config{ID: 2, Name: "receiver"})
	if err != nil {
		b.Fatalf("Open receiver: %v", err)
	}
	defer receiver.Close()

	frame, err := gocan.NewFrame(0x123, []byte{1, 2, 3, 4, 5, 6, 7, 8}, 0)
	if err != nil {
		b.Fatalf("NewFrame: %v", err)
	}
	key := gocan.FrameKey{Bus: receiver.ID(), ID: frame.ID, Direction: gocan.DirectionReceive}
	cursor := capture.End()

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if err := sender.Send(ctx, frame); err != nil {
			b.Fatalf("Send: %v", err)
		}
		_, next, err := capture.Next(ctx, key, cursor)
		if err != nil {
			b.Fatalf("Next: %v", err)
		}
		cursor = next
		if i%clearInterval == clearInterval-1 {
			capture.Clear()
			cursor = capture.End()
		}
	}
}
