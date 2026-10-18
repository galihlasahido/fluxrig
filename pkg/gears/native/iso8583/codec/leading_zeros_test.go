// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

package codec

import (
	"context"
	"log/slog"
	"testing"

	"github.com/moov-io/iso8583/field"
	"github.com/moov-io/iso8583/padding"
	"github.com/moov-io/iso8583/prefix"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jaab-tech/fluxrig/pkg/fluxmsg"
)

func newSpecGear(t *testing.T, specPath string) *Gear {
	t.Helper()
	g := &Gear{}
	require.NoError(t, g.Init(&mockGearContext{
		config: map[string]any{"spec_path": specPath, "direction": "auto"},
		logger: slog.Default(),
	}))
	return g
}

// moov trims every leading pad character on unpack, so a spec's left padding
// used to turn the wire value "000042" into "42" and "000000" into "". The codec
// restores the declared fixed length, so pack -> unpack is lossless.
func TestFixedLeftPaddedFieldsRoundTripKeepLeadingZeros(t *testing.T) {
	g := newSpecGear(t, "../../../../../examples/specs/iso8583-v87-ascii.yaml")

	cases := []struct {
		name      string
		de7, de11 string
	}{
		{"leading zeros", "0102030405", "000042"},
		{"all zero", "0000000000", "000000"},
		{"no leading zero", "1231235959", "123456"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := fluxmsg.New()
			in.Metadata["iso8583.mti"] = "0200"
			require.NoError(t, in.Set("iso8583.field.7", c.de7))
			require.NoError(t, in.Set("iso8583.field.11", c.de11))
			enc, err := g.Process(context.Background(), in)
			require.NoError(t, err)
			assert.Contains(t, string(enc.RawPayload), c.de7+c.de11)

			dec := fluxmsg.New()
			dec.RawPayload = enc.RawPayload
			out, err := g.Process(context.Background(), dec)
			require.NoError(t, err)

			v7, _ := out.Get("iso8583.field.7")
			v11, _ := out.Get("iso8583.field.11")
			assert.Equal(t, c.de7, v7, "DE7")
			assert.Equal(t, c.de11, v11, "DE11")
			assert.Equal(t, "0200", out.Metadata["iso8583.mti"])
		})
	}
}

// A variable-length field keeps no padding on decode: the length prefix says how
// long the value is, and a leading zero in it is data.
func TestVariableLengthPaddedFieldIsNotRepadded(t *testing.T) {
	g := newSpecGear(t, "../../../../../test/robot/suites/conductor/specs/auth.yaml")

	in := fluxmsg.New()
	in.Metadata["iso8583.mti"] = "0200"
	require.NoError(t, in.Set("iso8583.field.2", "4111111111111111"))
	enc, err := g.Process(context.Background(), in)
	require.NoError(t, err)

	dec := fluxmsg.New()
	dec.RawPayload = enc.RawPayload
	out, err := g.Process(context.Background(), dec)
	require.NoError(t, err)
	pan, _ := out.Get("iso8583.field.2")
	assert.Equal(t, "4111111111111111", pan)
}

func TestRestoreFixedPadding(t *testing.T) {
	left := padding.NewLeftPadder('0')
	right := padding.NewRightPadder('0')
	spec := func(pad padding.Padder, pref prefix.Prefixer, n int) *field.Spec {
		return &field.Spec{Length: n, Pad: pad, Pref: pref}
	}
	assert.Equal(t, "000042", restoreFixedPadding("42", spec(left, prefix.ASCII.Fixed, 6)))
	assert.Equal(t, "000000", restoreFixedPadding("", spec(left, prefix.ASCII.Fixed, 6)))
	assert.Equal(t, "000042", restoreFixedPadding("000042", spec(left, prefix.ASCII.Fixed, 6)))
	assert.Equal(t, "42", restoreFixedPadding("42", spec(left, prefix.ASCII.LL, 6)), "variable length")
	assert.Equal(t, "42", restoreFixedPadding("42", spec(right, prefix.ASCII.Fixed, 6)), "right pad")
	assert.Equal(t, "42", restoreFixedPadding("42", spec(nil, prefix.ASCII.Fixed, 6)), "no pad")
	assert.Equal(t, "42", restoreFixedPadding("42", nil), "no spec")
	// The pad character comes from the spec.
	assert.Equal(t, "xx42", restoreFixedPadding("42", spec(padding.NewLeftPadder('x'), prefix.ASCII.Fixed, 4)))
}
