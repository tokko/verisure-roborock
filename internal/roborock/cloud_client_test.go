package roborock

import (
	"context"
	"encoding/json"
	"testing"

	"verisure-roborock/internal/config"
	"verisure-roborock/internal/xiaomi"
)

type fakeRPC struct {
	calls []rpcCall
	resp  map[string]json.RawMessage
	err   error
}

type rpcCall struct {
	did    string
	method string
}

func (f *fakeRPC) RPC(_ context.Context, did, method string, _ uint32, _ any) (json.RawMessage, error) {
	f.calls = append(f.calls, rpcCall{did: did, method: method})
	if f.err != nil {
		return nil, f.err
	}
	return f.resp[method], nil
}

func TestCloudVacuumCommandsUseCloudRPC(t *testing.T) {
	rpc := &fakeRPC{resp: map[string]json.RawMessage{
		"get_status":        json.RawMessage(`[{"state":5,"battery":91}]`),
		"get_clean_summary": json.RawMessage(`[10,20,1,[1700000000]]`),
		"app_pause":         json.RawMessage(`["ok"]`),
		"app_charge":        json.RawMessage(`["ok"]`),
	}}
	v, err := NewCloudVacuum("upstairs", "192.0.2.10", "did-1", rpc)
	if err != nil {
		t.Fatal(err)
	}

	status, err := v.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != StateCleaning || status.Battery != 91 {
		t.Fatalf("status = %+v", status)
	}
	summary, err := v.CleanSummary(context.Background())
	if err != nil {
		t.Fatalf("CleanSummary: %v", err)
	}
	if summary.TotalTime != 10 || len(summary.Records) != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if err := v.Pause(context.Background()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := v.Charge(context.Background()); err != nil {
		t.Fatalf("Charge: %v", err)
	}

	want := []rpcCall{
		{"did-1", "get_status"},
		{"did-1", "get_clean_summary"},
		{"did-1", "app_pause"},
		{"did-1", "app_charge"},
	}
	if len(rpc.calls) != len(want) {
		t.Fatalf("calls = %+v, want %+v", rpc.calls, want)
	}
	for i := range want {
		if rpc.calls[i] != want[i] {
			t.Fatalf("call %d = %+v, want %+v", i, rpc.calls[i], want[i])
		}
	}
}

func TestMatchCloudDevice(t *testing.T) {
	devices := []xiaomi.Device{
		{DID: "did-1", Name: "upstairs", LocalIP: "192.0.2.10", Token: "001122"},
		{DID: "did-2", Name: "downstairs", LocalIP: "192.0.2.11", Token: "aabbcc"},
	}
	cases := []struct {
		name string
		cfg  config.VacuumConfig
		want string
	}{
		{"did wins", config.VacuumConfig{DID: "did-2", Host: "192.0.2.10"}, "did-2"},
		{"host match", config.VacuumConfig{Host: "192.0.2.10"}, "did-1"},
		{"token match ignores case", config.VacuumConfig{Token: "AABBCC"}, "did-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := matchCloudDevice(tc.cfg, devices)
			if !ok {
				t.Fatal("matchCloudDevice returned no match")
			}
			if got.DID != tc.want {
				t.Fatalf("DID = %q, want %q", got.DID, tc.want)
			}
		})
	}
}
