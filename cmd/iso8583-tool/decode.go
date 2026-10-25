// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/moov-io/iso8583"
	"github.com/moov-io/iso8583/encoding"
	"github.com/moov-io/iso8583/prefix"

	"github.com/jaab-tech/fluxrig/pkg/gears/native/iso8583/codec/sdl"
)

func runDecodeMode() {
	if *decodeMessage == "" {
		log.Fatalf("decode mode requires -message flag with hex-encoded ISO8583 message")
	}
	if *decodeSpec == "" {
		log.Fatalf("decode mode requires -spec flag with path to SDL spec file")
	}

	// Load spec
	content, err := os.ReadFile(*decodeSpec)
	if err != nil {
		log.Fatalf("Failed to read spec file: %v", err)
	}

	result, err := decodeHex(content, *decodeMessage)
	if err != nil {
		log.Fatalf("%v", err)
	}

	// Output as JSON
	jsonOut, _ := json.Marshal(result)
	fmt.Println(string(jsonOut))
}

// decodeHex decodes a hex-encoded message with the spec in content and
// returns the MTI and every present field by key. A fixed-length field comes
// back at the width the wire carried, as the codec gear reports it.
func decodeHex(content []byte, msgHex string) (_ map[string]string, err error) {
	// Load moov spec
	moovSpec, _, err := sdl.LoadSpecContent(content, "")
	if err != nil {
		return nil, fmt.Errorf("failed to load moov spec: %w", err)
	}

	// Patch spec with ASCII encoding (like fluxrig decoder does).
	// Deliberately never assigns Pad: moov unpads on unpack whenever
	// Pad is set, which would strip leading zeros ("0110"->"110",
	// "00"->""). This tool only unpacks, so it needs no pack padding.
	for _, fld := range moovSpec.Fields {
		if fld == nil {
			continue
		}
		spec := fld.Spec()
		if spec == nil {
			continue
		}
		if spec.Enc == nil {
			spec.Enc = encoding.ASCII
		}
		if spec.Pref == nil {
			spec.Pref = prefix.ASCII.Fixed
		}
		for _, subField := range spec.Subfields {
			subSpec := subField.Spec()
			if subSpec != nil {
				if subSpec.Enc == nil {
					subSpec.Enc = encoding.ASCII
				}
				if subSpec.Pref == nil {
					subSpec.Pref = prefix.ASCII.Fixed
				}
			}
		}
	}

	// Decode hex message
	msgBytes, err := hex.DecodeString(msgHex)
	if err != nil {
		return nil, fmt.Errorf("failed to decode hex message: %w", err)
	}

	// Remove length prefix if present (2 bytes)
	if len(msgBytes) >= 2 {
		length := int(msgBytes[0])<<8 | int(msgBytes[1])
		if length == len(msgBytes)-2 {
			msgBytes = msgBytes[2:]
		}
	}

	// Decode using moov-io/iso8583
	isoMsg := iso8583.NewMessage(moovSpec)
	if isoMsg == nil {
		return nil, fmt.Errorf("failed to create isoMsg from moovSpec")
	}

	// The codec gear recovers a panic inside Unpack (a malformed or adversarial
	// payload can trigger one in the moov-io library); this tool must too, or a
	// user pointing it at bad input gets a raw stack trace instead of the clean
	// error every other failure path here already reports.
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic while unpacking message: %v", r)
			}
		}()
		if unpackErr := isoMsg.Unpack(msgBytes); unpackErr != nil {
			err = fmt.Errorf("failed to unpack message: %w", unpackErr)
		}
	}()
	if err != nil {
		return nil, err
	}

	// Extract fields
	result := make(map[string]string)

	// Get MTI
	mti, _ := isoMsg.GetString(0)
	mti = sdl.FixedWidth(isoMsg.GetField(0), mti)
	if len(mti) > 0 {
		for len(mti) < 4 {
			mti = "0" + mti
		}
		result["iso8583.mti"] = mti
	}

	// Get bitmap
	bitmap := isoMsg.Bitmap()
	if bitmap != nil {
		for i := 0; i <= 128; i++ {
			if i > 0 && !bitmap.IsSet(i) {
				continue
			}
			val, getErr := isoMsg.GetString(i)
			if getErr != nil {
				continue
			}
			result[fmt.Sprintf("iso8583.field.%d", i)] = sdl.FixedWidth(isoMsg.GetField(i), val)
		}
	}

	return result, nil
}
