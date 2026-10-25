// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/hex"
	"os"
	"testing"

	"github.com/moov-io/iso8583"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jaab-tech/fluxrig/pkg/gears/native/iso8583/codec/sdl"
)

// The decode mode prints a fixed-length field at the width the wire carried,
// as the codec gear does. The library strips padding on unpack, so without
// the restore STAN 000001 printed as "1" and the MTI field as "100". A
// variable-length field keeps the value the sender sent.
func TestDecodeHexKeepsFixedWidth(t *testing.T) {
	content, err := os.ReadFile("../../examples/specs/iso8583-v87-ascii.yaml")
	require.NoError(t, err)

	spec, _, err := sdl.LoadSpecContent(content, "")
	require.NoError(t, err)
	sdl.MakeNilSafe(spec)
	m := iso8583.NewMessage(spec)
	require.NoError(t, m.Field(0, "0100"))
	require.NoError(t, m.Field(2, "0012345678"))
	require.NoError(t, m.Field(11, "000001"))
	packed, err := m.Pack()
	require.NoError(t, err)

	got, err := decodeHex(content, hex.EncodeToString(packed))
	require.NoError(t, err)

	for key, want := range map[string]string{
		"iso8583.mti":      "0100",
		"iso8583.field.0":  "0100",
		"iso8583.field.11": "000001",
		"iso8583.field.2":  "0012345678",
	} {
		assert.Equal(t, want, got[key], key)
	}
}
