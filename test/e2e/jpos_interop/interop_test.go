// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

package jposinterop

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A: jpos-client (TestRunner, 20+1 echoes) -> fluxrig jpos_style_server_respond.
func TestA_JposClientToFluxrigServer(t *testing.T) {
	prepare(t)
	srv := freePort(t)
	noLeftovers(t, srv)
	startFluxrig(t, scenarioText(t, "jpos_style_server_respond.yaml", map[string]int{"8888": srv}))

	// jpos-client dials 8889 and the simulator sends 0800/180000 echoes (21 in all).
	cl := startJpos(t, "jpos-client", map[string]int{"8889": srv}, fixEchoExpectation)
	cl.waitExit(waitTimeout())

	out := cl.output()
	replies := count(parseISOMsgs(out, "receive"), isReply)
	if replies != 21 {
		t.Fatalf("jPOS client received %d 0810/DE39=00 replies, want 21\n%s", replies, tail(out, 3000))
	}
	if ok, bad := strings.Count(out, "[OK]"), strings.Count(out, "[FAILURE]"); ok != 21 || bad != 0 {
		t.Errorf("jPOS client simulator results: %d OK, %d FAILURE, want 21 OK, 0 FAILURE", ok, bad)
	}
	if strings.Contains(out, "Exception") {
		t.Errorf("jPOS client logged an exception:\n%s", tail(out, 3000))
	}
}

// B: fluxrig iso8583_periodic_echo (bento generator -> client) -> jpos-server.
func TestB_FluxrigClientToJposServer(t *testing.T) {
	prepare(t)
	srv := freePort(t)
	noLeftovers(t, srv)
	js := startJpos(t, "jpos-server", map[string]int{"8888": srv}, nil)
	poll(t, waitTimeout(), "jpos-server listening", func() bool { return portOpen(srv) })

	// The example echoes every 10s; the test shortens it so that several echoes
	// are seen. Nothing else changes.
	sc := scenarioText(t, "iso8583_periodic_echo.yaml", map[string]int{"8583": srv})
	if !strings.Contains(sc, `interval: "10s"`) {
		t.Fatal("scenario no longer has the interval the test rewrites")
	}
	sc = strings.Replace(sc, `interval: "10s"`, `interval: "1s"`, 1)
	f := startFluxrig(t, sc)

	const want = 3
	poll(t, waitTimeout(), "fluxrig to log echo replies", func() bool {
		return strings.Count(f.rackLog()+f.rack.output(), "echo reply mti=0810") >= want
	})
	log := f.rackLog() + f.rack.output()
	if !strings.Contains(log, "rc=00") {
		t.Errorf("no echo reply with rc=00 in the Rack log")
	}

	// Wire evidence from the real jPOS server: what it unpacked and what it sent.
	poll(t, 10*time.Second, "jPOS server to log its replies", func() bool {
		return count(parseISOMsgs(js.output(), "send"), isReply) >= want
	})
	rx := parseISOMsgs(js.output(), "receive")
	if n := count(rx, func(m isoMsg) bool {
		return m["0"] == "0800" && m["3"] == "180000" && len(m["7"]) == 10 && len(m["11"]) == 6
	}); n < want {
		t.Errorf("jPOS server saw %d well-formed 0800/180000 echoes, want >= %d: %v", n, want, rx)
	}
}

// echoScenario is iso8583_periodic_echo.yaml aimed at port, echoing every second.
func echoScenario(t *testing.T, port int) string {
	t.Helper()
	sc := scenarioText(t, "iso8583_periodic_echo.yaml", map[string]int{"8583": port})
	return replaceScenario(t, sc, `interval: "10s"`, `interval: "1s"`)
}

