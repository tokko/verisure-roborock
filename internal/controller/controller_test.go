package controller

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"verisure-roborock/internal/roborock"
	"verisure-roborock/internal/store"
	"verisure-roborock/internal/verisure"
)

// --- Mocks ---

type mockAlarm struct {
	state    verisure.ArmState
	err      error
	calls    int
	states   []verisure.ArmState // optional: per-call state sequence
	errs     []error             // optional: per-call err sequence
}

func (m *mockAlarm) ArmState(_ context.Context) (verisure.ArmState, error) {
	i := m.calls
	m.calls++
	if i < len(m.errs) || i < len(m.states) {
		var s verisure.ArmState
		var e error
		if i < len(m.states) {
			s = m.states[i]
		} else {
			s = m.state
		}
		if i < len(m.errs) {
			e = m.errs[i]
		}
		return s, e
	}
	return m.state, m.err
}

type mockVacuum struct {
	name         string
	host         string
	status       roborock.VacuumStatus
	statusErr    error
	summary      roborock.CleanSummary
	summaryErr   error
	startCalled  bool
	startPaused  bool
	startErr     error
	pauseCalled  bool
	pauseErr     error
	chargeCalled bool
	chargeErr    error
	statusCalls  int
}

func (m *mockVacuum) Name() string { return m.name }
func (m *mockVacuum) Host() string { return m.host }
func (m *mockVacuum) Status(_ context.Context) (roborock.VacuumStatus, error) {
	m.statusCalls++
	return m.status, m.statusErr
}
func (m *mockVacuum) CleanSummary(_ context.Context) (roborock.CleanSummary, error) {
	return m.summary, m.summaryErr
}
func (m *mockVacuum) StartOrResume(_ context.Context, paused bool) error {
	m.startCalled = true
	m.startPaused = paused
	return m.startErr
}
func (m *mockVacuum) Pause(_ context.Context) error {
	m.pauseCalled = true
	return m.pauseErr
}
func (m *mockVacuum) Charge(_ context.Context) error {
	m.chargeCalled = true
	return m.chargeErr
}

// --- Helpers ---

func newStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return st
}

func summaryAt(d time.Duration) roborock.CleanSummary {
	return roborock.CleanSummary{
		Records: []int64{time.Now().Add(-d).Unix()},
	}
}

func assertBool(t *testing.T, name string, got, want bool) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", name, got, want)
	}
}

// --- TestMaybeStartVacuum ---

func TestMaybeStartVacuum(t *testing.T) {
	cooldown := 24 * time.Hour

	cases := []struct {
		name        string
		status      roborock.VacuumStatus
		statusErr   error
		summary     roborock.CleanSummary
		summaryErr  error
		wantStarted bool
		wantPaused  bool
		wantErr     bool
		wantStartFn bool // whether StartOrResume should have been called
	}{
		{
			name:        "vacuum in error state -> not started",
			status:      roborock.VacuumStatus{State: roborock.StateError, ErrorCode: 42},
			summary:     summaryAt(48 * time.Hour),
			wantStarted: false,
			wantStartFn: false,
		},
		{
			name:        "vacuum returning -> not started",
			status:      roborock.VacuumStatus{State: roborock.StateReturning},
			summary:     summaryAt(48 * time.Hour),
			wantStarted: false,
			wantStartFn: false,
		},
		{
			name:        "vacuum already cleaning -> not started",
			status:      roborock.VacuumStatus{State: roborock.StateCleaning, Battery: 80},
			summary:     summaryAt(48 * time.Hour),
			wantStarted: false,
			wantStartFn: false,
		},
		{
			name:        "idle, last clean within cooldown -> not started",
			status:      roborock.VacuumStatus{State: roborock.StateIdle, Battery: 80},
			summary:     summaryAt(2 * time.Hour),
			wantStarted: false,
			wantStartFn: false,
		},
		{
			name:        "idle, last clean past cooldown -> started (app_start)",
			status:      roborock.VacuumStatus{State: roborock.StateIdle, Battery: 80},
			summary:     summaryAt(30 * time.Hour),
			wantStarted: true,
			wantPaused:  false,
			wantStartFn: true,
		},
		{
			name:        "paused, last clean past cooldown -> resumed (app_resume)",
			status:      roborock.VacuumStatus{State: roborock.StatePaused, Battery: 80},
			summary:     summaryAt(30 * time.Hour),
			wantStarted: true,
			wantPaused:  true,
			wantStartFn: true,
		},
		{
			name:        "idle, no clean history -> started (zero time)",
			status:      roborock.VacuumStatus{State: roborock.StateIdle, Battery: 80},
			summary:     roborock.CleanSummary{}, // empty Records
			wantStarted: true,
			wantStartFn: true,
		},
		{
			name:        "Status returns error -> error propagated",
			statusErr:   errors.New("status failed"),
			wantStarted: false,
			wantErr:     true,
			wantStartFn: false,
		},
		{
			name:        "CleanSummary returns error -> error propagated",
			status:      roborock.VacuumStatus{State: roborock.StateIdle, Battery: 80},
			summaryErr:  errors.New("summary failed"),
			wantStarted: false,
			wantErr:     true,
			wantStartFn: false,
		},
		{
			name:        "Low battery -> still started (no battery guard)",
			status:      roborock.VacuumStatus{State: roborock.StateIdle, Battery: 5},
			summary:     summaryAt(30 * time.Hour),
			wantStarted: true,
			wantStartFn: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &mockVacuum{
				name:       "test",
				host:       "1.2.3.4",
				status:     tc.status,
				statusErr:  tc.statusErr,
				summary:    tc.summary,
				summaryErr: tc.summaryErr,
			}
			c := New(&mockAlarm{}, []VacuumCommander{v}, newStore(t), time.Second, cooldown)

			started, err := c.maybeStartVacuum(context.Background(), v)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertBool(t, "started", started, tc.wantStarted)
			assertBool(t, "StartOrResume called", v.startCalled, tc.wantStartFn)
			if tc.wantStartFn {
				assertBool(t, "paused arg", v.startPaused, tc.wantPaused)
			}
		})
	}
}

