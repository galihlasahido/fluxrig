// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

// Package jposinterop proves, with real jPOS peers, that fluxrig scenarios can do
// what the jPOS example applications (../../../../jpos-example) do. See README.md.
package jposinterop

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Environment knobs (all optional).
//
//	FLUXRIG_JPOS_INTEROP=1      required: the suite is heavy, binds ports and
//	                            downloads jPOS from Maven Central, so it is opt-in.
//	FLUXRIG_JPOS_EXAMPLES       path of the jpos-example checkout (default: found
//	                            by walking up from the repository).
//	FLUXRIG_BIN_DIR             directory with fluxrig, fluxrig-mixer, iso8583-tool
//	                            (default: built once with `go build` into a temp dir).
//	FLUXRIG_JPOS_WORK           directory where the jPOS modules are compiled once
//	                            and cached (default: <tmp>/fluxrig-jpos-interop).
//	FLUXRIG_INTEROP_TIMEOUT     per-wait timeout, a Go duration (default 90s).
//	FLUXRIG_INTEROP_LOGDIR      if set, the stdout of every process is copied there.
const (
	envEnable   = "FLUXRIG_JPOS_INTEROP"
	envExamples = "FLUXRIG_JPOS_EXAMPLES"
	envBinDir   = "FLUXRIG_BIN_DIR"
	envWork     = "FLUXRIG_JPOS_WORK"
	envTimeout  = "FLUXRIG_INTEROP_TIMEOUT"
	envLogDir   = "FLUXRIG_INTEROP_LOGDIR"
)

var javaFlags = []string{"--add-opens", "java.base/java.lang=ALL-UNNAMED", "-Djava.awt.headless=true"}

func waitTimeout() time.Duration {
	if v := os.Getenv(envTimeout); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 90 * time.Second
}

// poll calls cond every 100ms until it returns true or d elapses.
func poll(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout after %s waiting for: %s", d, what)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func portOpen(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// ---------------------------------------------------------------------------
// Prerequisites, repository and tool discovery
// ---------------------------------------------------------------------------

var (
	setupOnce sync.Once
	setupErr  error
	skipWhy   string
	repoRoot  string
	binDir    string
	jposWork  string
	jposSrc   string
)

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found")
		}
		dir = parent
	}
}

