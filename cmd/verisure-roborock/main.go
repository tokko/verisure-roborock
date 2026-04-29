package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"verisure-roborock/internal/config"
	"verisure-roborock/internal/controller"
	"verisure-roborock/internal/roborock"
	"verisure-roborock/internal/store"
	"verisure-roborock/internal/verisure"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Configure structured logging.
	var logLevel slog.Level
	if err := logLevel.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		logLevel = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))

	slog.Info("verisure-roborock starting", "version", version, "vacuums", len(cfg.Vacuums))

	// Load persisted state.
	st, err := store.New(cfg.StorePath)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}

	// Build Verisure client, restoring session cookie from store if available.
	persisted := st.Get()
	verisureClient, err := verisure.NewClient(
		cfg.VerisureEmail,
		cfg.VerisurePassword,
		cfg.VerisureMFAPhone,
		persisted.VerisureCookie,
	)
	if err != nil {
		return fmt.Errorf("verisure client: %w", err)
	}
	if cfg.VerisureGIID != "" {
		verisureClient.SetGIID(cfg.VerisureGIID)
	}

	// Build Roborock clients.
	var vacuums []controller.VacuumCommander
	for _, vc := range cfg.Vacuums {
		rc, err := roborock.NewClient(vc.Name, vc.Host, vc.Token, cfg.RoborockTimeout)
		if err != nil {
			return fmt.Errorf("roborock client %s: %w", vc.Name, err)
		}
		vacuums = append(vacuums, rc)
	}

	ctrl := controller.New(verisureClient, vacuums, st, cfg.PollInterval, cfg.CleanCooldown)

	// Root context, cancelled on SIGINT/SIGTERM.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// HTTP status/health server.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		st := st.Get()
		type vacuumStatus struct {
			Name        string `json:"name"`
			Host        string `json:"host"`
			StartedByUs bool   `json:"started_by_us"`
		}
		var vs []vacuumStatus
		for _, v := range cfg.Vacuums {
			entry := st.Vacuums[v.Host]
			vs = append(vs, vacuumStatus{
				Name:        v.Name,
				Host:        v.Host,
				StartedByUs: entry.StartedByUs,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"control_state": ctrl.State().String(),
			"alarm_state":   st.AlarmState,
			"vacuums":       vs,
		})
	})
	// MFA code submission endpoint — operator POSTs SMS code here during login.
	// Protected by MFA_SECRET bearer token when configured.
	mux.HandleFunc("POST /mfa-code", func(w http.ResponseWriter, r *http.Request) {
		if cfg.MFASecret != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got != cfg.MFASecret {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 16))
		if err != nil || len(body) == 0 {
			http.Error(w, "provide the SMS code in the request body", http.StatusBadRequest)
			return
		}
		code := string(body)
		slog.Info("mfa-code: received code from operator")
		verisureClient.SubmitMFACode(code)
		io.WriteString(w, "ok — MFA code submitted\n")
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		slog.Info("http server listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server error", "err", err)
		}
	}()

	// Persist Verisure session cookie so MFA is not repeated on restart.
	// First persist fires 30s after startup (reconcile has completed by then),
	// then every 5 min to keep it current.
	persistCookie := func() {
		if cookie := verisureClient.SessionCookie(); cookie != "" {
			if err := st.SetVerisureCookie(cookie); err != nil {
				slog.Error("persist verisure cookie", "err", err)
			}
		}
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
			persistCookie()
		}
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				persistCookie()
			}
		}
	}()

	// Run the controller (blocks until ctx is cancelled).
	if err := ctrl.Run(ctx); err != nil && err != context.Canceled {
		return err
	}

	// Persist cookie on graceful shutdown so MFA is not needed after a redeploy.
	persistCookie()

	// Graceful HTTP shutdown.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	srv.Shutdown(shutdownCtx)

	slog.Info("shutdown complete")
	return nil
}