// --- TestOnArmedAway ---

func TestOnArmedAway(t *testing.T) {
	cooldown := 24 * time.Hour

	t.Run("two vacuums both need clean -> both started, CleaningActive", func(t *testing.T) {
		v1 := &mockVacuum{name: "v1", host: "1.1.1.1",
			status: roborock.VacuumStatus{State: roborock.StateIdle},
			summary: summaryAt(30 * time.Hour)}
		v2 := &mockVacuum{name: "v2", host: "2.2.2.2",
			status: roborock.VacuumStatus{State: roborock.StateIdle},
			summary: summaryAt(30 * time.Hour)}
		c := New(&mockAlarm{}, []VacuumCommander{v1, v2}, newStore(t), time.Second, cooldown)
		c.onArmedAway(context.Background())
		assertBool(t, "v1 started", v1.startCalled, true)
		assertBool(t, "v2 started", v2.startCalled, true)
		if c.State() != StateCleaningActive {
			t.Errorf("state: got %v, want CleaningActive", c.State())
		}
	})

	t.Run("one needs clean, one within cooldown -> one started, CleaningActive", func(t *testing.T) {
		v1 := &mockVacuum{name: "v1", host: "1.1.1.1",
			status: roborock.VacuumStatus{State: roborock.StateIdle},
			summary: summaryAt(30 * time.Hour)}
		v2 := &mockVacuum{name: "v2", host: "2.2.2.2",
			status: roborock.VacuumStatus{State: roborock.StateIdle},
			summary: summaryAt(2 * time.Hour)}
		c := New(&mockAlarm{}, []VacuumCommander{v1, v2}, newStore(t), time.Second, cooldown)
		c.onArmedAway(context.Background())
		assertBool(t, "v1 started", v1.startCalled, true)
		assertBool(t, "v2 started", v2.startCalled, false)
		if c.State() != StateCleaningActive {
			t.Errorf("state: got %v, want CleaningActive", c.State())
		}
	})

	t.Run("all within cooldown -> none started, ArmedAway", func(t *testing.T) {
		v1 := &mockVacuum{name: "v1", host: "1.1.1.1",
			status: roborock.VacuumStatus{State: roborock.StateIdle},
			summary: summaryAt(2 * time.Hour)}
		v2 := &mockVacuum{name: "v2", host: "2.2.2.2",
			status: roborock.VacuumStatus{State: roborock.StateIdle},
			summary: summaryAt(2 * time.Hour)}
		c := New(&mockAlarm{}, []VacuumCommander{v1, v2}, newStore(t), time.Second, cooldown)
		c.onArmedAway(context.Background())
		assertBool(t, "v1 started", v1.startCalled, false)
		assertBool(t, "v2 started", v2.startCalled, false)
		if c.State() != StateArmedAway {
			t.Errorf("state: got %v, want ArmedAway", c.State())
		}
	})

	t.Run("one Status error, other still started", func(t *testing.T) {
		v1 := &mockVacuum{name: "v1", host: "1.1.1.1",
			statusErr: errors.New("boom")}
		v2 := &mockVacuum{name: "v2", host: "2.2.2.2",
			status: roborock.VacuumStatus{State: roborock.StateIdle},
			summary: summaryAt(30 * time.Hour)}
		c := New(&mockAlarm{}, []VacuumCommander{v1, v2}, newStore(t), time.Second, cooldown)
		c.onArmedAway(context.Background())
		assertBool(t, "v1 started", v1.startCalled, false)
		assertBool(t, "v2 started", v2.startCalled, true)
		if c.State() != StateCleaningActive {
			t.Errorf("state: got %v, want CleaningActive", c.State())
		}
	})

	t.Run("all error -> ArmedAway", func(t *testing.T) {
		v1 := &mockVacuum{name: "v1", host: "1.1.1.1", statusErr: errors.New("boom")}
		v2 := &mockVacuum{name: "v2", host: "2.2.2.2", statusErr: errors.New("boom")}
		c := New(&mockAlarm{}, []VacuumCommander{v1, v2}, newStore(t), time.Second, cooldown)
		c.onArmedAway(context.Background())
		assertBool(t, "v1 started", v1.startCalled, false)
		assertBool(t, "v2 started", v2.startCalled, false)
		if c.State() != StateArmedAway {
			t.Errorf("state: got %v, want ArmedAway", c.State())
		}
	})
}

