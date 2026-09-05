package cdd_test

import (
	"fmt"

	"github.com/tomrford/gocan/cdd"
)

func ExampleDocument_Select() {
	document, err := cdd.ParseFile("testdata/catalog.cdd")
	if err != nil {
		panic(err)
	}
	// Select from document.ECUs and their Variants. Zero means automatic only
	// when that level has exactly one choice; indexes are one-based.
	catalog, err := document.Select(cdd.Selection{ECU: 1, Variant: 1})
	if err != nil {
		panic(err)
	}
	for _, entry := range catalog.Entries {
		if entry.Class != nil {
			fmt.Println("Class:", entry.Class.DisplayName("en-GB"))
		} else {
			fmt.Println("Instance:", entry.Instance.DisplayName("en-GB"))
		}
	}
	did, ok := catalog.DIDByName("Measurement")
	if !ok {
		panic("missing or ambiguous DID")
	}
	// Choose a service alternative explicitly. This record codec accepts the
	// data returned by uds.Client.ReadDataByIdentifier, after the identifier.
	service := did.Read[0]
	if service.Err != nil || service.PositiveResponse == nil || service.PositiveResponse.Err != nil {
		panic("unsupported response")
	}
	record := service.PositiveResponse.Record
	if err := record.CodecError(); err != nil {
		panic(err)
	}
	values, err := record.Decode([]byte{42})
	if err != nil {
		panic(err)
	}
	fmt.Println("Value:", values["Value"])
	// Output:
	// Class: Controls
	// Instance: StandaloneReset
	// Class: Measurements
	// Value: 42
}
