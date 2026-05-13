package roborock

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"verisure-roborock/internal/config"
	"verisure-roborock/internal/xiaomi"
)

type XiaomiRPC interface {
	RPC(ctx context.Context, did, method string, id uint32, params any) (json.RawMessage, error)
}

// CloudVacuum controls a Roborock vacuum through Xiaomi Cloud RPC.
type CloudVacuum struct {
	name     string
	storeKey string
	did      string
	cloud    XiaomiRPC
	msgID    atomic.Uint32
}

func NewCloudVacuum(name, storeKey, did string, cloud XiaomiRPC) (*CloudVacuum, error) {
	if did == "" {
		return nil, fmt.Errorf("cloud vacuum %s: missing DID", name)
	}
	if storeKey == "" {
		storeKey = did
	}
	return &CloudVacuum{name: name, storeKey: storeKey, did: did, cloud: cloud}, nil
}

func NewCloudVacuums(ctx context.Context, cfg []config.VacuumConfig, cloud *xiaomi.CloudClient) ([]*CloudVacuum, error) {
	devices, err := cloud.Devices(ctx)
	if err != nil {
		return nil, err
	}
	vacuums := make([]*CloudVacuum, 0, len(cfg))
	for _, vc := range cfg {
		device, ok := matchCloudDevice(vc, devices)
		if !ok {
			return nil, fmt.Errorf("no Xiaomi cloud device matched %s (set ROBOROCK_%s_DID, or verify host/token)", vc.Name, vacuumIndexHint(vc))
		}
		storeKey := vc.Host
		if storeKey == "" {
			storeKey = device.DID
		}
		name := vc.Name
		if name == "" {
			name = device.Name
		}
		v, err := NewCloudVacuum(name, storeKey, device.DID, cloud)
		if err != nil {
			return nil, err
		}
		vacuums = append(vacuums, v)
	}
	return vacuums, nil
}

func matchCloudDevice(vc config.VacuumConfig, devices []xiaomi.Device) (xiaomi.Device, bool) {
	token := strings.ToLower(strings.TrimSpace(vc.Token))
	if vc.DID != "" {
		for _, d := range devices {
			if d.DID == vc.DID {
				return d, true
			}
		}
		return xiaomi.Device{}, false
	}
	for _, d := range devices {
		switch {
		case vc.Host != "" && d.LocalIP == vc.Host:
			return d, true
		case token != "" && strings.ToLower(d.Token) == token:
			return d, true
		case vc.Host == "" && token == "" && vc.Name != "" && d.Name == vc.Name:
			return d, true
		}
	}
	return xiaomi.Device{}, false
}

func vacuumIndexHint(vc config.VacuumConfig) string {
	if vc.DID != "" {
		return vc.DID
	}
	if vc.Host != "" {
		return vc.Host
	}
	if vc.Name != "" {
		return vc.Name
	}
	return "N"
}

func (c *CloudVacuum) Name() string { return c.name }

func (c *CloudVacuum) Host() string { return c.storeKey }

func (c *CloudVacuum) Status(ctx context.Context) (VacuumStatus, error) {
	raw, err := c.call(ctx, "get_status", []any{})
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

func (c *CloudVacuum) CleanSummary(ctx context.Context) (CleanSummary, error) {
	raw, err := c.call(ctx, "get_clean_summary", []any{})
	if err != nil {
		return CleanSummary{}, err
	}
	return UnmarshalCleanSummary(raw)
}

func (c *CloudVacuum) StartOrResume(ctx context.Context, paused bool) error {
	if paused {
		if err := c.command(ctx, "app_resume"); err == nil {
			slog.Info("roborock cloud: resumed", "name", c.name)
			return nil
		}
		slog.Debug("roborock cloud: app_resume failed, trying app_start", "name", c.name)
	}
	if err := c.command(ctx, "app_start"); err != nil {
		return err
	}
	slog.Info("roborock cloud: started", "name", c.name)
	return nil
}

func (c *CloudVacuum) Pause(ctx context.Context) error {
	if err := c.command(ctx, "app_pause"); err != nil {
		return err
	}
	slog.Info("roborock cloud: paused", "name", c.name)
	return nil
}

func (c *CloudVacuum) Charge(ctx context.Context) error {
	if err := c.command(ctx, "app_charge"); err != nil {
		return err
	}
	slog.Info("roborock cloud: returning to dock", "name", c.name)
	return nil
}

func (c *CloudVacuum) command(ctx context.Context, method string) error {
	_, err := c.call(ctx, method, []any{})
	return err
}

func (c *CloudVacuum) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.msgID.Add(1)
	raw, err := c.cloud.RPC(ctx, c.did, method, id, params)
	if err != nil {
		return nil, fmt.Errorf("roborock cloud %s %s: %w", c.name, method, err)
	}
	return raw, nil
}