func findJposExamples(root string) string {
	if v := os.Getenv(envExamples); v != "" {
		return v
	}
	dir := root
	for {
		cand := filepath.Join(dir, "jpos-example")
		if st, err := os.Stat(filepath.Join(cand, "jpos-server")); err == nil && st.IsDir() {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func run(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// setup runs once: it decides whether the suite can run, builds the fluxrig
// binaries and compiles the jPOS modules. It sets skipWhy when a prerequisite is
// missing (the tests skip) and setupErr when something that should work failed.
func setup() {
	if os.Getenv(envEnable) != "1" {
		skipWhy = envEnable + "=1 not set (jPOS interop suite is opt-in)"
		return
	}
	for _, tool := range []string{"java", "mvn", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			skipWhy = tool + " not found in PATH"
			return
		}
	}
	var err error
	if repoRoot, err = findRepoRoot(); err != nil {
		setupErr = err
		return
	}
	if jposSrc = findJposExamples(repoRoot); jposSrc == "" {
		skipWhy = "jpos-example checkout not found (set " + envExamples + ")"
		return
	}

	// fluxrig binaries
	if binDir = os.Getenv(envBinDir); binDir == "" {
		binDir = filepath.Join(os.TempDir(), "fluxrig-jpos-interop-bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			setupErr = err
			return
		}
		out, err := run(repoRoot, "go", "build", "-o", binDir+"/", "./cmd/fluxrig", "./cmd/fluxrig-mixer", "./cmd/iso8583-tool")
		if err != nil {
			setupErr = fmt.Errorf("go build: %v\n%s", err, out)
			return
		}
	}

	// jPOS modules: copy, patch, compile, resolve the classpath (cached).
	if jposWork = os.Getenv(envWork); jposWork == "" {
		jposWork = filepath.Join(os.TempDir(), "fluxrig-jpos-interop")
	}
	if err := os.MkdirAll(jposWork, 0o755); err != nil {
		setupErr = err
		return
	}
	for _, mod := range []string{"jpos-server", "jpos-server-as-client", "jpos-server-forwarding",
		"jpos-server-multi-port", "jpos-client", "jpos-client-as-server"} {
		dst := filepath.Join(jposWork, mod)
		if _, err := os.Stat(filepath.Join(dst, "cp.txt")); err == nil {
			continue // already built
		}
		if err := os.RemoveAll(dst); err != nil {
			setupErr = err
			return
		}
		if out, err := run("", "cp", "-R", filepath.Join(jposSrc, mod), dst); err != nil {
			setupErr = fmt.Errorf("copy %s: %v\n%s", mod, err, out)
			return
		}
		// The client modules pull org.jpos.ee:jposee-client-simulator from
		// http://jpos.org/maven, which Maven 3.8+ blocks (plain http). The same
		// repository answers on https.
		pom := filepath.Join(dst, "pom.xml")
		b, _ := os.ReadFile(pom)
		_ = os.WriteFile(pom, bytes.ReplaceAll(b, []byte("http://jpos.org/maven"), []byte("https://jpos.org/maven")), 0o644)
		if out, err := run(dst, "mvn", "-q", "-B", "compile"); err != nil {
			skipWhy = fmt.Sprintf("cannot compile %s with Maven (offline?): %v\n%s", mod, err, tail(out, 1500))
			return
		}
		if out, err := run(dst, "mvn", "-q", "-B", "dependency:build-classpath", "-Dmdep.outputFile=cp.txt"); err != nil {
			skipWhy = fmt.Sprintf("cannot resolve classpath of %s: %v\n%s", mod, err, tail(out, 1500))
			return
		}
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func prepare(t *testing.T) {
	t.Helper()
	setupOnce.Do(setup)
	if setupErr != nil {
		t.Fatalf("interop setup failed: %v", setupErr)
	}
	if skipWhy != "" {
		t.Skip(skipWhy)
	}
}

// ---------------------------------------------------------------------------
// Processes
// ---------------------------------------------------------------------------

type proc struct {
	t    *testing.T
	name string
	cmd  *exec.Cmd
	log  string // path of the combined stdout+stderr file
	done chan struct{}
	err  error
}

func start(t *testing.T, name, dir string, env []string, bin string, args ...string) *proc {
	t.Helper()
	logPath := filepath.Join(dir, name+".stdout")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	p := &proc{t: t, name: name, cmd: cmd, log: logPath, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		f.Close()
		close(p.done)
	}()
	t.Cleanup(func() {
		p.stop()
		// Keep the logs of every process when asked, for post-mortems.
		if dst := os.Getenv(envLogDir); dst != "" {
			_ = os.MkdirAll(dst, 0o755)
			if b, err := os.ReadFile(p.log); err == nil {
				_ = os.WriteFile(filepath.Join(dst, t.Name()+"-"+name+".stdout"), b, 0o644)
			}
		}
	})
	return p
}

func (p *proc) output() string {
	b, _ := os.ReadFile(p.log)
	return string(b)
}

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// stop asks the process group to terminate, then kills it. Idempotent.
func (p *proc) stop() {
	if p.exited() {
		return
	}
	pgid := p.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-p.done
	}
}

// ---------------------------------------------------------------------------
// fluxrig: Mixer + Rack running one scenario
// ---------------------------------------------------------------------------

type fluxrig struct {
	t     *testing.T
	dir   string
	mixer *proc
	rack  *proc
	api   string
	name  string
}

// scenarioText reads an examples/scenarios file and applies the port mapping
// (old port string -> new port), so concurrent runs and a busy 8888 do not clash.
func scenarioText(t *testing.T, file string, ports map[string]int) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, "examples", "scenarios", file))
	if err != nil {
		t.Fatal(err)
	}
	return patchPorts(string(b), ports)
}

var portToken = regexp.MustCompile(`\b(8888|8889|8583)\b`)

// patchPorts replaces whole-token occurrences, so 8888 never hits 18888.
func patchPorts(s string, ports map[string]int) string {
	return portToken.ReplaceAllStringFunc(s, func(m string) string {
		if p, ok := ports[m]; ok {
			return fmt.Sprint(p)
		}
		return m
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// startFluxrig starts a Mixer (embedded NATS, auto-adopt) and one Rack, imports
// and activates the scenario, and returns once the Rack reports it applied.
func startFluxrig(t *testing.T, scenario string) *fluxrig {
	t.Helper()
	d := t.TempDir()
	f := &fluxrig{t: t, dir: d, name: "rack-interop"}
	apiPort, natsPort := freePort(t), freePort(t)
	f.api = fmt.Sprintf("http://127.0.0.1:%d/api/v1", apiPort)

	mdir, rdir := filepath.Join(d, "mixer"), filepath.Join(d, "rack")
	for _, p := range []string{mdir + "/logs", mdir + "/data", rdir + "/logs", rdir + "/data"} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The codec resolves spec_path relative to the Rack's working directory.
	if out, err := run("", "cp", "-R", filepath.Join(repoRoot, "examples", "specs"), filepath.Join(rdir, "specs")); err != nil {
		t.Fatalf("copy specs: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(rdir, "examples"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(rdir, "specs"), filepath.Join(rdir, "examples", "specs")); err != nil {
		t.Fatal(err)
	}

	writeFile(t, mdir+"/mixer.toml", fmt.Sprintf(`[logging]
level = "debug"
filename = "logs/mixer.log"

[api]
port = %d

[snake]
port = %d
url = "nats://127.0.0.1:%d"

[store]
dir = "./data"
database_file = "flux.duckdb"
cluster_key_file = "cluster.key"

[enrollment]
auto_adopt = true
`, apiPort, natsPort, natsPort))
	writeFile(t, rdir+"/rack.toml", fmt.Sprintf(`[logging]
level = "debug"
filename = "logs/rack.log"

[store]
dir = "./data"
state_file = "state.flux"

[rack]
name = "%s"
heartbeat_interval = "1s"

[rack.bus]
url = "nats://127.0.0.1:%d"
`, f.name, natsPort))
	writeFile(t, d+"/scenario.yaml", scenario)

	if out, err := run(mdir, filepath.Join(binDir, "fluxrig"), "keys", "gen-cluster", "-o", "./data/cluster.key"); err != nil {
		t.Fatalf("gen-cluster: %v\n%s", err, out)
	}
	env := []string{"FLUXRIG_API_AUTH_DISABLED_DANGEROUSLY=true"}
	f.mixer = start(t, "mixer", mdir, env, filepath.Join(binDir, "fluxrig-mixer"), "-c", "mixer.toml")
	poll(t, 30*time.Second, "mixer API", func() bool { return portOpen(apiPort) })
	f.rack = start(t, "rack", rdir, env, filepath.Join(binDir, "fluxrig"), "rack", "-c", "rack.toml")
	poll(t, 60*time.Second, "rack enrolled", func() bool {
		return strings.Contains(httpGet(f.api+"/racks?status=active"), f.name)
	})
	resp, err := http.Post(f.api+"/scenario/import?activate=true", "application/x-yaml", strings.NewReader(scenario))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("scenario import: HTTP %d: %s", resp.StatusCode, body)
	}
	poll(t, 60*time.Second, "scenario applied on the rack", func() bool {
		return strings.Contains(f.rackLog(), "Scenario Applied Successfully")
	})
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("---- rack.log (tail) ----\n%s", tail(f.rackLog(), 6000))
		}
	})
	return f
}

func (f *fluxrig) rackLog() string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "rack", "logs", "rack.log"))
	return string(b)
}