// --- TestOnDisengaged ---

func TestOnDisengaged(t *testing.T) {
	t.Run("CleaningActive + StartedByUs + active -> Pause+Charge", func(t *testing.T) {
		v := &mockVacuum{name: "v", host: "1.1.1.1",
			status: roborock.VacuumStatus{State: roborock.StateCleaning}}
		st := newStore(t)
		if err := st.SetVacuumStartedByUs(v.host, true); err != nil {
			t.Fatal(err)
		}
		c := New(&mockAlarm{}, []VacuumCommander{v}, st, time.Second, time.Hour)
		c.state = StateCleaningActive
		c.onDisengaged(context.Background())
		assertBool(t, "pause called", v.pauseCalled, true)
		assertBool(t, "charge called", v.chargeCalled, true)
		if c.State() != StateIdle {
			t.Errorf("state: got %v, want Idle", c.State())
		}
	})

	t.Run("CleaningActive + !StartedByUs -> no Pause/Charge", func(t *testing.T) {
		v := &mockVacuum{name: "v", host: "1.1.1.1",
			status: roborock.VacuumStatus{State: roborock.StateCleaning}}
		st := newStore(t)
		c := New(&mockAlarm{}, []VacuumCommander{v}, st, time.Second, time.Hour)
		c.state = StateCleaningActive
		c.onDisengaged(context.Background())
		assertBool(t, "pause called", v.pauseCalled, false)
		assertBool(t, "charge called", v.chargeCalled, false)
		if c.State() != StateIdle {
			t.Errorf("state: got %v, want Idle", c.State())
		}
	})

	t.Run("not CleaningActive -> idle, no action", func(t *testing.T) {
		v := &mockVacuum{name: "v", host: "1.1.1.1",
			status: roborock.VacuumStatus{State: roborock.StateCleaning}}
		st := newStore(t)
		if err := st.SetVacuumStartedByUs(v.host, true); err != nil {
			t.Fatal(err)
		}
		c := New(&mockAlarm{}, []VacuumCommander{v}, st, time.Second, time.Hour)
		c.state = StateArmedAway
		c.onDisengaged(context.Background())
		assertBool(t, "pause called", v.pauseCalled, false)
		assertBool(t, "charge called", v.chargeCalled, false)
		if c.State() != StateIdle {
			t.Errorf("state: got %v, want Idle", c.State())
		}
	})

	t.Run("CleaningActive + vacuum already docked -> no Pause/Charge", func(t *testing.T) {
		v := &mockVacuum{name: "v", host: "1.1.1.1",
			status: roborock.VacuumStatus{State: roborock.StateCharging}}
		st := newStore(t)
		if err := st.SetVacuumStartedByUs(v.host, true); err != nil {
			t.Fatal(err)
		}
		c := New(&mockAlarm{}, []VacuumCommander{v}, st, time.Second, time.Hour)
		c.state = StateCleaningActive
		c.onDisengaged(context.Background())
		assertBool(t, "pause called", v.pauseCalled, false)
		assertBool(t, "charge called", v.chargeCalled, false)
	})
}

