// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

package codec

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jaab-tech/fluxrig/pkg/fluxmsg"
)

// jPOS ISO87APackager compatibility of examples/specs/iso8583-v87-ascii.yaml.
// The human-readable record is examples/specs/iso8583-v87-ascii-jpos-compat.md.
//
// Provenance of the jPOS side. jPOS 2.1.6 (the version the jpos-example modules
// depend on) is not in the local Maven repository; jPOS 2.0.0 is. The field table
// and the wire strings below come from running org.jpos.iso.packager.ISO87APackager
// out of jpos-2.0.0.jar: packager classes and lengths read by reflection from
// ISOBasePackager.fld, wire strings from ISOMsg.pack(). That the 2.1.6 packager
// defines the same fields is from memory, not verified here.
//
// The examples frame with ASCIIChannel (4 ASCII digits of length before each
// message). That is channel behaviour, not packager behaviour, and is not tested.

const (
	shippedSpecPath = "../../../../../examples/specs/iso8583-v87-ascii.yaml"
	compatSpecPath  = "../../../../../examples/specs/iso8583-v87-ascii-jpos.yaml"
)

func initJposGear(t *testing.T, specPath string) *Gear {
	t.Helper()
	g := &Gear{}
	ctx := &mockGearContext{
		config: map[string]any{"spec_path": specPath, "direction": "auto"},
		logger: slog.Default(),
	}
	require.NoError(t, g.Init(ctx))
	return g
}

// fieldFact pairs what jPOS declares with what the spec must declare to agree on
// the wire. pref is the moov prefixer: ASCII.Fixed or ASCII.LLL (jPOS IFA_LLLCHAR).
// enc is the value encoder (DE3 has none in the YAML; the gear defaults it to ASCII).
type fieldFact struct {
	id        int
	jposClass string
	jposLen   int
	goType    string
	pref      string
	enc       string
}

var jposFacts = []fieldFact{
	{0, "IFA_NUMERIC", 4, "String", "ASCII.Fixed", "asciiEncoder"},
	{3, "IFA_NUMERIC", 6, "Numeric", "ASCII.Fixed", "asciiEncoder"},
	{7, "IFA_NUMERIC", 10, "String", "ASCII.Fixed", "asciiEncoder"},
	{11, "IFA_NUMERIC", 6, "String", "ASCII.Fixed", "asciiEncoder"},
	{12, "IFA_NUMERIC", 6, "String", "ASCII.Fixed", "asciiEncoder"},
	{39, "IF_CHAR", 2, "String", "ASCII.Fixed", "asciiEncoder"},
	{41, "IF_CHAR", 8, "String", "ASCII.Fixed", "asciiEncoder"},
	{63, "IFA_LLLCHAR", 999, "String", "ASCII.LLL", "asciiEncoder"},
	{70, "IFA_NUMERIC", 3, "String", "ASCII.Fixed", "asciiEncoder"},
}

func shortType(v any) string {
	s := fmt.Sprintf("%T", v)
	return s[strings.LastIndex(s, ".")+1:]
}

func checkFacts(t *testing.T, g *Gear) {
	t.Helper()
	for _, ff := range jposFacts {
		f, ok := g.moovSpec.Fields[ff.id]
		if !assert.Truef(t, ok, "DE%d (jPOS %s %d) not defined in spec", ff.id, ff.jposClass, ff.jposLen) {
			continue
		}
		s := f.Spec()
		assert.Equalf(t, ff.goType, shortType(f), "DE%d type (jPOS %s)", ff.id, ff.jposClass)
		assert.Equalf(t, ff.jposLen, s.Length, "DE%d length (jPOS %s)", ff.id, ff.jposClass)
		assert.Equalf(t, ff.pref, s.Pref.Inspect(), "DE%d prefix", ff.id)
		enc := ""
		if s.Enc != nil {
			enc = shortType(s.Enc)
		}
		assert.Equalf(t, ff.enc, enc, "DE%d encoder", ff.id)
	}
	bm := g.moovSpec.Fields[1].Spec()
	assert.Equal(t, "Bitmap", shortType(g.moovSpec.Fields[1]))
	assert.Equal(t, "hexToASCIIEncoder", shortType(bm.Enc))
	assert.Equal(t, "Hex.Fixed", bm.Pref.Inspect())
}

