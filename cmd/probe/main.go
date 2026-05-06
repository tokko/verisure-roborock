// Temporary probe tool — run with: go run ./cmd/probe
// Tests connectivity and token validity for each configured vacuum.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"verisure-roborock/internal/config"
	"verisure-roborock/internal/roborock"
)

func main() {
	config.LoadDotEnv(".env")

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	// Use a background context with NO overall deadline — each vacuum call gets
	// its own per-operation timeout via the client's timeout setting. This
	// matches exactly how the controller uses the roborock client in production.
	//
	// IMPORTANT: do NOT wrap these calls in context.WithTimeout. If you do, a
	// failing recv() will exhaust the budget and the automatic re-handshake
	// retry has 0s left, producing a misleading "handshake write: i/o timeout"
	// error that masks the real cause (wrong token / no response).
	bgCtx := context.Background()

	for _, v := range cfg.Vacuums {
		fmt.Printf("\n=== %s (%s) ===\n", v.Name, v.Host)
		c, err := roborock.NewClient(v.Name, v.Host, v.Token, 10*time.Second)
		if err != nil {
			fmt.Printf("  NewClient: %v\n", err)
			continue
		}

		fmt.Print("  Handshake ... ")
		hsErr := c.Handshake(bgCtx)
		if hsErr != nil {
			fmt.Printf("FAIL: %v\n", hsErr)
			c.Close()
			continue
		}
		fmt.Printf("OK (deviceID=%d / 0x%08X)\n", c.DeviceID(), c.DeviceID())

		fmt.Print("  get_status  ... ")
		st, stErr := c.Status(bgCtx)
		if stErr != nil {
			fmt.Printf("FAIL: %v\n", stErr)
		} else {
			b, _ := json.Marshal(st)
			fmt.Printf("OK: %s\n", b)
		}

		fmt.Print("  clean_summary .. ")
		cs, csErr := c.CleanSummary(bgCtx)
		if csErr != nil {
			fmt.Printf("FAIL: %v\n", csErr)
		} else {
			fmt.Printf("OK: last=%v count=%d\n", cs.LastCleanTime().Format(time.RFC3339), cs.TotalCount)
		}

		c.Close()
	}
	fmt.Println()
}
