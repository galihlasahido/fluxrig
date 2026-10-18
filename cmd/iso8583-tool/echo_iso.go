// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"github.com/moov-io/iso8583"
	"github.com/moov-io/iso8583/field"
	"github.com/moov-io/iso8583/prefix"
)

// isoResponderConfig drives the ISO8583 responder of echo mode. Framing is a
// binary length prefix (2 or 4 bytes). ASCII-decimal length framing is not handled
// here; it is added separately and composes with this: only readFrame and
// writeFrame need to learn the new prefix.
type isoResponderConfig struct {
	spec        *iso8583.MessageSpec
	framingSize int
	littleEnd   bool
	idleTimeout time.Duration
	maxFrame    int
}

// makeResponderSpec turns the load generator's pack-only 0800 template (built by
// getEchoSpec) into one that can also unpack, and adds the DE39 a 0810 needs.
//
//   - The template's fields use prefix.None.Fixed, whose DecodeLength claims all
//     remaining bytes, so the MTI would swallow the frame. Fixed-width fields get
//     prefix.ASCII.Fixed instead (the same patch decode mode applies); the Fixed
//     prefixers carry no bytes on the wire, so packing is unchanged.
//   - Left-padding is dropped: moov-io strips it on unpack, which turns an all-zero
//     value such as STAN "000000" into "" and panics. The responder only copies
//     values that arrived at full width.
//   - The bitmap is rebuilt with an explicit 8-byte primary; a secondary follows
//     automatically when bit 1 is set (DE70 needs it).
func makeResponderSpec(spec *iso8583.MessageSpec) {
	for _, f := range spec.Fields {
		f.Spec().Pad = nil
		f.Spec().Pref = prefix.ASCII.Fixed
	}
	bm := *spec.Fields[1].Spec()
	bm.Length = 8
	spec.Fields[1] = field.NewBitmap(&bm)
	spec.Fields[39] = field.NewString(&field.Spec{
		Length:      2,
		Description: "Response Code",
		Enc:         spec.Fields[7].Spec().Enc,
		Pref:        prefix.ASCII.Fixed,
	})
}

func (c *isoResponderConfig) byteOrder() binary.ByteOrder {
	if c.littleEnd {
		return binary.LittleEndian
	}
	return binary.BigEndian
}

func (c *isoResponderConfig) readFrame(conn net.Conn) ([]byte, error) {
	if c.idleTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(c.idleTimeout))
	}
	hdr := make([]byte, c.framingSize)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return nil, err
	}
	var n uint64
	if c.framingSize == 2 {
		n = uint64(c.byteOrder().Uint16(hdr))
	} else {
		n = uint64(c.byteOrder().Uint32(hdr))
	}
	if n > uint64(c.maxFrame) {
		return nil, fmt.Errorf("frame of %d bytes exceeds -iso-max-frame %d", n, c.maxFrame)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, err
	}
	return body, nil
}

func (c *isoResponderConfig) writeFrame(conn net.Conn, payload []byte) error {
	buf := make([]byte, c.framingSize+len(payload))
	if c.framingSize == 2 {
		//nolint:gosec // payload len fits
		c.byteOrder().PutUint16(buf, uint16(len(payload)))
	} else {
		//nolint:gosec // payload len fits
		c.byteOrder().PutUint32(buf, uint32(len(payload)))
	}
	copy(buf[c.framingSize:], payload)
	_, err := conn.Write(buf)
	return err
}

// buildResponse turns an 0800 into an 0810: every field of the request is copied,
// the MTI is flipped and DE39 is set to 00. It returns (nil, nil) for any MTI it
// does not answer.
func (c *isoResponderConfig) buildResponse(body []byte) (resp []byte, err error) {
	defer func() {
		// moov-io can panic on malformed input; a bad frame must not kill the tool.
		if r := recover(); r != nil {
			resp, err = nil, fmt.Errorf("panic while handling frame: %v", r)
		}
	}()
	req := iso8583.NewMessage(c.spec)
	if err := req.Unpack(body); err != nil {
		return nil, fmt.Errorf("unpack: %w", err)
	}
	mti, err := req.GetMTI()
	if err != nil {
		return nil, err
	}
	if mti != "0800" {
		return nil, nil
	}
	out := iso8583.NewMessage(c.spec)
	out.MTI("0810")
	set := req.GetFields()
	for id := range set {
		if id == 0 || id == 1 {
			continue
		}
		str, err := req.GetString(id)
		if err != nil {
			return nil, fmt.Errorf("read DE%d: %w", id, err)
		}
		if err := out.Field(id, str); err != nil {
			return nil, fmt.Errorf("copy DE%d: %w", id, err)
		}
	}
	if err := out.Field(39, "00"); err != nil {
		return nil, fmt.Errorf("set DE39: %w", err)
	}
	return out.Pack()
}

func serveISO(conn net.Conn, c *isoResponderConfig) {
	defer func() { _ = conn.Close() }()
	for {
		body, err := c.readFrame(conn)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.Printf("iso responder %s: %v", conn.RemoteAddr(), err)
			}
			return
		}
		resp, err := c.buildResponse(body)
		if err != nil {
			log.Printf("iso responder %s: %v", conn.RemoteAddr(), err)
			continue
		}
		if resp == nil {
			continue
		}
		if err := c.writeFrame(conn, resp); err != nil {
			log.Printf("iso responder %s: write: %v", conn.RemoteAddr(), err)
			return
		}
	}
}
