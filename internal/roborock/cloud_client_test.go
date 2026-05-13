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
	params any
}

func (f *fakeRPC) RPC(_ context.Context, did, method string, _ uint32, params any) (json.RawMessage, error) {
	f.calls = append(f.calls, rpcCall{did: did, method: method, params: params})
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
		{did: "did-1", method: "get_status"},
		{did: "did-1", method: "get_clean_summary"},
		{did: "did-1", method: "app_pause"},
		{did: "did-1", method: "app_charge"},
	}
	if len(rpc.calls) != len(want) {
		t.Fatalf("calls = %+v, want %+v", rpc.calls, want)
	}
	for i := range want {
		if rpc.calls[i].did != want[i].did || rpc.calls[i].method != want[i].method {
			t.Fatalf("call %d = %+v, want %+v", i, rpc.calls[i], want[i])
		}
	}
}

func TestCloudVacuumCleanRooms(t *testing.T) {
	rpc := &fakeRPC{resp: map[string]json.RawMessage{
		"app_segment_clean": json.RawMessage(`["ok"]`),
	}}
	v, err := NewCloudVacuum("downstairs", "192.0.2.11", "did-2", rpc)
	if err != nil {
		t.Fatal(err)
	}

	if err := v.CleanRooms(context.Background(), []int{21, 22}, 1); err != nil {
		t.Fatalf("CleanRooms: %v", err)
	}
	if len(rpc.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(rpc.calls))
	}
	got := rpc.calls[0]
	if got.method != "app_segment_clean" {
		t.Fatalf("method = %q", got.method)
	}
	params, ok := got.params.([]any)
	if !ok || len(params) != 1 {
		t.Fatalf("params = %#v", got.params)
	}
	payload, ok := params[0].(map[string]any)
	if !ok {
		t.Fatalf("payload = %#v", params[0])
	}
	if payload["repeat"] != 1 {
		t.Fatalf("repeat = %#v", payload["repeat"])
	}
	segments, ok := payload["segments"].([]int)
	if !ok || len(segments) != 2 || segments[0] != 21 || segments[1] != 22 {
		t.Fatalf("segments = %#v", payload["segments"])
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