// --- TestReconcileOnStartup ---

func TestReconcileOnStartup(t *testing.T) {
	t.Run("DISARMED + StartedByUs + active -> stop", func(t *testing.T) {
		v := &mockVacuum{name: "v", host: "1.1.1.1",
			status: roborock.VacuumStatus{State: roborock.StateCleaning}}
		st := newStore(t)
		if err := st.SetVacuumStartedByUs(v.host, true); err != nil {
			t.Fatal(err)
		}
		alarm := &mockAlarm{state: verisure.ArmStateDisarmed}
		c := New(alarm, []VacuumCommander{v}, st, time.Second, time.Hour)
		c.reconcileOnStartup(context.Background())
		assertBool(t, "pause called", v.pauseCalled, true)
		assertBool(t, "charge called", v.chargeCalled, true)
		if c.State() != StateIdle {
			t.Errorf("state: got %v, want Idle", c.State())
		}
	})

	t.Run("ARMED_AWAY + StartedByUs + idle -> restart", func(t *testing.T) {
		v := &mockVacuum{name: "v", host: "1.1.1.1",
			status: roborock.VacuumStatus{State: roborock.StateIdle}}
		st := newStore(t)
		if err := st.SetVacuumStartedByUs(v.host, true); err != nil {
			t.Fatal(err)
		}
		alarm := &mockAlarm{state: verisure.ArmStateArmedAway}
		c := New(alarm, []VacuumCommander{v}, st, time.Second, time.Hour)
		c.reconcileOnStartup(context.Background())
		assertBool(t, "start called", v.startCalled, true)
		if c.State() != StateCleaningActive {
			t.Errorf("state: got %v, want CleaningActive", c.State())
		}
	})

	t.Run("ARMED_AWAY + StartedByUs + cleaning -> no action", func(t *testing.T) {
		v := &mockVacuum{name: "v", host: "1.1.1.1",
			status: roborock.VacuumStatus{State: roborock.StateCleaning}}
		st := newStore(t)
		if err := st.SetVacuumStartedByUs(v.host, true); err != nil {
			t.Fatal(err)
		}
		alarm := &mockAlarm{state: verisure.ArmStateArmedAway}
		c := New(alarm, []VacuumCommander{v}, st, time.Second, time.Hour)
		c.reconcileOnStartup(context.Background())
		assertBool(t, "start called", v.startCalled, false)
		assertBool(t, "pause called", v.pauseCalled, false)
		assertBool(t, "charge called", v.chargeCalled, false)
		if c.State() != StateCleaningActive {
			t.Errorf("state: got %v, want CleaningActive", c.State())
		}
	})

	t.Run("alarm fetch fails -> reconcile gives up via ctx cancel", func(t *testing.T) {
		// We can't realistically wait for all 12 attempts (5s+ backoffs).
		// Instead, verify reconcile handles persistent failures by cancelling ctx mid-attempt;
		// the key behavior is: it returns without panicking and without taking vacuum actions.
		v := &mockVacuum{name: "v", host: "1.1.1.1"}
		st := newStore(t)
		if err := st.SetVacuumStartedByUs(v.host, true); err != nil {
			t.Fatal(err)
		}
		alarm := &mockAlarm{err: errors.New("verisure down")}
		c := New(alarm, []VacuumCommander{v}, st, time.Second, time.Hour)

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel immediately so backoff wait returns quickly
		c.reconcileOnStartup(ctx)

		assertBool(t, "start called", v.startCalled, false)
		assertBool(t, "pause called", v.pauseCalled, false)
		if alarm.calls < 1 {
			t.Errorf("expected at least 1 alarm fetch attempt, got %d", alarm.calls)
		}
	})

	t.Run("no vacuums with StartedByUs -> no actions", func(t *testing.T) {
		v := &mockVacuum{name: "v", host: "1.1.1.1",
			status: roborock.VacuumStatus{State: roborock.StateCleaning}}
		st := newStore(t)
		alarm := &mockAlarm{state: verisure.ArmStateArmedAway}
		c := New(alarm, []VacuumCommander{v}, st, time.Second, time.Hour)
		c.reconcileOnStartup(context.Background())
		assertBool(t, "start called", v.startCalled, false)
		assertBool(t, "pause called", v.pauseCalled, false)
		assertBool(t, "charge called", v.chargeCalled, false)
		if v.statusCalls != 0 {
			t.Errorf("expected no Status calls, got %d", v.statusCalls)
		}
	})
}

