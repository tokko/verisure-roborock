// Command fetch-tokens discovers your Roborock vacuum IPs and local tokens.
//
// It tries two cloud backends in order:
//  1. Roborock cloud (euiot.roborock.com) — for accounts created in the Roborock app
//  2. Xiaomi / Mi Home cloud              — for accounts created in the Mi Home app
//
// The Roborock cloud flow sends a one-time code to your email.
// The Xiaomi cloud flow uses your password directly.
//
// Output is ready to paste into .env.
//
// Usage:
//
//	make fetch-tokens
//	# or
//	go run ./cmd/fetch-tokens
package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"verisure-roborock/internal/config"
	"verisure-roborock/internal/roborock"
	"verisure-roborock/internal/xiaomi"
)

func main() {
	config.LoadDotEnv(".env")

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	email := firstNonEmpty(os.Getenv("ROBOROCK_EMAIL"), os.Getenv("XIAOMI_EMAIL"), os.Getenv("VERISURE_EMAIL"))
	password := firstNonEmpty(os.Getenv("ROBOROCK_PASSWORD"), os.Getenv("XIAOMI_PASSWORD"), os.Getenv("VERISURE_PASSWORD"))
	country := firstNonEmpty(os.Getenv("XIAOMI_COUNTRY"), "de")

	if email == "" {
		die("set VERISURE_EMAIL (or XIAOMI_EMAIL) in .env")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Try Roborock cloud first — this is what the Roborock app uses.
	devices, err := fetchViaRoborock(ctx, email, country)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nRoborock cloud failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "Trying Xiaomi / Mi Home cloud instead...")
		fmt.Fprintln(os.Stderr, "")

		if password == "" {
			die("set VERISURE_PASSWORD (or XIAOMI_PASSWORD) in .env for Xiaomi cloud login")
		}
		devices, err = fetchViaXiaomi(ctx, email, password, country)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nXiaomi cloud also failed: %v\n", err)
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "Troubleshooting:")
			fmt.Fprintln(os.Stderr, "  - Roborock app users: check your email for the verification code")
			fmt.Fprintln(os.Stderr, "  - Mi Home app users:  verify VERISURE_PASSWORD / XIAOMI_PASSWORD")
			fmt.Fprintln(os.Stderr, "  - Non-EU accounts:    set XIAOMI_COUNTRY=us or sg")
			os.Exit(1)
		}
	}

	printEnv(devices)
}

// fetchViaRoborock authenticates via euiot.roborock.com using an email OTP.
func fetchViaRoborock(ctx context.Context, email, region string) ([]tokenDevice, error) {
	client, err := roborock.NewCloudClient(email, region)
	if err != nil {
		return nil, err
	}

	fmt.Fprintf(os.Stderr, "Sending verification code to %s...\n", email)
	msg, err := client.RequestEmailCode(ctx)
	if err != nil {
		return nil, fmt.Errorf("send code: %w", err)
	}
	if msg != "" {
		fmt.Fprintf(os.Stderr, "API response: %s\n", msg)
	}

	fmt.Fprint(os.Stderr, "Enter the code from your email (check spam too): ")
	code := readLine()
	if code == "" {
		return nil, fmt.Errorf("no code entered")
	}

	if err := client.LoginWithCode(ctx, code); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	fmt.Fprintln(os.Stderr, "Authenticated. Fetching devices...")

	cloudDevices, err := client.Devices(ctx)
	if err != nil {
		return nil, fmt.Errorf("device list: %w", err)
	}

	var out []tokenDevice
	for _, d := range cloudDevices {
		ip := ""
		if d.NetworkInfo != nil {
			ip = d.NetworkInfo.LocalIP
		}
		out = append(out, tokenDevice{
			Name:  d.Name,
			IP:    ip,
			Token: d.LocalKey,
		})
	}
	return out, nil
}

// fetchViaXiaomi authenticates via Xiaomi account.xiaomi.com (Mi Home accounts).
func fetchViaXiaomi(ctx context.Context, email, password, country string) ([]tokenDevice, error) {
	client, err := xiaomi.NewCloudClient(email, password, country)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "Logging into Xiaomi cloud (%s)...\n", country)
	if err := client.Login(ctx); err != nil {
		return nil, err
	}
	fmt.Fprintln(os.Stderr, "Fetching devices...")

	devices, err := client.Devices(ctx)
	if err != nil {
		return nil, err
	}

	var out []tokenDevice
	for _, d := range devices {
		if !d.IsRoborock() {
			continue
		}
		out = append(out, tokenDevice{
			Name:  d.Name,
			IP:    d.LocalIP,
			Token: d.Token,
		})
	}
	return out, nil
}

type tokenDevice struct {
	Name  string
	IP    string
	Token string
}

func printEnv(devices []tokenDevice) {
	if len(devices) == 0 {
		fmt.Fprintln(os.Stderr, "no Roborock devices found on this account")
		os.Exit(1)
	}

	fmt.Println("# Paste into your .env:")
	fmt.Println("# (verify IPs — they are only accurate if devices are on your local network now)")
	fmt.Println()

	for i, d := range devices {
		ip := d.IP
		if ip == "" {
			ip = "<check your router DHCP table>"
		}
		name := sanitizeName(d.Name)
		token := d.Token
		if token == "" {
			token = "<not available>"
		}
		fmt.Printf("ROBOROCK_%d_HOST=%s\n", i, ip)
		fmt.Printf("ROBOROCK_%d_TOKEN=%s\n", i, token)
		fmt.Printf("ROBOROCK_%d_NAME=%s\n", i, name)
		if i < len(devices)-1 {
			fmt.Println()
		}
	}

	fmt.Println()
	fmt.Fprintf(os.Stderr, "Found %d device(s)\n", len(devices))
}

func sanitizeName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, " ", "-")
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" {
		return "vacuum"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func readLine() string {
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() {
		return strings.TrimSpace(sc.Text())
	}
	return ""
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, "error:", msg)
	os.Exit(1)
}
