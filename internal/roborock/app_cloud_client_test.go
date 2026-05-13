package roborock

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type fakeRoborockAppRunner struct {
	calls  []appRunnerCall
	resp   map[string]json.RawMessage
	err    error
	errSeq []error
}

type appRunnerCall struct {
	selector string
	command  string
}

func (f *fakeRoborockAppRunner) Run(_ context.Context, selector, command string, _ any) (json.RawMessage, error) {
	f.calls = append(f.calls, appRunnerCall{selector: selector, command: command})
	i := len(f.calls) - 1
	if i < len(f.errSeq) && f.errSeq[i] != nil {
		return nil, f.errSeq[i]
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.resp[command], nil
}

func TestRoborockAppVacuumCommandsUseCloudRunner(t *testing.T) {
	runner := &fakeRoborockAppRunner{resp: map[string]json.RawMessage{
		"get_status":        json.RawMessage(`[{"state":5,"battery":91}]`),
		"get_clean_summary": json.RawMessage(`[10,20,1,[1700000000]]`),
		"app_start":         json.RawMessage(`["ok"]`),
		"app_pause":         json.RawMessage(`["ok"]`),
		"app_charge":        json.RawMessage(`["ok"]`),
	}}
	v, err := NewRoborockAppVacuum("upstairs", "192.0.2.10", "upstairs", runner)
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
	if err := v.StartOrResume(context.Background(), true); err != nil {
		t.Fatalf("StartOrResume: %v", err)
	}
	if err := v.Pause(context.Background()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := v.Charge(context.Background()); err != nil {
		t.Fatalf("Charge: %v", err)
	}

	want := []appRunnerCall{
		{"upstairs", "get_status"},
		{"upstairs", "get_clean_summary"},
		{"upstairs", "app_start"},
		{"upstairs", "app_pause"},
		{"upstairs", "app_charge"},
	}
	if len(runner.calls) != len(want) {
		t.Fatalf("calls = %+v, want %+v", runner.calls, want)
	}
	for i := range want {
		if runner.calls[i] != want[i] {
			t.Fatalf("call %d = %+v, want %+v", i, runner.calls[i], want[i])
		}
	}
}

func TestRoborockAppVacuumStartRetriesRateLimit(t *testing.T) {
	oldBackoff := roborockAppRateLimitBackoff
	roborockAppRateLimitBackoff = []time.Duration{0}
	t.Cleanup(func() { roborockAppRateLimitBackoff = oldBackoff })

	runner := &fakeRoborockAppRunner{
		resp: map[string]json.RawMessage{
			"app_start": json.RawMessage(`["ok"]`),
		},
		errSeq: []error{
			errors.New(`request too frequency - response code: 9002`),
			nil,
		},
	}
	v, err := NewRoborockAppVacuum("downstairs", "192.0.2.20", "downstairs", runner)
	if err != nil {
		t.Fatal(err)
	}

	if err := v.StartOrResume(context.Background(), false); err != nil {
		t.Fatalf("StartOrResume: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("app_start calls = %d, want 2", len(runner.calls))
	}
}