// --- TestPoll ---

func TestPoll(t *testing.T) {
	t.Run("alarm unchanged -> no transition", func(t *testing.T) {
		v := &mockVacuum{name: "v", host: "1.1.1.1"}
		alarm := &mockAlarm{state: verisure.ArmStateDisarmed}
		c := New(alarm, []VacuumCommander{v}, newStore(t), time.Second, time.Hour)
		c.lastAlarm = verisure.ArmStateDisarmed
		ok := c.poll(context.Background())
		assertBool(t, "poll ok", ok, true)
		assertBool(t, "start called", v.startCalled, false)
		assertBool(t, "pause called", v.pauseCalled, false)
	})

	t.Run("DISARMED -> ARMED_AWAY triggers onArmedAway", func(t *testing.T) {
		v := &mockVacuum{name: "v", host: "1.1.1.1",
			status: roborock.VacuumStatus{State: roborock.StateIdle},
			summary: summaryAt(30 * time.Hour)}
		alarm := &mockAlarm{state: verisure.ArmStateArmedAway}
		c := New(alarm, []VacuumCommander{v}, newStore(t), time.Second, time.Hour)
		c.lastAlarm = verisure.ArmStateDisarmed
		ok := c.poll(context.Background())
		assertBool(t, "poll ok", ok, true)
		assertBool(t, "start called", v.startCalled, true)
		if c.State() != StateCleaningActive {
			t.Errorf("state: got %v, want CleaningActive", c.State())
		}
	})

	t.Run("ARMED_AWAY -> DISARMED triggers onDisengaged", func(t *testing.T) {
		v := &mockVacuum{name: "v", host: "1.1.1.1",
			status: roborock.VacuumStatus{State: roborock.StateCleaning}}
		st := newStore(t)
		if err := st.SetVacuumStartedByUs(v.host, true); err != nil {
			t.Fatal(err)
		}
		alarm := &mockAlarm{state: verisure.ArmStateDisarmed}
		c := New(alarm, []VacuumCommander{v}, st, time.Second, time.Hour)
		c.lastAlarm = verisure.ArmStateArmedAway
		c.state = StateCleaningActive
		ok := c.poll(context.Background())
		assertBool(t, "poll ok", ok, true)
		assertBool(t, "pause called", v.pauseCalled, true)
		assertBool(t, "charge called", v.chargeCalled, true)
		if c.State() != StateIdle {
			t.Errorf("state: got %v, want Idle", c.State())
		}
	})

	t.Run("alarm fetch error -> poll returns false", func(t *testing.T) {
		v := &mockVacuum{name: "v", host: "1.1.1.1"}
		alarm := &mockAlarm{err: errors.New("boom")}
		c := New(alarm, []VacuumCommander{v}, newStore(t), time.Second, time.Hour)
		ok := c.poll(context.Background())
		assertBool(t, "poll ok", ok, false)
	})
}

// --- TestBackoffDuration ---

func TestBackoffDuration(t *testing.T) {
	cases := []struct {
		name    string
		attempt int
		base    time.Duration
		max     time.Duration
		want    time.Duration
	}{
		{"attempt=0 base=5s -> 5s", 0, 5 * time.Second, 5 * time.Minute, 5 * time.Second},
		{"attempt=1 base=5s -> 10s", 1, 5 * time.Second, 5 * time.Minute, 10 * time.Second},
		{"attempt=10 base=5s -> capped at 5min", 10, 5 * time.Second, 5 * time.Minute, 5 * time.Minute},
		{"attempt=100 -> still capped at 5min", 100, 5 * time.Second, 5 * time.Minute, 5 * time.Minute},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := backoffDuration(tc.attempt, tc.base, tc.max)
			if got != tc.want {
				t.Errorf("backoffDuration(%d, %v, %v) = %v, want %v",
					tc.attempt, tc.base, tc.max, got, tc.want)
			}
		})
	}
}
