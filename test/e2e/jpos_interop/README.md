# jPOS interoperability suite

Go tests that start **real jPOS peers** (the Maven modules of the `jpos-example`
repository, jPOS 2.1.6) next to a **real fluxrig Mixer + Rack** and assert on what
crosses the wire. The evidence is the jPOS channel log (`<send>` / `<receive>`
blocks, printed by the jPOS packager from the bytes on the socket), the jPOS
TestRunner results and the Rack log.

```bash
make test-jpos-interop
# or
FLUXRIG_JPOS_INTEROP=1 go test -count=1 -v ./test/e2e/jpos_interop/
```

The suite is opt-in (`FLUXRIG_JPOS_INTEROP=1`): it binds ports, runs JVMs and
downloads jPOS from Maven Central. It is skipped, with the reason, when `java`,
`mvn` or the `jpos-example` checkout are missing, or when Maven cannot compile the
modules (offline).

| Test | Real jPOS peer | fluxrig scenario | Asserts |
|:---|:---|:---|:---|
| A | `jpos-client` | `jpos_style_server_respond.yaml` | 21 echoes get `0810`, DE39 `00`; simulator reports 21 OK |
| B | `jpos-server` | `iso8583_periodic_echo.yaml` | jPOS receives well-formed `0800/180000`, answers `0810/00`, fluxrig logs `rc=00` |
| C1 | `jpos-server-forwarding` -> `jpos-server` | `iso8583_periodic_echo.yaml` as client | `00` through the forwarder; `68` once the upstream is stopped |
| C2 | `jpos-client`, `jpos-server` | `jpos_style_forwarding.yaml` | 21 replies `00`, upstream saw 21 forwarded `0800` |
| C3 | `jpos-client` | `jpos_style_forwarding.yaml` | upstream down -> `91`; upstream accepts but never answers -> `68` |
| D | two `jpos-client` runs | `jpos_style_multi_port.yaml` | each port answers its own 21 echoes (DE63 marks the port) |
| E1 | `jpos-client-as-server` | `jpos_style_server_logon.yaml` | on connect jPOS receives `0800` DE70=`001`, answers `0810/00`; a second client gets its own logon |
| E2 | `jpos-server-as-client` | `jpos_style_client_serves_requests.yaml` | jPOS sends the logon, fluxrig answers `0810/00` on the same connection |

## How it works

* `fluxrig`, `fluxrig-mixer` and `iso8583-tool` are built with `go build` (no
  `make build`, so no `swag`). `FLUXRIG_BIN_DIR` points at prebuilt binaries.
* Each test starts its own Mixer (embedded NATS, auto-adopt) and one Rack in a temp
  directory, imports the scenario through the Mixer API and waits for
  `Scenario Applied Successfully`. This is the mechanism of `test/e2e/segments`.
* The jPOS modules are copied from `jpos-example` (never modified there),
  compiled once into `FLUXRIG_JPOS_WORK` (default `<tmp>/fluxrig-jpos-interop`) and
  run with `java -cp target/classes:<classpath> com.ads.jpos.MainApp`, the same
  `MainApp` that `mvn exec:java` starts. Every run gets a fresh copy of the module,
  so logs and `q2` state never leak between tests.
* Ports are free ports chosen per test: the scenario and the jPOS descriptors are
  rewritten (`8888`, `8889`, `8583` as whole tokens), so a busy 8888 cannot break a
  run and nothing is left listening afterwards (checked at the end of each test).
* Everything is torn down by process-group SIGTERM, then SIGKILL after 10s.
* `FLUXRIG_INTEROP_LOGDIR=<dir>` copies the stdout of every process there.

## Deviations from the examples (all in the per-test copy)

1. `pom.xml` of the client modules: `http://jpos.org/maven` -> `https://jpos.org/maven`.
   Maven 3.8+ blocks plain-http repositories, so `jposee-client-simulator` cannot
   be resolved otherwise. (The UI qbean, `25_clientsimulator_ui.xml`, is removed:
   the run is headless.) JVM flags: `--add-opens java.base/java.lang=ALL-UNNAMED`,
   needed for jPOS 2.1.6's javassist logger binding on Java 17+; the suite ran on
   Java 25 with jPOS 2.1.6, no downgrade needed.
2. `jpos-client/cfg/echo_r` expects response MTI `1810`; a `0800` is answered with
   `0810` by every correct peer (jPOS `setResponseMTI()` included), so the
   simulator marks all echoes `[FAILURE]` ("Received:[0810] Expected:[1810]") even
   against the real `jpos-server`. The copy expects `0810`. The wire is unaffected.
3. `jpos-client-as-server`: `shutdown` is set to `false`. The simulator otherwise
   stops Q2 as soon as its two echoes are answered, which can be before a logon
   sent on connect is answered.
4. Short timeouts so tests finish quickly: the echo `interval` (10s -> 1s), the
   forwarder `Network` timeout (30s -> 2s) and the fluxrig ticket `default_ttl`
   (30s -> 2s) for the `68` paths, and `count="20"` -> `2` in C3.
5. `jpos_style_forwarding.yaml` correlates on DE41 + DE11. A jPOS simulator echo has
   no DE41, so every request would be declined with reason `no_key` (DE39 `96`).
   Tests C2/C3 use DE7 + DE11 as the key.

## What this found

See the commit history of this directory and the notes in `examples/scenarios`.
`jpos_style_server_logon.yaml` is new: the fluxrig counterpart of
`jpos-server-as-client` (`emit_conn_events`), which had no example.
