package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"verisure-roborock/internal/roborock"
	"verisure-roborock/internal/store"
	"verisure-roborock/internal/verisure"
)

// ControlState is the controller's state machine state.
type ControlState int

const (
	StateIdle           ControlState = iota // alarm disarmed, nothing active
	StateArmedAway                          // alarm armed-away, vacuums idle or skipped
	StateCleaningActive                     // at least one vacuum running under our control
)

func (s ControlState) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateArmedAway:
		return "armed_away"
	case StateCleaningActive:
		return "cleaning_active"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// AlarmSource abstracts Verisure for testing.
type AlarmSource interface {
	ArmState(ctx context.Context) (verisure.ArmState, error)
}

// VacuumCommander abstracts a single Roborock vacuum for testing.
type VacuumCommander interface {
	Name() string
	Host() string
	Status(ctx context.Context) (roborock.VacuumStatus, error)
	CleanSummary(ctx context.Context) (roborock.CleanSummary, error)
	StartOrResume(ctx context.Context, paused bool) error
	Pause(ctx context.Context) error
	Charge(ctx context.Context) error
}

// Controller owns the main poll loop and state machine.
type Controller struct {
	alarm         AlarmSource
	vacuums       []VacuumCommander
	store         *store.Store
	pollInterval  time.Duration
	cleanCooldown time.Duration

	state        ControlState
	lastAlarm    verisure.ArmState
}

// New creates a Controller.
func New(
	alarm AlarmSource,
	vacuums []VacuumCommander,
	st *store.Store,
	pollInterval time.Duration,
	cleanCooldown time.Duration,
) *Controller {
	return &Controller{
		alarm:         alarm,
		vacuums:       vacuums,
		store:         st,
		pollInterval:  pollInterval,
		cleanCooldown: cleanCooldown,
	}
}