// MTI and DE3, 7, 11, 12, 39, 41, 63, 70 agree with jPOS in both specs; only the
// bitmap length differs. jPOS writes a bitmap as 16 hex characters (8 bytes).
// moov builds a bitmap of Length bytes, so the shipped Length of 16 gives 16 bytes,
// 32 hex characters.
func TestJposISO87A_FieldDefinitions(t *testing.T) {
	t.Run("shipped", func(t *testing.T) {
		g := initJposGear(t, shippedSpecPath)
		checkFacts(t, g)
		assert.Equal(t, 16, g.moovSpec.Fields[1].Spec().Length, "KNOWN MISMATCH: jPOS bitmap is 8 bytes (16 hex characters)")
	})
	t.Run("compat", func(t *testing.T) {
		g := initJposGear(t, compatSpecPath)
		checkFacts(t, g)
		assert.Equal(t, 8, g.moovSpec.Fields[1].Spec().Length)
	})
}

// The compat spec is the shipped spec with one line added to wire.fields.
func TestJposISO87A_CompatSpecIsShippedPlusOneLine(t *testing.T) {
	shipped, err := os.ReadFile(shippedSpecPath)
	require.NoError(t, err)
	compat, err := os.ReadFile(compatSpecPath)
	require.NoError(t, err)
	const zero = "      0: {padding: {type: Left, pad: \"0\"}}\n"
	// Each file opens with its own comment block; compare from the first key on.
	cut := func(b []byte) string {
		s := string(b)
		i := strings.Index(s, "\nspec:")
		require.GreaterOrEqual(t, i, 0)
		return s[i:]
	}
	want := strings.Replace(cut(shipped), zero, "      1: {length: 8}\n"+zero, 1)
	assert.Equal(t, want, cut(compat), "iso8583-v87-ascii-jpos.yaml drifted from iso8583-v87-ascii.yaml")
}

// Wire strings packed by ISO87APackager (jpos-2.0.0.jar). Bitmaps are hex ASCII,
// 16 characters each; the 0800 carries DE70 so it has a second bitmap, announced
// by bit 1 of the first.
const (
	jposWire0800 = "0800822000000000000004000000000000000102030405000042301"
	jposWire0200 = "0200201000000280000200000012345600TERM0001003ABC"
)

var (
	jposKV0800 = map[int]string{7: "0102030405", 11: "000042", 70: "301"}
	jposKV0200 = map[int]string{3: "000000", 12: "123456", 39: "00", 41: "TERM0001", 63: "ABC"}
)

func encodeJpos(t *testing.T, g *Gear, mti string, kv map[int]string) string {
	t.Helper()
	msg := fluxmsg.New()
	msg.Metadata["iso8583.mti"] = mti
	for id, v := range kv {
		require.NoError(t, msg.Set(fmt.Sprintf("iso8583.field.%d", id), v))
	}
	out, err := g.Process(context.Background(), msg)
	require.NoError(t, err)
	return string(out.RawPayload)
}

// decodeJpos returns the decoded message, or nil when the gear rejected it.
func decodeJpos(g *Gear, wire string) *fluxmsg.FluxMsg {
	msg := fluxmsg.New()
	msg.RawPayload = []byte(wire)
	out, err := g.Process(context.Background(), msg)
	if err != nil || out == nil || out.Metadata["iso8583.mti"] == "" {
		return nil
	}
	return out
}

// KNOWN MISMATCH in the shipped spec: it packs a 16-byte bitmap (32 hex
// characters) and does not set bit 1 unless a second bitmap is needed, so a
// message with only DE1-64 is 16 characters longer than jPOS's and a jPOS peer
// reads the second bitmap as data. For DE70 the length agrees but bit 1 is clear.
func TestJposISO87A_Pack(t *testing.T) {
	for _, c := range []struct {
		name, mti, wire string
		kv              map[int]string
		extra           int
	}{{"0800", "0800", jposWire0800, jposKV0800, 0}, {"0200", "0200", jposWire0200, jposKV0200, 16}} {
		t.Run(c.name+"/shipped", func(t *testing.T) {
			got := encodeJpos(t, initJposGear(t, shippedSpecPath), c.mti, c.kv)
			t.Logf("fluxrig: %s", got)
			t.Logf("jPOS   : %s", c.wire)
			assert.NotEqual(t, c.wire, got, "KNOWN MISMATCH: bitmap")
			assert.Len(t, got, len(c.wire)+c.extra)
		})
		t.Run(c.name+"/compat", func(t *testing.T) {
			got := encodeJpos(t, initJposGear(t, compatSpecPath), c.mti, c.kv)
			assert.Equal(t, c.wire, got, "compat spec must pack byte-for-byte like jPOS")
		})
	}
}

