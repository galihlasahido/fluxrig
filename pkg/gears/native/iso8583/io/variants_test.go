// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

package io

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jaab-tech/fluxrig/pkg/fluxmsg"
	"github.com/jaab-tech/fluxrig/pkg/idgen"
)

// The framing variants seen on real ISO 8583 links: ASCII or binary length (big
// or little endian), a protocol header such as a TPDU (EDC terminals), and a
// trailer such as ETX (0x03). These tests drive a real Server over TCP with raw
// bytes and check the exact wire form in both directions.

type msgSink struct {
	mu   sync.Mutex
	msgs []*fluxmsg.FluxMsg
}

func (s *msgSink) emit(m *fluxmsg.FluxMsg) {
	s.mu.Lock()
	s.msgs = append(s.msgs, m)
	s.mu.Unlock()
}

func (s *msgSink) wait(t *testing.T, n int) *fluxmsg.FluxMsg {
	t.Helper()
	waitFor(t, "emitted message", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.msgs) >= n
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msgs[n-1]
}

func startVariantServer(t *testing.T, mutate func(*Config)) (*Server, *msgSink) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Mode = ModeServer
	cfg.Bind = "127.0.0.1:0"
	cfg.ReadTimeout = Duration(2 * time.Second)
	cfg.WriteTimeout = Duration(2 * time.Second)
	cfg.IdleTimeout = Duration(10 * time.Second)
	mutate(&cfg)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	gen, err := idgen.New(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	sink := &msgSink{}
	srv := NewServer(&cfg, slog.New(slog.DiscardHandler), sink.emit, gen)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return srv, sink
}

func dial(t *testing.T, srv *Server) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", srv.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func readN(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("reading %d reply bytes: %v", n, err)
	}
	return buf
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// An ISO 8583 body is opaque to the io gear; this one starts like an ASCII 0800.
var body = []byte("0800822000000000000004000000000000000102030405000042301")

func TestVariants_LengthPrefix(t *testing.T) {
	n := len(body) // 55
	cases := []struct {
		name   string
		mutate func(*Config)
		prefix []byte
	}{
		{"ascii 4 digits", func(c *Config) { c.FrameLengthEncoding = FrameEncodingASCIIDecimal; c.FrameLengthSize = 4 }, []byte("0055")},
		{"binary 2 bytes big endian", func(c *Config) { c.FrameLengthSize = 2; c.FrameLengthEndian = EndianBig }, []byte{0x00, 0x37}},
		{"binary 2 bytes little endian", func(c *Config) { c.FrameLengthSize = 2; c.FrameLengthEndian = EndianLittle }, []byte{0x37, 0x00}},
	}
	if n != 0x37 {
		t.Fatalf("test body is %d bytes, cases assume 55", n)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, sink := startVariantServer(t, tc.mutate)
			c := dial(t, srv)
			wire := append(append([]byte{}, tc.prefix...), body...)
			if _, err := c.Write(wire); err != nil {
				t.Fatal(err)
			}
			m := sink.wait(t, 1)
			if !bytes.Equal(m.RawPayload, body) {
				t.Fatalf("payload = %q, want the body without the prefix", m.RawPayload)
			}
			if _, err := srv.Process(context.Background(), m); err != nil {
				t.Fatal(err)
			}
			if got := readN(t, c, len(wire)); !bytes.Equal(got, wire) {
				t.Fatalf("reply on the wire = %x, want %x", got, wire)
			}
		})
	}
}

