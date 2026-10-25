// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/jaab-tech/fluxrig/pkg/bus"
	"github.com/jaab-tech/fluxrig/pkg/config"
)

var configPathCheck string

// checkTimeout bounds each network check. A pre-flight check gives one answer
// quickly, so it makes a single attempt instead of the bus's reconnect loop.
var checkTimeout time.Duration

// defaultCheckTimeout is the --timeout default.
const defaultCheckTimeout = 5 * time.Second

var checkCmd = &cobra.Command{
	Use:   "check",
	Short: "Perform a pre-flight diagnostic check of the environment",
	Long: `Verifies connectivity to the NATS bus, Mixer API, and local storage integrity.
Essential for troubleshooting connectivity and permission issues before production launch.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(color.CyanString("fluxrig Pre-flight Check"))
		fmt.Println(color.CyanString("========================="))

		if configPathCheck != "" {
			fmt.Printf("%-25s ", "Configuration")
			if err := applyCheckConfig(configPathCheck); err != nil {
				fmt.Printf("[%s] %v\n", color.RedString("FAIL"), err)
				fmt.Println()
				fmt.Println(color.RedString("✗ The configuration does not load. A Rack or Mixer would refuse to start with it."))
				os.Exit(1)
			}
			fmt.Printf("[%s] (%s)\n", color.GreenString("OK"), configPathCheck)
		}

		checkTimeout = effectiveCheckTimeout(checkTimeout)

		allOk := true

		// 1. NATS Check
		if !checkNATS(checkTimeout) {
			allOk = false
		}

		// 2. Mixer API Check
		if !checkAPI(checkTimeout) {
			allOk = false
		}

		// 3. Store Check
		storeDir, _ := cmd.Flags().GetString("store-dir")
		if !checkStore(storeDir) {
			allOk = false
		}

		// 4. PKI Check
		var storeDirFlag, _ = cmd.Flags().GetString("store-dir")
		if !checkPKI(configPathCheck, storeDirFlag) {
			allOk = false
		}

		fmt.Println()
		if allOk {
			fmt.Println(color.GreenString("✓ All systems operational."))
		} else {
			fmt.Println(color.RedString("✗ Some checks failed. Please review the errors above."))
			os.Exit(1)
		}
	},
}

// applyCheckConfig loads the file named by -c as a Rack config and as a Mixer
// config, and exports what the later checks read from each. A file is one kind
// or the other, and each loader refuses the keys of the other kind. So the file
// is valid when at least one loader accepts it. When both refuse it, the error
// carries both reasons, because check cannot know which kind was meant.
func applyCheckConfig(path string) error {
	rackCfg, rackErr := config.LoadRack(path)
	if rackErr == nil {
		if rackCfg.Snake.URL != "" {
			_ = os.Setenv("FLUXRIG_BUS_URL", rackCfg.Snake.URL)
		}
		if rackCfg.Store.Dir != "" {
			_ = os.Setenv("FLUXRIG_STORE_DIR", rackCfg.Store.Dir)
		}
	}
	mixerCfg, mixerErr := config.LoadMixer(path)
	if mixerErr == nil {
		_ = os.Setenv("FLUXRIG_API_URL", fmt.Sprintf("http://localhost:%d", mixerCfg.API.Port))
	}
	if rackErr != nil && mixerErr != nil {
		return fmt.Errorf("%s does not load as a Rack or a Mixer config. As a Rack config: %w. As a Mixer config: %w",
			path, rackErr, mixerErr)
	}
	return nil
}

// effectiveCheckTimeout returns the default for a zero or negative
// --timeout, which would leave the network checks unbounded, the hang the
// flag exists to prevent.
func effectiveCheckTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultCheckTimeout
	}
	return d
}

func checkNATS(timeout time.Duration) bool {
	fmt.Printf("%-25s ", "NATS Connectivity")
	url := os.Getenv("FLUXRIG_BUS_URL")
	if url == "" {
		url = "nats://localhost:4222"
	}

	b := bus.NewNatsBus("flux-check")
	err := b.Connect(url, bus.ConnectOptions{
		Name:                 "flux-check",
		ConnectTimeout:       timeout,
		InitialRetryAttempts: 1,
		InitialRetryTimeout:  timeout,
	})

	if err != nil {
		fmt.Printf("[%s] (%s) %v\n", color.RedString("FAIL"), url, err)
		return false
	}
	defer b.Close()

	fmt.Printf("[%s] (%s)\n", color.GreenString("OK"), url)
	return true
}

func checkAPI(timeout time.Duration) bool {
	fmt.Printf("%-25s ", "Mixer API (8090)")
	url := os.Getenv("FLUXRIG_API_URL")
	if url == "" {
		url = "http://localhost:8090"
	}

	req, err := http.NewRequestWithContext(context.Background(), "GET", url+"/api/v1/health", nil)
	if err != nil {
		fmt.Printf("[%s] (%s) %v\n", color.RedString("FAIL"), url, err)
		return false
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[%s] (%s) %v\n", color.RedString("FAIL"), url, err)
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("[%s] (%s) Status: %d\n", color.RedString("FAIL"), url, resp.StatusCode)
		return false
	}

	fmt.Printf("[%s] (%s)\n", color.GreenString("OK"), url)
	return true
}

func checkStore(overridePath string) bool {
	fmt.Printf("%-25s ", "Local CAS Store")
	path := os.Getenv("FLUXRIG_STORE_DIR")
	if path == "" {
		path = overridePath
	}
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".fluxrig", "store")
	}

	// Check if exists
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		fmt.Printf("[%s] Missing (%s)\n", color.YellowString("WARN"), path)
		return true // Warning is not a failure if it's the first run
	}

	if err != nil {
		fmt.Printf("[%s] %v\n", color.RedString("FAIL"), err)
		return false
	}

	if !info.IsDir() {
		fmt.Printf("[%s] Path is not a directory (%s)\n", color.RedString("FAIL"), path)
		return false
	}

	// Try writing a small file
	testFile := filepath.Join(path, ".check_tmp")
	if err := os.WriteFile(testFile, []byte("ok"), 0600); err != nil {
		fmt.Printf("[%s] Write permission denied (%s)\n", color.RedString("FAIL"), path)
		return false
	}
	_ = os.Remove(testFile)

	fmt.Printf("[%s] (%s)\n", color.GreenString("OK"), path)
	return true
}

func checkPKI(configPath string, overrideStoreDir string) bool {
	fmt.Printf("%-25s ", "PKI/TLS Certificates")

	pkiDirs := []string{}

	// 1. Try from Config
	if configPath != "" {
		// Try Mixer Config first
		if cfg, err := config.LoadMixer(configPath); err == nil {
			pkiDirs = append(pkiDirs, cfg.Store.Dir)
		} else if cfg, err := config.LoadRack(configPath); err == nil {
			pkiDirs = append(pkiDirs, cfg.Store.Dir)
		}
	}

	// 2. Try Override
	if overrideStoreDir != "" {
		pkiDirs = append(pkiDirs, overrideStoreDir)
	}

	// 3. Fallback to Home
	home, _ := os.UserHomeDir()
	pkiDirs = append(pkiDirs, filepath.Join(home, ".fluxrig", "pki"))

	requiredFiles := []string{"cluster.key", "machine.key"}

	// We only need one of the directories to have the keys to consider it OK (or WARN if missing everywhere)
	foundDir := ""
	missingInAll := true
	partialFound := false
	partialDir := ""

	for _, dir := range pkiDirs {
		foundCount := 0
		for _, f := range requiredFiles {
			path := filepath.Join(dir, f)
			if _, err := os.Stat(path); err == nil {
				foundCount++
			}
		}

		if foundCount == len(requiredFiles) {
			foundDir = dir
			missingInAll = false
			partialFound = false // Found a perfect match, ignore any previous partials
			break
		} else if foundCount > 0 {
			partialFound = true
			partialDir = dir
		}
	}

	if !missingInAll {
		fmt.Printf("[%s] Keys present in %s\n", color.GreenString("OK"), foundDir)
		return true
	}

	if partialFound {
		fmt.Printf("[%s] Partial keys found in %s (Bootstrap recommended)\n", color.YellowString("WARN"), partialDir)
		return true
	}

	fmt.Printf("[%s] Awaiting first enrollment (Sovereign auto-bootstrap)\n", color.GreenString("READY"))
	return true
}

func init() {
	checkCmd.Flags().StringVarP(&configPathCheck, "config", "c", "", "Path to configuration file")
	checkCmd.Flags().DurationVar(&checkTimeout, "timeout", defaultCheckTimeout, "How long each network check (NATS, Mixer API) may take")
	rootCmd.AddCommand(checkCmd)
}
