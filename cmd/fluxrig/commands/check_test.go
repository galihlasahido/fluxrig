// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCheckNATS(t *testing.T) {
	// Without NATS running, it should fail
	_ = os.Setenv("FLUXRIG_BUS_URL", "nats://localhost:14223") // Unlikely port
	ok := checkNATS(defaultCheckTimeout)
	require.False(t, ok)
}

func TestCheckAPI(t *testing.T) {
	// Without API running, it should fail
	_ = os.Setenv("FLUXRIG_API_URL", "http://localhost:18091") // Unlikely port
	ok := checkAPI(defaultCheckTimeout)
	require.False(t, ok)
}

func TestCheckStore(t *testing.T) {
	// Use a temp home to test store check
	tmpHome := t.TempDir()
	originalHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", originalHome) }()

	// 1. Initially missing (WARN -> true)
	ok := checkStore("")
	require.True(t, ok)

	// 2. Create directory
	storeDir := filepath.Join(tmpHome, ".fluxrig", "store")
	err := os.MkdirAll(storeDir, 0700)
	require.NoError(t, err)

	ok = checkStore("")
	require.True(t, ok)

	// 3. Not a directory
	_ = os.Remove(storeDir)
	err = os.WriteFile(storeDir, []byte("not-a-dir"), 0600)
	require.NoError(t, err)
	ok = checkStore("")
	require.False(t, ok)
}

func TestCheckPKI(t *testing.T) {
	tmpHome := t.TempDir()
	originalHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", originalHome) }()

	// 1. Missing (WARN -> true)
	ok := checkPKI("", "")
	require.True(t, ok)

	// 2. Partial keys
	pkiDir := filepath.Join(tmpHome, ".fluxrig", "pki")
	err := os.MkdirAll(pkiDir, 0700)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(pkiDir, "cluster.key"), []byte("test"), 0600)
	require.NoError(t, err)

	ok = checkPKI("", "")
	require.True(t, ok)

	// 3. All keys
	err = os.WriteFile(filepath.Join(pkiDir, "machine.key"), []byte("test"), 0600)
	require.NoError(t, err)
	ok = checkPKI("", "")
	require.True(t, ok)
}

// isolateCheckEnv clears the variables applyCheckConfig exports and the loaders
// read, and restores them when the test ends.
func isolateCheckEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"FLUXRIG_BUS_URL", "FLUXRIG_STORE_DIR", "FLUXRIG_API_URL"} {
		old, had := os.LookupEnv(key)
		require.NoError(t, os.Unsetenv(key))
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
}

func writeCheckConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// check -c exists to validate a config before a Rack or Mixer starts with it.
// A file that neither loader accepts must fail the check, not pass it.
func TestApplyCheckConfigRejectsConfigThatDoesNotLoad(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"mixer with a misspelled key", "[api]\nprot = 8090\n", "api.prot"},
		{"mixer with a bad duration", "[api]\nport = 8090\nread_header_timeout = \"3 seconds\"\n", "read_header_timeout"},
		{"rack with a misspelled key", "[rack]\nname = \"r1\"\ndrain_timout = \"35s\"\n", "rack.drain_timout"},
		{"rack with a bad duration", "[rack]\nname = \"r1\"\ndrain_timeout = \"soon\"\n", "drain_timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateCheckEnv(t)
			err := applyCheckConfig(writeCheckConfig(t, tc.body))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestApplyCheckConfigAcceptsRackAndMixerConfigs(t *testing.T) {
	t.Run("rack", func(t *testing.T) {
		isolateCheckEnv(t)
		path := writeCheckConfig(t, "[rack]\nname = \"r1\"\n\n[snake]\nurl = \"nats://bus.example:4222\"\n")
		require.NoError(t, applyCheckConfig(path))
		require.Equal(t, "nats://bus.example:4222", os.Getenv("FLUXRIG_BUS_URL"))
	})
	t.Run("mixer", func(t *testing.T) {
		isolateCheckEnv(t)
		path := writeCheckConfig(t, "[api]\nport = 18090\n")
		require.NoError(t, applyCheckConfig(path))
		require.Equal(t, "http://localhost:18090", os.Getenv("FLUXRIG_API_URL"))
	})
}

// A pre-flight check answers within its timeout when the bus does not
// answer. It makes one attempt bounded by --timeout, instead of the bus's
// reconnect loop, which took about 50 seconds to give up. The listener
// accepts the connection and never speaks, so the check has to time out.
func TestCheckNATSAnswersWithinTheTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, errAccept := ln.Accept()
			if errAccept != nil {
				return
			}
			defer func() { _ = c.Close() }()
		}
	}()
	t.Setenv("FLUXRIG_BUS_URL", "nats://"+ln.Addr().String())

	const timeout = 500 * time.Millisecond
	start := time.Now()
	done := make(chan bool, 1)
	go func() { done <- checkNATS(timeout) }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("checkNATS reported OK from a server that never spoke")
		}
		if elapsed := time.Since(start); elapsed > 4*timeout {
			t.Fatalf("checkNATS took %s with a %s timeout", elapsed, timeout)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("checkNATS did not answer within 10s with a 500ms timeout")
	}
}

// A zero or negative --timeout falls back to the default rather than leaving
// the network checks without a bound.
func TestEffectiveCheckTimeout(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if got := effectiveCheckTimeout(d); got != defaultCheckTimeout {
			t.Errorf("effectiveCheckTimeout(%s) = %s, want %s", d, got, defaultCheckTimeout)
		}
	}
	if got := effectiveCheckTimeout(2 * time.Second); got != 2*time.Second {
		t.Errorf("effectiveCheckTimeout(2s) = %s, want 2s", got)
	}
}