func TestVariants_Trailer(t *testing.T) {
	for _, inLength := range []bool{false, true} {
		name := "length excludes trailer"
		prefix := []byte{0x00, 0x37}
		if inLength {
			name = "length includes trailer"
			prefix = []byte{0x00, 0x38}
		}
		t.Run(name, func(t *testing.T) {
			srv, sink := startVariantServer(t, func(c *Config) {
				c.FrameLengthSize = 2
				c.FrameTrailer = "03"
				c.FrameTrailerInLength = inLength
			})
			c := dial(t, srv)
			wire := append(append(append([]byte{}, prefix...), body...), 0x03)
			if _, err := c.Write(wire); err != nil {
				t.Fatal(err)
			}
			m := sink.wait(t, 1)
			if !bytes.Equal(m.RawPayload, body) {
				t.Fatalf("payload = %q, want the body without prefix and trailer", m.RawPayload)
			}
			if _, err := srv.Process(context.Background(), m); err != nil {
				t.Fatal(err)
			}
			if got := readN(t, c, len(wire)); !bytes.Equal(got, wire) {
				t.Fatalf("reply on the wire = %x, want %x", got, wire)
			}
		})
	}
}

func TestVariants_TrailerMismatchClosesConnection(t *testing.T) {
	srv, sink := startVariantServer(t, func(c *Config) {
		c.FrameLengthSize = 2
		c.FrameTrailer = "03"
	})
	c := dial(t, srv)
	bad := append(append([]byte{0x00, 0x37}, body...), 0x04) // ETX expected
	if _, err := c.Write(bad); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection stayed open after a frame with a wrong trailer")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.msgs) != 0 {
		t.Fatalf("emitted %d messages for a frame with a wrong trailer", len(sink.msgs))
	}
}

func TestVariants_ConfigRejectsBadTrailer(t *testing.T) {
	for _, tr := range []string{"zz", "030", "010203040506070809"} {
		cfg := DefaultConfig()
		cfg.Mode, cfg.Bind, cfg.FrameTrailer = ModeServer, ":0", tr
		if err := cfg.Validate(); err == nil {
			t.Errorf("frame_trailer %q accepted", tr)
		}
	}
}

// An EDC-style link: little endian length, a 5 byte TPDU (60 DDDD SSSS) before
// the message and an ETX after it. The TPDU is not part of the message the
// scenario sees; it is kept in metadata and, on the reply, comes back with the
// two addresses swapped.
func TestVariants_TPDUAndTrailer(t *testing.T) {
	for _, swap := range []bool{true, false} {
		name := "swap on"
		if !swap {
			name = "swap off"
		}
		t.Run(name, func(t *testing.T) {
			srv, sink := startVariantServer(t, func(c *Config) {
				c.FrameLengthSize = 2
				c.FrameLengthEndian = EndianLittle
				c.TPDUEnabled = true
				c.TPDUSwap = swap
				c.PreserveHeaders = true
				c.FrameTrailer = "03"
			})
			c := dial(t, srv)
			tpdu := mustHex("6000030001") // dest 0003, src 0001
			frame := func(h []byte) []byte {
				n := len(h) + len(body)
				out := []byte{byte(n), byte(n >> 8)}
				out = append(out, h...)
				out = append(out, body...)
				return append(out, 0x03)
			}
			if _, err := c.Write(frame(tpdu)); err != nil {
				t.Fatal(err)
			}
			m := sink.wait(t, 1)
			if !bytes.Equal(m.RawPayload, body) {
				t.Fatalf("payload = %q, want the message without TPDU", m.RawPayload)
			}
			if got := m.Metadata["iso8583.raw_header"]; got != "hex:6000030001" {
				t.Fatalf("raw_header = %q", got)
			}
			if _, err := srv.Process(context.Background(), m); err != nil {
				t.Fatal(err)
			}
			want := frame(tpdu)
			if swap {
				want = frame(mustHex("6000010003"))
			}
			if got := readN(t, c, len(want)); !bytes.Equal(got, want) {
				t.Fatalf("reply on the wire = %x, want %x", got, want)
			}
		})
	}
}

