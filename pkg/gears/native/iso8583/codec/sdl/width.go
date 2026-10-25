// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

package sdl

import (
	"strings"

	"github.com/moov-io/iso8583/field"
)

// FixedWidth gives a decoded fixed-length field back the width the wire
// carried. The library strips a field's padding when it unpacks, so DE 11
// "000042" reads as "42" and an all-zero DE 3 reads as "0". The wire
// re-encodes either one correctly, but a decoder that reports the trimmed
// value hands a different string to a rule or a translation that compares the
// fixed-width value. A variable-length field keeps the value as unpacked: its
// width is whatever the sender chose.
//
// Anything that reports decoded values calls this, the codec gear and the
// iso8583-tool decode mode alike, so both print the same value.
func FixedWidth(f field.Field, val string) string {
	// Text fields only. A Binary or Hex field renders its bytes as hex, two
	// characters per byte, while its Length counts bytes: padding that string
	// to Length would mix the two units and change the bytes a re-encode
	// produces. A composite is handled through its own subfields.
	switch f.(type) {
	case *field.String, *field.Numeric:
	default:
		return val
	}
	spec := f.Spec()
	if spec == nil || spec.Pad == nil || spec.Pref == nil || spec.Length <= 0 {
		return val
	}
	if !strings.HasSuffix(spec.Pref.Inspect(), ".Fixed") || len(val) >= spec.Length {
		return val
	}
	return string(spec.Pad.Pad([]byte(val), spec.Length))
}
