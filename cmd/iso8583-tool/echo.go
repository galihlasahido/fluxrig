// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"io"
	"log"
	"net"
	"os"

	"github.com/jaab-tech/fluxrig/pkg/gears/native/iso8583/codec/sdl"
	"time"
)

var (
	port = flag.String("port", "8590", "Port to listen on (Echo Mode)")
	sink = flag.Bool("sink", false, "Sink mode (discard data instead of echo)")

	isoResponder   = flag.Bool("iso", false, "Echo mode: act as an ISO8583 responder (answer 0800 with 0810) instead of a raw TCP echo. Uses -framing-bytes, -endian and -encoding.")
	isoSpecPath    = flag.String("iso-spec", "", "Echo mode with -iso: path to the SDL spec. Without it, the built-in 0800 echo spec is used (DE 0/1/7/11/39/70).")
	isoIdleTimeout = flag.Duration("iso-idle-timeout", 5*time.Minute, "Echo mode with -iso: close a connection that sends nothing for this long")
	isoMaxFrame    = flag.Int("iso-max-frame", 1<<20, "Echo mode with -iso: largest accepted frame body in bytes")
)

func runEchoMode() {
	// Log to stdout
	log.SetOutput(os.Stdout)

	addr := ":" + *port

	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Error listening: %v", err)
	}
	defer func() { _ = l.Close() }()

	var cfg *isoResponderConfig
	if *isoResponder {
		spec := getEchoSpec()
		if *isoSpecPath != "" {
			loaded, _, err := sdl.LoadSpec(*isoSpecPath)
			if err != nil {
				log.Fatalf("echo: cannot read spec %s: %v", *isoSpecPath, err)
			}
			spec = loaded
		} else {
			makeResponderSpec(spec)
		}
		cfg = &isoResponderConfig{
			spec:        spec,
			framingSize: *framingBytes,
			littleEnd:   *endian == "little",
			idleTimeout: *isoIdleTimeout,
			maxFrame:    *isoMaxFrame,
		}
		log.Printf("ISO8583 responder listening on %s (framing=%d bytes, endian=%s, encoding=%s)", addr, cfg.framingSize, *endian, *encodingFlag)
	} else {
		log.Printf("TCP Echo Server listening on %s (Sink: %v)", addr, *sink)
	}

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("Error accepting: %v", err)
			continue
		}
		if cfg != nil {
			go serveISO(conn, cfg)
			continue
		}
		go handleRequest(conn, *sink)
	}
}

func handleRequest(conn net.Conn, sink bool) {
	defer func() { _ = conn.Close() }()
	if sink {
		// Discard everything (Blackhole)
		_, _ = io.Copy(io.Discard, conn)
	} else {
		// Echo everything
		_, _ = io.Copy(conn, conn)
	}
}