func TestSwapTPDU(t *testing.T) {
	if got := swapTPDU(mustHex("60AABBCCDD"), 5); !bytes.Equal(got, mustHex("60CCDDAABB")) {
		t.Errorf("swap = %x", got)
	}
	in := mustHex("60AABBCCDD")
	_ = swapTPDU(in, 5)
	if !bytes.Equal(in, mustHex("60AABBCCDD")) {
		t.Error("swapTPDU modified its input")
	}
	if got := swapTPDU(mustHex("60AABB"), 5); !bytes.Equal(got, mustHex("60AABB")) {
		t.Errorf("short header changed: %x", got)
	}
	if got := swapTPDU(mustHex("60AABBCCDD"), 7); !bytes.Equal(got, mustHex("60AABBCCDD")) {
		t.Errorf("non-5 byte tpdu_length must leave the header alone: %x", got)
	}
}

// Client mode reads the same frames. The preserved header is binary (bytes >=
// 0x80 here), so it must be kept hex-encoded for the message to stay valid.
func TestVariants_ClientPreservesBinaryHeaderAndStripsTrailer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cfg := DefaultConfig()
	cfg.Mode = ModeClient
	cfg.Connect = l.Addr().String()
	cfg.FrameLengthSize = 2
	cfg.TPDUEnabled = true
	cfg.PreserveHeaders = true
	cfg.FrameTrailer = "03"
	cfg.ReconnectWait = Duration(100 * time.Millisecond)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	gen, _ := idgen.New(uuid.New())
	sink := &msgSink{}
	cl := NewClient(&cfg, slog.New(slog.DiscardHandler), sink.emit, gen)
	if err := cl.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Stop() })

	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	n := 5 + len(body)
	wire := append([]byte{byte(n >> 8), byte(n)}, mustHex("60F0F1F2F3")...)
	wire = append(append(wire, body...), 0x03)
	if _, err := conn.Write(wire); err != nil {
		t.Fatal(err)
	}
	m := sink.wait(t, 1)
	if !bytes.Equal(m.RawPayload, body) {
		t.Fatalf("payload = %q", m.RawPayload)
	}
	if got := m.Metadata["iso8583.raw_header"]; got != "hex:60f0f1f2f3" {
		t.Fatalf("raw_header = %q, want hex:60f0f1f2f3", got)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("message with a preserved binary header is not valid: %v", err)
	}
}

// Data elements may be ASCII text or packed bytes (BCD digits, a binary bitmap).
// The io gear does not parse them (codec_iso8583 does, per field, from the spec),
// but it reads the MTI and the bitmap to validate and describe each frame, so both
// forms must pass that check with the matching `encoding`.
func TestVariants_AsciiAndPackedElementsAreRecognised(t *testing.T) {
	cases := []struct {
		name     string
		encoding string
		frame    []byte
	}{
		{"ascii mti, hex-ascii bitmap, ascii elements", EncodingASCII,
			[]byte("0800" + "2220000000000000" + "180000" + "0102030405" + "000042")},
		{"bcd mti, binary bitmap, bcd elements", EncodingBCD,
			mustHex("0800" + "2220000000000000" + "180000" + "0102030405" + "000042")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, sink := startVariantServer(t, func(c *Config) { c.Encoding = tc.encoding })
			c := dial(t, srv)
			wire := append([]byte{0x00, byte(len(tc.frame))}, tc.frame...)
			if _, err := c.Write(wire); err != nil {
				t.Fatal(err)
			}
			m := sink.wait(t, 1)
			if m.Metadata["iso8583.mti"] != "0800" || m.Metadata["iso8583.bitmaps"] != "1" || m.Metadata["iso8583.fields"] != "[3,7,11]" {
				t.Fatalf("metadata mti=%q bitmaps=%q fields=%q", m.Metadata["iso8583.mti"], m.Metadata["iso8583.bitmaps"], m.Metadata["iso8583.fields"])
			}
			if !bytes.Equal(m.RawPayload, tc.frame) {
				t.Fatalf("payload altered: %x", m.RawPayload)
			}
		})
	}
}
