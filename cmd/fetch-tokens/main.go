// Command fetch-tokens logs into the Xiaomi cloud using your Mi Home / Roborock
// account credentials and prints the local miIO token for each Roborock device
// found on your account. Copy the output directly into your .env file.
//
// Usage:
//
//	make fetch-tokens
//	# or
//	go run ./cmd/fetch-tokens
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"verisure-roborock/internal/config"
	"verisure-roborock/internal/xiaomi"
)

func main() {
	config.LoadDotEnv(".env")

	email := os.Getenv("XIAOMI_EMAIL")
	if email == "" {
		email = os.Getenv("VERISURE_EMAIL")
	}
	password := os.Getenv("XIAOMI_PASSWORD")
	if password == "" {
		password = os.Getenv("VERISURE_PASSWORD")
	}
	country := os.Getenv("XIAOMI_COUNTRY")
	if country == "" {
		country = "de" // EU default
	}

	if email == "" || password == "" {
		fmt.Fprintln(os.Stderr, "error: set XIAOMI_EMAIL and XIAOMI_PASSWORD (or VERISURE_EMAIL / VERISURE_PASSWORD) in .env")
		os.Exit(1)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	slog.Info("logging into Xiaomi cloud", "email", email, "country", country)

	client, err := xiaomi.NewCloudClient(email, password, country)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if err := client.Login(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "login failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Troubleshooting:")
		fmt.Fprintln(os.Stderr, "  - Check XIAOMI_EMAIL and XIAOMI_PASSWORD in your .env")
		fmt.Fprintln(os.Stderr, "  - For non-EU accounts, set XIAOMI_COUNTRY=us (or sg, cn, ru, tw, i2)")
		fmt.Fprintln(os.Stderr, "  - Xiaomi may require a captcha after repeated failures — wait a few minutes")
		os.Exit(1)
	}

	slog.Info("fetching device list...")
	devices, err := client.Devices(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "device list failed: %v\n", err)
		os.Exit(1)
	}

	// Filter to Roborock vacuums.
	var robots []xiaomi.Device
	for _, d := range devices {
		if d.IsRoborock() {
			robots = append(robots, d)
		}
	}

	if len(robots) == 0 {
		fmt.Fprintln(os.Stderr, "no Roborock devices found on this account")
		fmt.Fprintf(os.Stderr, "found %d total devices:\n", len(devices))
		for _, d := range devices {
			fmt.Fprintf(os.Stderr, "  %s (%s) @ %s\n", d.Name, d.Model, d.LocalIP)
		}
		os.Exit(1)
	}

	fmt.Println("# Paste the following into your .env file:")
	fmt.Println("# (review IPs — they are only correct if the device is currently on your local network)")
	fmt.Println()

	for i, d := range robots {
		token := d.Token
		if token == "" {
			token = "<token not available — device may be owned by a different account>"
		}
		name := sanitizeName(d.Name)
		fmt.Printf("ROBOROCK_%d_HOST=%s\n", i, d.LocalIP)
		fmt.Printf("ROBOROCK_%d_TOKEN=%s\n", i, token)
		fmt.Printf("ROBOROCK_%d_NAME=%s\n", i, name)
		if i < len(robots)-1 {
			fmt.Println()
		}
	}

	fmt.Println()
	fmt.Printf("# Found %d Roborock device(s)\n", len(robots))
	if len(devices) > len(robots) {
		fmt.Printf("# (%d other non-Roborock devices were skipped)\n", len(devices)-len(robots))
	}
}

// sanitizeName converts a device name into a safe env-value label.
func sanitizeName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, " ", "-")
	// Keep only alphanumeric and hyphens.
	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