// Run reconciles state on startup then enters the poll loop.
// It blocks until ctx is cancelled.
//
// The poll interval backs off exponentially on consecutive Verisure errors
// (base = pollInterval, cap = 5 min) and resets on the next success.
// This prevents hammering Verisure when they are rate-limiting or down.
func (c *Controller) Run(ctx context.Context) error {
	slog.Info("controller: starting")
	c.reconcileOnStartup(ctx)

	failures := 0
	for {
		wait := backoffDuration(failures, c.pollInterval, 5*time.Minute)
		if failures > 0 {
			slog.Debug("controller: poll back-off", "failures", failures, "wait", wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}

		if c.poll(ctx) {
			failures = 0
		} else {
			failures++
		}
	}
}

// State returns the current controller state (for the /status handler).
func (c *Controller) State() ControlState { return c.state }

// poll fetches the current alarm state and triggers transitions on change.
// Returns true on success, false on error.
func (c *Controller) poll(ctx context.Context) bool {
	alarm, err := c.alarm.ArmState(ctx)
	if err != nil {
		slog.Warn("controller: poll failed", "err", err)
		return false
	}

	if alarm == c.lastAlarm {
		return true
	}

	slog.Info("controller: alarm state changed", "from", c.lastAlarm, "to", alarm)
	c.lastAlarm = alarm
	if err := c.store.SetAlarmState(string(alarm)); err != nil {
		slog.Error("controller: store alarm state", "err", err)
	}

	if alarm.IsArmedAway() {
		c.onArmedAway(ctx)
	} else if alarm.IsDisengaged() {
		c.onDisengaged(ctx)
	}
	return true
}

// onArmedAway iterates all vacuums and starts/resumes any that need cleaning.
func (c *Controller) onArmedAway(ctx context.Context) {
	slog.Info("controller: alarm armed-away — checking vacuums")
	anyStarted := false

	for _, v := range c.vacuums {
		started, err := c.maybeStartVacuum(ctx, v)
		if err != nil {
			slog.Error("controller: vacuum check failed", "name", v.Name(), "err", err)
			continue
		}
		if started {
			anyStarted = true
		}
	}

	newState := StateArmedAway
	if anyStarted {
		newState = StateCleaningActive
	}
	c.setState(newState)
}

// maybeStartVacuum checks and optionally starts a single vacuum.
// Returns true if we started it.
func (c *Controller) maybeStartVacuum(ctx context.Context, v VacuumCommander) (bool, error) {
	status, err := v.Status(ctx)
	if err != nil {
		return false, fmt.Errorf("get status: %w", err)
	}

	slog.Debug("controller: vacuum status", "name", v.Name(), "state", status.State, "battery", status.Battery)

	switch {
	case status.State.IsError():
		slog.Warn("controller: vacuum in error state, skipping", "name", v.Name(), "code", status.ErrorCode)
		return false, nil

	case status.State.IsInTransit():
		slog.Info("controller: vacuum returning/docking, skipping", "name", v.Name())
		return false, nil

	case status.State.IsActiveClean():
		// Already cleaning — not started by us (we didn't set StartedByUs).
		slog.Info("controller: vacuum already cleaning (manual), noting", "name", v.Name())
		return false, nil
	}

	// Check cleaning history.
	summary, err := v.CleanSummary(ctx)
	if err != nil {
		// Safe default: don't start if we don't know the history.
		return false, fmt.Errorf("get clean summary: %w", err)
	}

	lastClean := summary.LastCleanTime()
	if !lastClean.IsZero() && time.Since(lastClean) < c.cleanCooldown {
		slog.Info("controller: recent clean found, skipping",
			"name", v.Name(),
			"last_clean", lastClean,
			"age", time.Since(lastClean).Round(time.Minute),
		)
		return false, nil
	}

	if lastClean.IsZero() {
		slog.Info("controller: no clean history — starting first clean", "name", v.Name())
	} else {
		slog.Info("controller: last clean was too long ago — starting",
			"name", v.Name(),
			"last_clean", lastClean,
			"age", time.Since(lastClean).Round(time.Minute),
		)
	}

	if err := v.StartOrResume(ctx, status.State.IsPaused()); err != nil {
		return false, fmt.Errorf("start: %w", err)
	}

	if err := c.store.SetVacuumStartedByUs(v.Host(), true); err != nil {
		slog.Error("controller: persist StartedByUs", "name", v.Name(), "err", err)
	}
	return true, nil
}

// onDisengaged iterates all vacuums we started and pauses + docks them.
func (c *Controller) onDisengaged(ctx context.Context) {
	if c.state != StateCleaningActive {
		slog.Info("controller: alarm disengaged but nothing active — no action")
		c.setState(StateIdle)
		return
	}

	slog.Info("controller: alarm disengaged — stopping vacuums we started")
	st := c.store.Get()

	for _, v := range c.vacuums {
		entry := st.Vacuums[v.Host()]
		if !entry.StartedByUs {
			continue
		}
		c.stopVacuum(ctx, v)
	}

	c.setState(StateIdle)
}

// stopVacuum pauses and docks a single vacuum. Errors are logged, not returned.
func (c *Controller) stopVacuum(ctx context.Context, v VacuumCommander) {
	status, err := v.Status(ctx)
	if err != nil {
		slog.Warn("controller: cannot get status before stop, attempting pause anyway", "name", v.Name(), "err", err)
	} else if !status.State.IsActiveClean() && !status.State.IsPaused() {
		slog.Info("controller: vacuum not cleaning, skipping stop", "name", v.Name(), "state", status.State)
		if persistErr := c.store.SetVacuumStartedByUs(v.Host(), false); persistErr != nil {
			slog.Error("controller: persist StartedByUs=false", "err", persistErr)
		}
		return
	}

	if err := withRetrySingle(ctx, 3, 2*time.Second, 30*time.Second, func() error {
		return v.Pause(ctx)
	}); err != nil {
		slog.Error("controller: pause failed", "name", v.Name(), "err", err)
		// Continue to try charge anyway.
	}

	if err := withRetrySingle(ctx, 3, 2*time.Second, 30*time.Second, func() error {
		return v.Charge(ctx)
	}); err != nil {
		slog.Error("controller: charge (return to dock) failed", "name", v.Name(), "err", err)
	}

	if err := c.store.SetVacuumStartedByUs(v.Host(), false); err != nil {
		slog.Error("controller: persist StartedByUs=false", "err", err)
	}
}

// reconcileOnStartup corrects state after a restart.
// It reads the persisted state, fetches current alarm + vacuum states,
// and acts to bring the world into consistency.
func (c *Controller) reconcileOnStartup(ctx context.Context) {
	slog.Info("controller: reconciling state on startup")
	st := c.store.Get()

	// Fetch current alarm state (retry with backoff, give up after ~30 min so
	// the poll loop can take over rather than blocking startup indefinitely).
	const maxReconcileAttempts = 12
	var alarm verisure.ArmState
	var err error
	for attempt := 0; attempt < maxReconcileAttempts; attempt++ {
		alarm, err = c.alarm.ArmState(ctx)
		if err == nil {
			break
		}
		if attempt == maxReconcileAttempts-1 {
			slog.Warn("controller: reconcile giving up after too many failures, poll loop will correct state",
				"attempts", maxReconcileAttempts)
			return
		}
		wait := backoffDuration(attempt, 5*time.Second, 5*time.Minute)
		slog.Warn("controller: reconcile alarm fetch failed", "err", err, "retry_in", wait,
			"attempt", attempt+1, "max", maxReconcileAttempts)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}

	c.lastAlarm = alarm
	slog.Info("controller: current alarm state", "state", alarm)

	anyActive := false
	for _, v := range c.vacuums {
		entry := st.Vacuums[v.Host()]
		if !entry.StartedByUs {
			continue
		}

		slog.Info("controller: reconciling vacuum we previously started", "name", v.Name())

		status, err := v.Status(ctx)
		if err != nil {
			slog.Warn("controller: cannot get vacuum status during reconcile, will correct on next poll",
				"name", v.Name(), "err", err)
			continue
		}

		switch {
		case alarm.IsDisengaged() && (status.State.IsActiveClean() || status.State.IsPaused()):
			// We were down while the alarm was disarmed — stop the vacuum.
			slog.Warn("controller: vacuum still running after alarm disarmed, stopping", "name", v.Name())
			c.stopVacuum(ctx, v)

		case alarm.IsArmedAway() && (status.State.IsIdle() || status.State.IsError()):
			// We crashed mid-clean and the vacuum stopped — restart it.
			slog.Warn("controller: vacuum stopped mid-clean, restarting", "name", v.Name())
			if err := v.StartOrResume(ctx, false); err != nil {
				slog.Error("controller: restart failed", "name", v.Name(), "err", err)
			} else {
				anyActive = true
			}

		case alarm.IsArmedAway() && status.State.IsActiveClean():
			slog.Info("controller: vacuum still cleaning — all good", "name", v.Name())
			anyActive = true
		}
	}

	if anyActive {
		c.setState(StateCleaningActive)
	} else if alarm.IsArmedAway() {
		c.setState(StateArmedAway)
	} else {
		c.setState(StateIdle)
	}

	// Persist the initial alarm state so /status shows it immediately.
	if err := c.store.SetAlarmState(string(alarm)); err != nil {
		slog.Error("controller: persist alarm state on reconcile", "err", err)
	}
	slog.Info("controller: reconcile complete", "state", c.state, "alarm", alarm)
}

func (c *Controller) setState(s ControlState) {
	c.state = s
	if err := c.store.SetControlState(s.String()); err != nil {
		slog.Error("controller: persist control state", "err", err)
	}
	slog.Debug("controller: state →", "state", s)
}

// --- Retry helpers ---

// backoffDuration computes exponential backoff capped at max.
func backoffDuration(attempt int, base, max time.Duration) time.Duration {
	if attempt > 10 {
		attempt = 10
	}
	d := base * (1 << attempt)
	if d > max {
		return max
	}
	return d
}

// withRetry retries fn up to attempts times, returning the last error.
func withRetry[T any](ctx context.Context, attempts int, base, max time.Duration, fn func() (T, error)) (T, error) {
	var zero T
	var err error
	for i := range attempts {
		var val T
		val, err = fn()
		if err == nil {
			return val, nil
		}
		if i < attempts-1 {
			wait := backoffDuration(i, base, max)
			slog.Debug("retry", "attempt", i+1, "err", err, "wait", wait)
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(wait):
			}
		}
	}
	return zero, err
}

// withRetrySingle is withRetry for functions that return only an error.
func withRetrySingle(ctx context.Context, attempts int, base, max time.Duration, fn func() error) error {
	_, err := withRetry(ctx, attempts, base, max, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
}
