// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"testing"
	"time"

	"github.com/moov-io/iso8583"
)

func roundTrip(t *testing.T, framing int, little bool) {
	t.Helper()
	spec := getEchoSpec()
	makeResponderSpec(spec)
	cfg := &isoResponderConfig{spec: spec, framingSize: framing, littleEnd: little, idleTimeout: 5 * time.Second, maxFrame: 4096}

	srv, cli := net.Pipe()
	defer func() { _ = cli.Close() }()
	go serveISO(srv, cfg)
	_ = cli.SetDeadline(time.Now().Add(5 * time.Second))

	req := iso8583.NewMessage(spec)
	req.MTI("0800")
	if err := req.Field(7, "0102030405"); err != nil {
		t.Fatal(err)
	}
	if err := req.Field(11, "000042"); err != nil {
		t.Fatal(err)
	}
	if err := req.Field(70, "301"); err != nil {
		t.Fatal(err)
	}
	payload, err := req.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.writeFrame(cli, payload); err != nil {
		t.Fatal(err)
	}
	body, err := cfg.readFrame(cli)
	if err != nil {
		t.Fatal(err)
	}
	resp := iso8583.NewMessage(spec)
	if err := resp.Unpack(body); err != nil {
		t.Fatal(err)
	}
	want := map[int]string{7: "0102030405", 11: "000042", 39: "00", 70: "301"}
	for id, v := range want {
		if got, _ := resp.GetString(id); got != v {
			t.Errorf("DE%d = %q, want %q", id, got, v)
		}
	}
	if mti, _ := resp.GetMTI(); mti != "0810" {
		t.Errorf("MTI = %q, want 0810", mti)
	}

	// A non-0800 gets no answer, and the connection stays usable.
	other := iso8583.NewMessage(spec)
	other.MTI("0810")
	_ = other.Field(11, "000001")
	p2, _ := other.Pack()
	if err := cfg.writeFrame(cli, p2); err != nil {
		t.Fatal(err)
	}
	if err := cfg.writeFrame(cli, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.readFrame(cli); err != nil {
		t.Fatalf("second 0800 after ignored frame: %v", err)
	}
}

func TestISOResponderRoundTrip(t *testing.T) {
	old := *encodingFlag
	defer func() { *encodingFlag = old }()
	for _, enc := range []string{"bcd", "ascii"} {
		*encodingFlag = enc
		t.Run(enc, roundTripFramings)
	}
}

func roundTripFramings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frame  int
		little bool
	}{{"2-big", 2, false}, {"4-big", 4, false}, {"2-little", 2, true}} {
		t.Run(tc.name, func(t *testing.T) { roundTrip(t, tc.frame, tc.little) })
	}
}

func TestISOResponderRejectsOversizeFrame(t *testing.T) {
	cfg := &isoResponderConfig{spec: getEchoSpec(), framingSize: 2, maxFrame: 4, idleTimeout: time.Second}
	srv, cli := net.Pipe()
	defer func() { _ = cli.Close() }()
	done := make(chan struct{})
	go func() { serveISO(srv, cfg); close(done) }()
	_ = cli.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = cli.Write([]byte{0x00, 0x10})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("responder did not close on oversize frame")
	}
}