// C1: fluxrig client -> jpos-server-forwarding (:8889) -> jpos-server (:8888).
// Then the upstream goes away and the jPOS forwarder must answer DE39=68.
func TestC1_FluxrigClientToJposForwarding(t *testing.T) {
	prepare(t)
	up, fwd := freePort(t), freePort(t)
	noLeftovers(t, up, fwd)
	ports := map[string]int{"8888": up, "8889": fwd}
	server := startJpos(t, "jpos-server", ports, nil)
	poll(t, waitTimeout(), "jpos-server listening", func() bool { return portOpen(up) })
	forwarder := startJpos(t, "jpos-server-forwarding", ports, func(dir string) {
		// The forwarder waits 30s for the upstream reply before it answers 68;
		// 2s keeps the test short. Only this participant's timeout changes.
		replaceOnce(t, filepath.Join(dir, "deploy", "50_token-trx-manager.xml"),
			`<property name="timeout" value="30000" />`, `<property name="timeout" value="2000" />`)
	})
	poll(t, waitTimeout(), "forwarder listening", func() bool { return portOpen(fwd) })

	f := startFluxrig(t, echoScenario(t, fwd))
	poll(t, waitTimeout(), "echoes answered 00 through the forwarder", func() bool { return rcCount(f, "00") >= 3 })
	poll(t, 10*time.Second, "the upstream jpos-server to log the forwarded echoes", func() bool {
		return count(parseISOMsgs(server.output(), "receive"), func(m isoMsg) bool { return m["0"] == "0800" && m["3"] == "180000" }) >= 3
	})

	// Upstream down: the forwarder must now answer 68 to the fluxrig client.
	server.stop()
	poll(t, waitTimeout(), "the forwarder to answer 68", func() bool { return rcCount(f, "68") >= 1 })
	if n := count(parseISOMsgs(forwarder.output(), "send"), func(m isoMsg) bool { return m["0"] == "0810" && m["39"] == "68" }); n < 1 {
		t.Errorf("the jPOS forwarder log shows no 0810 with DE39=68")
	}
}

// jposClientKey: the jPOS simulator's echo has no DE41, so the scenario's
// correlation key (DE41 + DE11) would reject every request with reason no_key.
// DE7 + DE11 identifies a jPOS echo.
const (
	keyDefault = `correlation_key: ["iso8583.field.41", "iso8583.field.11"]`
	keyJpos    = `correlation_key: ["iso8583.field.7", "iso8583.field.11"]`
)

func forwardingScenario(t *testing.T, fwd, up int, ttl string) string {
	t.Helper()
	sc := scenarioText(t, "jpos_style_forwarding.yaml", map[string]int{"8888": up, "8889": fwd})
	sc = replaceScenario(t, sc, keyDefault, keyJpos)
	if ttl != "" {
		sc = replaceScenario(t, sc, `default_ttl: "30s"`, `default_ttl: "`+ttl+`"`)
	}
	return sc
}

// C2: jpos-client -> fluxrig jpos_style_forwarding (:8889) -> jpos-server (:8888).
func TestC2_JposClientToFluxrigForwarding(t *testing.T) {
	prepare(t)
	up, fwd := freePort(t), freePort(t)
	noLeftovers(t, up, fwd)
	server := startJpos(t, "jpos-server", map[string]int{"8888": up}, nil)
	poll(t, waitTimeout(), "jpos-server listening", func() bool { return portOpen(up) })
	f := startFluxrig(t, forwardingScenario(t, fwd, up, ""))
	poll(t, waitTimeout(), "uplink connected", func() bool { return strings.Contains(f.rackLog(), "uplink_io") && portOpen(up) })

	cl := startJpos(t, "jpos-client", map[string]int{"8889": fwd}, clientEdit(0))
	cl.waitExit(waitTimeout())
	out := cl.output()
	if n := count(parseISOMsgs(out, "receive"), isReply); n != 21 {
		t.Fatalf("jPOS client got %d 0810/DE39=00 replies through fluxrig, want 21\n%s", n, tail(out, 2000))
	}
	if ok, bad := strings.Count(out, "[OK]"), strings.Count(out, "[FAILURE]"); ok != 21 || bad != 0 {
		t.Errorf("simulator results: %d OK, %d FAILURE, want 21/0", ok, bad)
	}
	if n := count(parseISOMsgs(server.output(), "receive"), func(m isoMsg) bool { return m["0"] == "0800" }); n != 21 {
		t.Errorf("jpos-server saw %d forwarded 0800, want 21", n)
	}
}

