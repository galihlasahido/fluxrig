// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

package codec

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jaab-tech/fluxrig/pkg/fluxmsg"
)

// Documents, against the shipped reference spec, what happens to a field that
// the spec's wire.fields declares with `padding: {type: Left, pad: "0"}`.
//
// Root cause: the padding entry is imported into moov's field.Spec.Pad, and moov
// (field/packer_unpacker.go, DefaultUnpacker) calls Pad.Unpad on every unpack,
// which trims ALL leading pad characters regardless of the declared fixed
// length. The wire bytes are correct ("000042"); the value handed to the FluxMsg
// is "42", and an all-zero value ("000000") becomes "". The codec's own MTI
// handling re-pads DE0 for the same reason (gear.go, decode).
//
// This test pins the CURRENT behaviour so a change is a conscious decision:
// existing flows (and TestProcessorSimAlignment) correlate on the unpadded value.
// If decode is changed to restore the fixed length, update the "got" values here
// to the wire values.
func TestPaddedFixedFieldsUnpackWithoutLeadingZeros(t *testing.T) {
	g := &Gear{}
	require.NoError(t, g.Init(&mockGearContext{
		config: map[string]any{
			"spec_path": "../../../../../examples/specs/iso8583-v87-ascii.yaml",
			"direction": "auto",
		},
		logger: slog.Default(),
	}))

	cases := []struct {
		name        string
		de7, de11   string
		got7, got11 string
	}{
		{"padded", "0102030405", "000042", "102030405", "42"},
		{"all zero", "0000000000", "000000", "", ""},
		{"no leading zero", "1231235959", "123456", "1231235959", "123456"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := fluxmsg.New()
			in.Metadata["iso8583.mti"] = "0200"
			require.NoError(t, in.Set("iso8583.field.7", c.de7))
			require.NoError(t, in.Set("iso8583.field.11", c.de11))
			enc, err := g.Process(context.Background(), in)
			require.NoError(t, err)

			// The wire carries the zeros...
			assert.Contains(t, string(enc.RawPayload), c.de7+c.de11)

			dec := fluxmsg.New()
			dec.RawPayload = enc.RawPayload
			out, err := g.Process(context.Background(), dec)
			require.NoError(t, err)

			// ...the decoded values do not.
			v7, _ := out.Get("iso8583.field.7")
			v11, _ := out.Get("iso8583.field.11")
			assert.Equal(t, c.got7, v7, "DE7")
			assert.Equal(t, c.got11, v11, "DE11")
			// MTI is re-padded by the codec itself.
			assert.Equal(t, "0200", out.Metadata["iso8583.mti"])
		})
	}
}
