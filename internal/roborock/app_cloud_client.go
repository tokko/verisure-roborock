package roborock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

type RoborockAppRunner interface {
	Run(ctx context.Context, selector, command string, params any) (json.RawMessage, error)
}

// PythonRoborockAppRunner delegates Roborock-app cloud control to the
// maintained python-roborock library via scripts/roborock_cloud.py.
type PythonRoborockAppRunner struct {
	Python   string
	Script   string
	Email    string
	AuthPath string
	Timeout  time.Duration
}

type roborockAppVacuum struct {
	name     string
	storeKey string
	selector string
	runner   RoborockAppRunner
	msgID    atomic.Uint32
}

var roborockAppRateLimitBackoff = []time.Duration{30 * time.Second, 90 * time.Second}

func NewRoborockAppVacuum(name, storeKey, selector string, runner RoborockAppRunner) (*roborockAppVacuum, error) {
	if selector == "" {
		return nil, fmt.Errorf("roborock app vacuum %s: missing selector", name)
	}
	if storeKey == "" {
		storeKey = selector
	}
	if runner == nil {
		return nil, fmt.Errorf("roborock app vacuum %s: missing runner", name)
	}
	return &roborockAppVacuum{name: name, storeKey: storeKey, selector: selector, runner: runner}, nil
}

func (c *roborockAppVacuum) Name() string { return c.name }

func (c *roborockAppVacuum) Host() string { return c.storeKey }

func (c *roborockAppVacuum) Status(ctx context.Context) (VacuumStatus, error) {
	raw, err := c.call(ctx, "get_status", nil)
	if err != nil {
		return VacuumStatus{}, err
	}
	var results []VacuumStatus
	if err := json.Unmarshal(raw, &results); err != nil {
		return VacuumStatus{}, fmt.Errorf("get_status decode: %w", err)
	}
	if len(results) == 0 {
		return VacuumStatus{}, fmt.Errorf("get_status: empty result")
	}
	return results[0], nil
}

func (c *roborockAppVacuum) CleanSummary(ctx context.Context) (CleanSummary, error) {
	raw, err := c.call(ctx, "get_clean_summary", nil)
	if err != nil {
		return CleanSummary{}, err
	}
	return UnmarshalCleanSummary(raw)
}

func (c *roborockAppVacuum) StartOrResume(ctx context.Context, _ bool) error {
	if err := c.commandWithRateLimitRetry(ctx, "app_start"); err != nil {
		return err
	}
	slog.Info("roborock app cloud: started", "name", c.name)
	return nil
}

func (c *roborockAppVacuum) CleanRooms(ctx context.Context, rooms []int, repeat int) error {
	if repeat <= 0 {
		repeat = 1
	}
	params := []any{map[string]any{
		"segments": rooms,
		"repeat":   repeat,
	}}
	if err := c.commandWithParamsRateLimitRetry(ctx, "app_segment_clean", params); err != nil {
		return err
	}
	slog.Info("roborock app cloud: room clean started", "name", c.name, "rooms", rooms, "repeat", repeat)
	return nil
}

func (c *roborockAppVacuum) Pause(ctx context.Context) error {
	if err := c.command(ctx, "app_pause"); err != nil {
		return err
	}
	slog.Info("roborock app cloud: paused", "name", c.name)
	return nil
}

func (c *roborockAppVacuum) Charge(ctx context.Context) error {
	if err := c.command(ctx, "app_charge"); err != nil {
		return err
	}
	slog.Info("roborock app cloud: returning to dock", "name", c.name)
	return nil
}

func (c *roborockAppVacuum) command(ctx context.Context, command string) error {
	_, err := c.call(ctx, command, []any{})
	return err
}

func (c *roborockAppVacuum) commandWithParamsRateLimitRetry(ctx context.Context, command string, params any) error {
	var err error
	for attempt := 0; attempt <= len(roborockAppRateLimitBackoff); attempt++ {
		_, err = c.call(ctx, command, params)
		if err == nil || !isRoborockAppRateLimit(err) || attempt == len(roborockAppRateLimitBackoff) {
			return err
		}
		wait := roborockAppRateLimitBackoff[attempt]
		slog.Warn("roborock app cloud: rate limited, retrying",
			"name", c.name,
			"command", command,
			"retry_in", wait,
			"attempt", attempt+1,
		)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return err
}

func (c *roborockAppVacuum) commandWithRateLimitRetry(ctx context.Context, command string) error {
	return c.commandWithParamsRateLimitRetry(ctx, command, []any{})
}

func isRoborockAppRateLimit(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "request too frequency") || strings.Contains(msg, "response code: 9002")
}

func (c *roborockAppVacuum) call(ctx context.Context, command string, params any) (json.RawMessage, error) {
	c.msgID.Add(1)
	raw, err := c.runner.Run(ctx, c.selector, command, params)
	if err != nil {
		return nil, fmt.Errorf("roborock app cloud %s %s: %w", c.name, command, err)
	}
	return raw, nil
}

func (r PythonRoborockAppRunner) Run(ctx context.Context, selector, command string, params any) (json.RawMessage, error) {
	python := r.Python
	if python == "" {
		python = "python3"
	}
	if r.Script == "" {
		return nil, fmt.Errorf("missing roborock cloud helper script")
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 45 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req := map[string]any{
		"email":     r.Email,
		"auth_path": r.AuthPath,
		"selector":  selector,
		"command":   command,
		"params":    params,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, python, r.Script, "rpc")
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		detail := bytes.TrimSpace(stderr.Bytes())
		if len(detail) == 0 {
			detail = bytes.TrimSpace(stdout.Bytes())
		}
		return nil, fmt.Errorf("%w: %s", err, detail)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("helper response decode: %w: %.300s", err, stdout.Bytes())
	}
	if resp.Error != "" {
		return nil, fmt.Errorf(resp.Error)
	}
	return resp.Result, nil
}