// C3: the decline paths of the fluxrig forwarder, driven by the jPOS client.
// Upstream down (nothing listens): conductor reports no_destination -> DE39=91.
// Upstream black hole (accepts, never answers): ticket TTL -> timeout -> DE39=68.
func TestC3_FluxrigForwardingDeclines(t *testing.T) {
	prepare(t)
	t.Run("upstream_down_91", func(t *testing.T) {
		up, fwd := freePort(t), freePort(t)
		noLeftovers(t, fwd)
		startFluxrig(t, forwardingScenario(t, fwd, up, ""))
		cl := startJpos(t, "jpos-client", map[string]int{"8889": fwd}, clientEdit(2))
		cl.waitExit(waitTimeout())
		if n := count(parseISOMsgs(cl.output(), "receive"), func(m isoMsg) bool { return m["0"] == "0810" && m["39"] == "91" }); n != 3 {
			t.Fatalf("got %d 0810/DE39=91 replies, want 3\n%s", n, tail(cl.output(), 2500))
		}
	})
	t.Run("upstream_silent_68", func(t *testing.T) {
		up, fwd := freePort(t), freePort(t)
		noLeftovers(t, up, fwd)
		start(t, "sink", t.TempDir(), nil, filepath.Join(binDir, "iso8583-tool"), "-mode", "echo", "-port", fmt.Sprint(up), "-sink")
		poll(t, waitTimeout(), "sink listening", func() bool { return portOpen(up) })
		startFluxrig(t, forwardingScenario(t, fwd, up, "2s"))
		cl := startJpos(t, "jpos-client", map[string]int{"8889": fwd}, clientEdit(2))
		cl.waitExit(waitTimeout())
		if n := count(parseISOMsgs(cl.output(), "receive"), func(m isoMsg) bool { return m["0"] == "0810" && m["39"] == "68" }); n != 3 {
			t.Fatalf("got %d 0810/DE39=68 replies, want 3\n%s", n, tail(cl.output(), 2500))
		}
	})
}

// D: fluxrig jpos_style_multi_port (two servers) <- two jpos-client runs, one per port.
func TestD_JposClientsToFluxrigMultiPort(t *testing.T) {
	prepare(t)
	p1, p2 := freePort(t), freePort(t)
	noLeftovers(t, p1, p2)
	startFluxrig(t, scenarioText(t, "jpos_style_multi_port.yaml", map[string]int{"8888": p1, "8889": p2}))

	// jpos-client dials 8889 only; run it twice, aimed at each fluxrig listener.
	// The scenario stamps DE63 with the port that answered.
	a := startJpos(t, "jpos-client", map[string]int{"8889": p1}, clientEdit(0))
	b := startJpos(t, "jpos-client", map[string]int{"8889": p2}, clientEdit(0))
	a.waitExit(waitTimeout())
	b.waitExit(waitTimeout())
	for _, tc := range []struct {
		name string
		cl   *jpos
		mark string
	}{{"port 8888 listener", a, "PORT8888"}, {"port 8889 listener", b, "PORT8889"}} {
		out := tc.cl.output()
		n := count(parseISOMsgs(out, "receive"), func(m isoMsg) bool { return isReply(m) && m["63"] == tc.mark })
		if n != 21 {
			t.Errorf("%s: %d replies with DE39=00 and DE63=%s, want 21\n%s", tc.name, n, tc.mark, tail(out, 1500))
		}
		if ok := strings.Count(out, "[OK]"); ok != 21 {
			t.Errorf("%s: %d OK results, want 21", tc.name, ok)
		}
	}
}