func TestJposISO87A_UnpackJposBytes(t *testing.T) {
	t.Run("shipped", func(t *testing.T) {
		g := initJposGear(t, shippedSpecPath)
		assert.Nil(t, decodeJpos(g, jposWire0800), "KNOWN MISMATCH: jPOS 0800 does not unpack")
		assert.Nil(t, decodeJpos(g, jposWire0200), "KNOWN MISMATCH: jPOS 0200 does not unpack")
	})
	t.Run("compat", func(t *testing.T) {
		g := initJposGear(t, compatSpecPath)
		for _, c := range []struct {
			mti, wire string
			kv        map[int]string
		}{{"0800", jposWire0800, jposKV0800}, {"0200", jposWire0200, jposKV0200}} {
			out := decodeJpos(g, c.wire)
			require.NotNil(t, out, "jPOS %s must unpack with the compat spec", c.mti)
			assert.Equal(t, c.mti, out.Metadata["iso8583.mti"])
			for id, want := range c.kv {
				got, _ := out.Get(fmt.Sprintf("iso8583.field.%d", id))
				if fmt.Sprint(got) != want {
					// KNOWN ISSUE, independent of the bitmap: the spec's left-padding
					// (wire.fields padding) makes moov strip leading zeros on unpack.
					t.Logf("KNOWN ISSUE %s DE%d unpacked as %q, jPOS value %q", c.mti, id, got, want)
				}
			}
			// Fields without padding are exact.
			for _, id := range []int{39, 41, 63} {
				if want, ok := c.kv[id]; ok {
					got, _ := out.Get(fmt.Sprintf("iso8583.field.%d", id))
					assert.Equal(t, want, fmt.Sprint(got), "DE%d", id)
				}
			}
		}
	})
}

// The spec defines only part of ISO87A. Fields jPOS defines that the spec does not
// make an inbound message fail to unpack. The count is exact so any change is seen.
func TestJposISO87A_UndefinedFields(t *testing.T) {
	g := initJposGear(t, shippedSpecPath)
	var missing []int
	for id := 2; id <= 128; id++ {
		if _, ok := g.moovSpec.Fields[id]; !ok {
			missing = append(missing, id)
		}
	}
	for _, ff := range jposFacts {
		assert.NotContains(t, missing, ff.id, "DE%d is used by the jPOS examples", ff.id)
	}
	t.Logf("jPOS ISO87A fields the spec does not define (%d): %v", len(missing), missing)
	assert.Len(t, missing, 61)
}

// Short values. jPOS (jpos-2.0.0.jar) packs DE3="0" as "000000" (numeric, left
// zero-filled) and DE41="T1" as "T1      " (character, right space-filled). The
// compat spec zero-fills numerics the same way; DE41 has no padding declared.
func TestJposISO87A_ShortValues(t *testing.T) {
	g := initJposGear(t, compatSpecPath)
	msg := fluxmsg.New()
	msg.Metadata["iso8583.mti"] = "0200"
	require.NoError(t, msg.Set("iso8583.field.3", "0"))
	out, err := g.Process(context.Background(), msg)
	require.NoError(t, err)
	assert.Equal(t, "02002000000000000000"+"000000", string(out.RawPayload), "DE3 short value, as jPOS")

	msg = fluxmsg.New()
	msg.Metadata["iso8583.mti"] = "0200"
	require.NoError(t, msg.Set("iso8583.field.41", "T1"))
	out, err = g.Process(context.Background(), msg)
	if err != nil || out == nil || len(out.RawPayload) == 0 {
		t.Logf("KNOWN DIFFERENCE: DE41 \"T1\" is rejected (err=%v); jPOS packs \"T1      \"", err)
		return
	}
	t.Logf("DE41 \"T1\" packs as %q; jPOS packs \"T1      \"", out.RawPayload)
}