func httpGet(url string) string {
	c := http.Client{Timeout: 2 * time.Second}
	r, err := c.Get(url)
	if err != nil {
		return ""
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// ---------------------------------------------------------------------------
// jPOS peers
// ---------------------------------------------------------------------------

type jpos struct {
	*proc
	dir string
}

// startJpos copies a pre-built jPOS example module into a fresh directory, applies
// the port mapping to its deploy/ and cfg/ files, lets edit change the copy, and
// starts it the way `mvn exec:java -Dexec.mainClass=com.ads.jpos.MainApp` would.
func startJpos(t *testing.T, module string, ports map[string]int, edit func(dir string)) *jpos {
	t.Helper()
	dir := filepath.Join(t.TempDir(), module)
	if out, err := run("", "cp", "-R", filepath.Join(jposWork, module), dir); err != nil {
		t.Fatalf("copy %s: %v\n%s", module, err, out)
	}
	for _, sub := range []string{"deploy", "cfg"} {
		files, _ := filepath.Glob(filepath.Join(dir, sub, "*"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			if p := patchPorts(string(b), ports); p != string(b) {
				_ = os.WriteFile(f, []byte(p), 0o644)
			}
		}
	}
	// A headless run has no use for the Swing meter.
	_ = os.Remove(filepath.Join(dir, "deploy", "25_clientsimulator_ui.xml"))
	if edit != nil {
		edit(dir)
	}
	cp, err := os.ReadFile(filepath.Join(dir, "cp.txt"))
	if err != nil {
		t.Fatal(err)
	}
	args := append(append([]string{}, javaFlags...), "-cp", filepath.Join(dir, "target", "classes")+":"+strings.TrimSpace(string(cp)), "com.ads.jpos.MainApp")
	p := start(t, module, dir, nil, "java", args...)
	return &jpos{proc: p, dir: dir}
}

// waitExit waits for a self-terminating peer (the client simulators shut Q2 down
// when their test suite ends).
func (p *proc) waitExit(d time.Duration) {
	p.t.Helper()
	select {
	case <-p.done:
	case <-time.After(d):
		p.t.Fatalf("%s did not exit within %s; output tail:\n%s", p.name, d, tail(p.output(), 4000))
	}
}

// parseISOMsgs extracts the fields of every ISO message a jPOS channel logged as
// received ("receive") or sent ("send"). The channel log is the wire evidence:
// jPOS prints what its packager unpacked from, or packed onto, the socket.
var (
	isomsgRe = regexp.MustCompile(`(?s)<(receive|send)>\s*<isomsg direction="[a-z]+">(.*?)</isomsg>`)
	fieldRe  = regexp.MustCompile(`<field id="(\d+)" value="([^"]*)"`)
)

type isoMsg map[string]string

func parseISOMsgs(out, kind string) []isoMsg {
	var res []isoMsg
	for _, m := range isomsgRe.FindAllStringSubmatch(out, -1) {
		if m[1] != kind {
			continue
		}
		msg := isoMsg{}
		for _, f := range fieldRe.FindAllStringSubmatch(m[2], -1) {
			msg[f[1]] = f[2]
		}
		res = append(res, msg)
	}
	return res
}

func count(msgs []isoMsg, pred func(isoMsg) bool) int {
	n := 0
	for _, m := range msgs {
		if pred(m) {
			n++
		}
	}
	return n
}

func isReply(m isoMsg) bool { return m["0"] == "0810" && m["39"] == "00" }

// noLeftovers fails if any of the ports still accepts connections after cleanup.
func noLeftovers(t *testing.T, ports ...int) {
	t.Helper()
	t.Cleanup(func() {
		time.Sleep(200 * time.Millisecond)
		for _, p := range ports {
			if portOpen(p) {
				t.Errorf("port %d still open after the test cleanup", p)
			}
		}
	})
}

// fixEchoExpectation changes the expected-response MTI of the jpos-client echo
// test from 1810 to 0810. The example's cfg/echo_r expects MTI 1810, but the
// response MTI of a 0800 is 0810 (jPOS setResponseMTI(), which jpos-server and
// jpos-client-as-server use), so against ANY correct peer, including the real
// jPOS server, the simulator reports every echo as a FAILURE ("[0] Received:[0810]
// Expected:[1810]"). The wire is fine; only the expectation file is wrong.
func fixEchoExpectation(dir string) {
	p := filepath.Join(dir, "cfg", "echo_r")
	b, err := os.ReadFile(p)
	if err != nil {
		panic(err)
	}
	_ = os.WriteFile(p, bytes.ReplaceAll(b, []byte(`value="1810"`), []byte(`value="0810"`)), 0o644)
}

// clientEdit returns an edit function for the jpos-client modules: it applies
// fixEchoExpectation and, when echoes > 0, runs that many echoes in the first
// test instead of 20 (the suite still ends with one more echo).
func clientEdit(echoes int) func(string) {
	return func(dir string) {
		fixEchoExpectation(dir)
		if echoes <= 0 {
			return
		}
		p := filepath.Join(dir, "deploy", "30_clientsimulator.xml")
		b, err := os.ReadFile(p)
		if err != nil {
			panic(err)
		}
		nb := bytes.Replace(b, []byte(`count="20"`), []byte(fmt.Sprintf(`count="%d"`, echoes)), 1)
		if bytes.Equal(nb, b) {
			panic("count=\"20\" not found in 30_clientsimulator.xml")
		}
		_ = os.WriteFile(p, nb, 0o644)
	}
}

// replaceOnce rewrites a file in a jPOS module copy and fails if the text is gone.
func replaceOnce(t *testing.T, path, old, new string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(old)) {
		t.Fatalf("%s no longer contains %q", path, old)
	}
	if err := os.WriteFile(path, bytes.Replace(b, []byte(old), []byte(new), 1), 0o644); err != nil {
		t.Fatal(err)
	}
}

func replaceScenario(t *testing.T, sc, old, new string) string {
	t.Helper()
	if !strings.Contains(sc, old) {
		t.Fatalf("scenario no longer contains %q", old)
	}
	return strings.Replace(sc, old, new, 1)
}

// rcCount counts "echo reply ... rc=<code>" lines of the periodic echo sink.
func rcCount(f *fluxrig, rc string) int {
	return countLines(f.rackLog()+f.rack.output(), "echo reply mti=0810", "rc="+rc)
}

func countLines(s string, subs ...string) int {
	n := 0
outer:
	for _, l := range strings.Split(s, "\n") {
		for _, sub := range subs {
			if !strings.Contains(l, sub) {
				continue outer
			}
		}
		n++
	}
	return n
}

// waitListening waits until a jPOS QServer reports that it listens on port. It is
// used instead of a TCP probe because a probe is a connection, and some peers
// (jpos-server-as-client) react to every connection they accept.
func (p *proc) waitListening(port int) {
	p.t.Helper()
	want := fmt.Sprintf("listening on port %d", port)
	poll(p.t, waitTimeout(), p.name+" "+want, func() bool { return strings.Contains(p.output(), want) })
}
