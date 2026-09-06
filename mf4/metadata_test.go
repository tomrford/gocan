package mf4_test

import (
	"encoding/binary"
	"encoding/xml"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/mf4"
)

func TestExportMetadata(t *testing.T) {
	f := newFile(t)
	properties := map[string]string{"boost.workspace.id": "w<&\"42", "boost.pack.é<&\".version": "1.2\nβ", "empty": ""}
	wantProperties := maps.Clone(properties)
	busNames := map[gocan.BusID]string{1: "Powertrain <A> & B", 2: "Diagnostics"}
	w, err := mf4.NewWriter(f, start, mf4.Options{
		Compression: true, ToolName: "Boost <test> & export", ToolVersion: "1.2+β",
		Comment: "Cooling test\nWorkspace <A> & B", Properties: properties, BusNames: busNames,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Groups are created lazily. Caller mutations after construction must not
	// change the exported names, even for buses first seen later.
	busNames[1], busNames[2], properties["boost.workspace.id"] = "changed", "changed", "changed"
	if err := w.WriteEvent(gocan.Event{Bus: 2, Timestamp: start, Kind: gocan.EventReceiveOverrun}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFrame(frame(1, 0, 0x123, 0, 42)); err != nil {
		t.Fatal(err)
	}
	remote, err := gocan.NewRemoteFrame(0x123, 8, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFrame(gocan.FrameEvent{Bus: 1, Timestamp: start.Add(time.Second), Direction: gocan.DirectionReceive, Frame: remote}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	checkGroups(t, f, []uint64{1, 1})
	file := readImage(t, f)
	var header struct {
		XMLName    xml.Name `xml:"http://www.asam.net/mdf/v4 HDcomment"`
		Text       string   `xml:"TX"`
		Properties []struct {
			Name  string `xml:"name,attr"`
			Value string `xml:",chardata"`
		} `xml:"common_properties>e"`
	}
	if err := xml.Unmarshal([]byte(file.text(file.u64(128))), &header); err != nil {
		t.Fatal(err)
	}
	gotProperties := make(map[string]string)
	for _, property := range header.Properties {
		gotProperties[property.Name] = property.Value
	}
	if header.Text != "Cooling test\nWorkspace <A> & B" || !maps.Equal(gotProperties, wantProperties) {
		t.Fatalf("header metadata changed: %+v", header)
	}
	var history struct {
		XMLName     xml.Name `xml:"http://www.asam.net/mdf/v4 FHcomment"`
		ToolName    string   `xml:"tool_id"`
		ToolVersion string   `xml:"tool_version"`
	}
	if err := xml.Unmarshal([]byte(file.text(file.u64(file.u64(96)+32))), &history); err != nil ||
		history.ToolName != "Boost <test> & export" || history.ToolVersion != "1.2+β" {
		t.Fatalf("producer metadata changed: %+v, %v", history, err)
	}
	for cg := file.u64(file.u64(88) + 32); cg != 0; cg = file.u64(cg + 24) {
		source := file.u64(cg + 48)
		if file.text(file.u64(source+24)) != "Powertrain <A> & B" || file.text(file.u64(source+32)) != "CAN1" {
			t.Fatal("display name changed bus identity or was not snapshotted")
		}
	}
	if file.text(file.u64(file.u64(120)+48)) != "CAN2 (Diagnostics) receive overrun" {
		t.Fatal("event-only bus lost its name or numeric identity")
	}
}

func TestMetadataValidation(t *testing.T) {
	for name, options := range map[string]mf4.Options{
		"tool":          {ToolName: "invalid\x00name"},
		"version":       {ToolVersion: "invalid\xff"},
		"comment":       {Comment: "invalid\ufffe"},
		"empty key":     {Properties: map[string]string{"": "value"}},
		"invalid key":   {Properties: map[string]string{"key\x01": "value"}},
		"invalid value": {Properties: map[string]string{"key": "value\x00"}},
		"bus ID":        {BusNames: map[gocan.BusID]string{0: "name"}},
		"bus name":      {BusNames: map[gocan.BusID]string{1: "name\xff"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFile(t)
			if _, err := mf4.NewWriter(f, start, options); err == nil {
				t.Fatal("accepted lossy or invalid metadata")
			}
			info, err := f.Stat()
			if err != nil || info.Size() != 0 {
				t.Fatal("invalid metadata modified output", err)
			}
		})
	}
}

func TestMetadataDefaults(t *testing.T) {
	f := newFile(t)
	w, err := mf4.NewWriter(f, start, mf4.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint64(file[128:]) != 0 ||
		!strings.Contains(string(file), "<tool_id>gocan</tool_id>") ||
		!strings.Contains(string(file), "<tool_version></tool_version>") {
		t.Fatal("default metadata invented an application version or measurement comment")
	}
}