// E1: fluxrig jpos_style_server_logon (emit_conn_events) sends a logon 0800
// DE70=001 to a connecting jpos-client-as-server, which answers 0810 DE39=00.
func TestE1_FluxrigServerLogonToJposClientAsServer(t *testing.T) {
	prepare(t)
	srv := freePort(t)
	noLeftovers(t, srv)
	startFluxrig(t, scenarioText(t, "jpos_style_server_logon.yaml", map[string]int{"8888": srv}))

	cl := startJpos(t, "jpos-client-as-server", map[string]int{"8888": srv}, func(dir string) {
		fixEchoExpectation(dir)
		// The example's simulator shuts Q2 down as soon as its two echoes are
		// done, which can be before a logon sent on connect is answered. A client
		// that serves requests stays up, so keep it up.
		replaceOnce(t, filepath.Join(dir, "deploy", "30_clientsimulator.xml"),
			`<property name="shutdown" value="true" />`, `<property name="shutdown" value="false" />`)
	})
	logonReply := func(m isoMsg) bool { return m["0"] == "0810" && m["39"] == "00" && m["70"] == "001" }
	poll(t, waitTimeout(), "the jPOS client to answer the logon", func() bool {
		return count(parseISOMsgs(cl.output(), "send"), logonReply) >= 1
	})
	out := cl.output()
	rx := count(parseISOMsgs(out, "receive"), func(m isoMsg) bool {
		return m["0"] == "0800" && m["70"] == "001" && len(m["7"]) == 10 && len(m["11"]) == 6
	})
	if rx != 1 {
		t.Errorf("jPOS client received %d logon 0800 (DE70=001), want exactly 1\n%s", rx, tail(out, 2000))
	}
	// Its own two echoes are answered by the same fluxrig server.
	poll(t, waitTimeout(), "the simulator results", func() bool {
		return strings.Contains(cl.output(), "[OK]") || strings.Contains(cl.output(), "[FAILURE]")
	})
	if n := strings.Count(cl.output(), "[OK]"); n != 2 || strings.Contains(cl.output(), "[FAILURE]") {
		t.Errorf("simulator echoes: %d OK, want 2 and no FAILURE", n)
	}

	// A second connection gets its own logon (the event names the connection).
	cl2 := startJpos(t, "jpos-client-as-server", map[string]int{"8888": srv}, func(dir string) {
		fixEchoExpectation(dir)
		replaceOnce(t, filepath.Join(dir, "deploy", "30_clientsimulator.xml"),
			`<property name="shutdown" value="true" />`, `<property name="shutdown" value="false" />`)
	})
	poll(t, waitTimeout(), "the second jPOS client to answer its logon", func() bool {
		return count(parseISOMsgs(cl2.output(), "send"), logonReply) >= 1
	})
	if n := count(parseISOMsgs(cl.output(), "receive"), func(m isoMsg) bool { return m["0"] == "0800" && m["70"] == "001" }); n != 1 {
		t.Errorf("first client received %d logons after the second connected, want still 1", n)
	}
}

// E2: jpos-server-as-client sends a logon to every client that connects; the
// fluxrig jpos_style_client_serves_requests client must answer it 0810 DE39=00
// on the same connection, while its own echoes are answered by the jPOS server.
func TestE2_JposServerAsClientLogonToFluxrigClient(t *testing.T) {
	prepare(t)
	srv := freePort(t)
	noLeftovers(t, srv)
	js := startJpos(t, "jpos-server-as-client", map[string]int{"8888": srv}, nil)
	js.waitListening(srv)

	sc := scenarioText(t, "jpos_style_client_serves_requests.yaml", map[string]int{"8888": srv})
	sc = replaceScenario(t, sc, `interval: 30s`, `interval: 1s`) // see its own echoes answered too
	f := startFluxrig(t, sc)

	poll(t, waitTimeout(), "the jPOS server to receive the logon answer", func() bool {
		return count(parseISOMsgs(js.output(), "receive"), func(m isoMsg) bool {
			return m["0"] == "0810" && m["39"] == "00" && m["70"] == "001"
		}) >= 1
	})
	sent := count(parseISOMsgs(js.output(), "send"), func(m isoMsg) bool { return m["0"] == "0800" && m["70"] == "001" })
	if sent != 1 {
		t.Errorf("jPOS server sent %d logons, want 1", sent)
	}
	// The fluxrig client's own echo is answered by the jPOS server (0810/00 for 0800/180000).
	poll(t, waitTimeout(), "the jPOS server to answer a fluxrig echo", func() bool {
		return count(parseISOMsgs(js.output(), "send"), func(m isoMsg) bool { return isReply(m) && m["3"] == "180000" }) >= 1
	})
	// What the fluxrig client saw, as printed by its show_traffic gear.
	poll(t, waitTimeout(), "fluxrig to print the logon request", func() bool {
		return strings.Contains(f.rack.output(), `"70":"001"`)
	})
}
