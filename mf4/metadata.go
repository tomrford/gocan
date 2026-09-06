package mf4

import (
	"encoding/xml"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"
)

// MDF comments use the standard HDcomment/FHcomment XML vocabulary. Property
// names and values are application-owned; the writer only validates and escapes
// them. See asammdf's HeaderBlock.comment in the pinned reference.
func (options Options) comments() (header, history string, err error) {
	for _, value := range []string{options.ToolName, options.ToolVersion, options.Comment} {
		if !validXMLText(value) {
			return "", "", errors.New("MF4 metadata requires valid UTF-8 and XML 1.0 text")
		}
	}
	keys := make([]string, 0, len(options.Properties))
	for key, value := range options.Properties {
		if key == "" || !validXMLText(key) || !validXMLText(value) {
			return "", "", errors.New("MF4 properties require nonempty keys and valid UTF-8 and XML 1.0 text")
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	const namespace = "http://www.asam.net/mdf/v4"
	if options.Comment != "" || len(keys) != 0 {
		type property struct {
			Name  string `xml:"name,attr"`
			Value string `xml:",chardata"`
		}
		comment := struct {
			XMLName    xml.Name
			Text       string     `xml:"TX"`
			Properties []property `xml:"common_properties>e"`
		}{XMLName: xml.Name{Space: namespace, Local: "HDcomment"}, Text: options.Comment}
		for _, key := range keys {
			comment.Properties = append(comment.Properties, property{key, options.Properties[key]})
		}
		data, err := xml.Marshal(comment)
		if err != nil {
			return "", "", err
		}
		header = string(data)
	}
	toolName := options.ToolName
	if toolName == "" {
		toolName = "gocan"
	}
	comment := struct {
		XMLName     xml.Name
		Text        string `xml:"TX"`
		ToolID      string `xml:"tool_id"`
		ToolVendor  string `xml:"tool_vendor"`
		ToolVersion string `xml:"tool_version"`
	}{XMLName: xml.Name{Space: namespace, Local: "FHcomment"}, Text: "Exported raw CAN capture", ToolID: toolName, ToolVersion: options.ToolVersion}
	data, err := xml.Marshal(comment)
	return header, string(data), err
}

// encoding/xml replaces invalid characters with U+FFFD; reject them first so
// caller-supplied metadata cannot silently change during export.
func validXMLText(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsFunc(value, func(r rune) bool {
		return r < 0x20 && r != '\t' && r != '\n' && r != '\r' || r == 0xfffe || r == 0xffff
	})
}
