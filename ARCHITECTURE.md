# verisure-roborock — Architecture

## Purpose

A single Go binary that polls the Verisure home alarm and automates all configured Roborock vacuums:

- **Alarm armed-away** → for each vacuum, if last full clean was >24h ago, start (or resume) it
- **Alarm disarmed** → for each vacuum we started, pause it and return it to dock
- **On startup** → reconcile persisted state against current alarm + vacuum states before entering the poll loop

---

## Component Overview

```
┌─────────────────────────────────────────────────────────────┐
│                          main.go                            │
│  Loads config → creates clients → calls controller.Run()   │
│  Starts HTTP status server (:8080) in background goroutine  │
└─────────────────────────────┬───────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────┐
│                       controller                            │
│  Owns the Verisure poll loop (no channel indirection)       │
│  Explicit state machine: reconcile → poll → react          │
│  onArmedAway() / onDisengaged() / reconcileOnStartup()     │
└────────┬────────────────────────────┬───────────────────────┘
         │                            │
         ▼                            ▼
┌──────────────────┐    ┌───────────────────────────────────┐
│ verisure.Client  │    │       []roborock.Client           │
│ Session auth     │    │ One per configured vacuum         │
│ GET arm state    │    │ Xiaomi Cloud RPC or UDP miIO      │
│ Auto re-login    │    └───────────────────────────────────┘
└──────────────────┘                  │
                                      ▼
                         ┌───────────────────────────────────┐
                         │            store                  │
                         │  Atomic JSON, per-vacuum state    │
                         │  last_clean_time (per vacuum)     │
                         │  started_by_us (per vacuum)       │
                         │  alarm_state, control_state       │
                         └───────────────────────────────────┘
```

**Key architectural decision:** The controller owns the Verisure poll loop directly (no goroutine + channel). This eliminates the dropped-transition race: if `onArmedAway` is executing and the alarm changes, the new state is seen on the *next* poll iteration. Poll interval is short enough (default 60s) that this is acceptable — a one-cycle delay on disarm is fine.

---

## Package Layout

```
verisure-roborock/
├── cmd/
│   └── verisure-roborock/
│       └── main.go              # wiring, signal handling, run()
├── internal/
│   ├── config/
│   │   └── config.go            # env loading, multi-vacuum config, validation
│   ├── verisure/
│   │   ├── client.go            # HTTP client, auth, arm state
│   │   └── types.go             # ArmState enum, response structs
│   ├── roborock/
│   │   ├── client.go            # UDP socket, high-level commands
│   │   ├── cloud_client.go      # Xiaomi Cloud RPC high-level commands
│   │   ├── miio.go              # binary frame encode/decode, AES, checksum
│   │   └── types.go             # VacuumState enum, status/command types
│   ├── controller/
│   │   └── controller.go        # state machine, reconcile, poll loop
│   └── store/
│       └── store.go             # atomic JSON persistence, per-vacuum entries
├── systemd/
│   └── verisure-roborock.service
├── .env.example
├── .gitignore
├── Makefile
├── go.mod
└── ARCHITECTURE.md
```

Module name: `verisure-roborock` (no GitHub path needed for a local binary)

---

## State Machine

### Controller States

```go
type ControlState int

const (
    StateIdle           ControlState = iota // alarm disarmed, nothing active
    StateArmedAway                           // alarm armed, vacuums idle or skipped
    StateCleaningActive                      // at least one vacuum running under our control
)
```

### Transitions

```
startup → reconcileOnStartup() → correct state

DISARMED/ARMED_HOME → StateIdle
    │
    │ ARMED_AWAY
    ▼
StateArmedAway ──[any vacuum needs clean]──► StateCleaningActive
    │                                                │
    │ [all vacuums skipped]                          │ DISARMED/ARMED_HOME
    │                                                ▼
    │                              for each vacuum: pause() + charge() → StateIdle
    │ DISARMED/ARMED_HOME
    ▼
StateIdle (no vacuum action)
```

### Startup Reconciliation

On startup, before entering the poll loop, `reconcileOnStartup()`:

1. Read store state (alarm state, per-vacuum `started_by_us`)
2. Fetch current alarm state from Verisure
3. For each vacuum where `started_by_us == true`:
   - Fetch vacuum status
   - **If alarm is now DISARMED and vacuum is active/paused**: pause + charge (we were down, alarm disarmed, must dock)
   - **If alarm is still ARMED_AWAY and vacuum is idle/error**: attempt restart (we crashed mid-clean)
   - **If alarm is still ARMED_AWAY and vacuum is active**: set `StateCleaningActive`, nothing else needed
4. Set controller state to match reality, persist

This ensures the process never leaves a vacuum running unattended after a restart.

---

## Multi-Vacuum Support

### Configuration

Vacuums are configured as a numbered list of env vars:

```
ROBOROCK_0_HOST=192.168.1.50
ROBOROCK_0_TOKEN=aabbcc...
ROBOROCK_0_NAME=kitchen          # optional label for logs

ROBOROCK_1_HOST=192.168.1.51
ROBOROCK_1_TOKEN=ddeeff...
ROBOROCK_1_NAME=upstairs
```

`config.Load()` scans `ROBOROCK_0_*`, `ROBOROCK_1_*`, ... until a gap. Minimum 1 vacuum required.

### Per-Vacuum Store Entry

```go
type VacuumState struct {
    Name          string    `json:"name"`
    LastCleanTime time.Time `json:"last_clean_time"` // last confirmed completed clean
    StartedByUs   bool      `json:"started_by_us"`   // did this process start the current run?
}

type State struct {
    AlarmState   string                 `json:"alarm_state"`
    ControlState string                 `json:"control_state"`
    Vacuums      map[string]VacuumState `json:"vacuums"` // key: vacuum host IP
}
```

### Controller loop over vacuums

`onArmedAway` and `onDisengaged` iterate over all `[]roborock.Client` and operate on each independently. A failure on one vacuum is logged but does not abort processing of others.

---

## API Integration

### Verisure (REST, unofficial)

**Base URL:** `https://e-api01.verisure.com/xbn/2` (configurable via `VERISURE_BASE_URL`)

**Auth flow:**
1. `POST /cookie` — body `{"login": email, "password": password}` → sets `vid` session cookie in `http.CookieJar`
2. Optional 2FA: if `VERISURE_OTP_SECRET` is set, compute TOTP (RFC 6238, SHA-1, 30s window, 6 digits) and `POST /multifactor/cookie`
3. `GET /installation` → discover `giid` (installation ID) if `VERISURE_GIID` not set

**Session refresh:** HTTP 401/403 → re-login once under a `sync.Mutex`. If re-login fails, error propagates to poll loop which backs off.

**Polling:** Ticker at `POLL_INTERVAL`. Controller compares new state to previous; calls `onArmedAway` or `onDisengaged` only on state change.

**Endpoint:**
```
GET /installation/{giid}/armstate
→ {"data": {"state": "ARMED_AWAY" | "ARMED_HOME" | "DISARMED", ...}}
```

### Roborock Control

Default runtime control uses Xiaomi Cloud RPC (`/home/rpc/{did}`) and the same miIO method names as local control. The local UDP miIO transport remains available with `ROBOROCK_CONTROL=local`.

### Roborock (local miIO, UDP port 54321)

**miIO binary frame** (big-endian, 32-byte header):
```
Bytes  Field
0-1    Magic: 0x2131
2-3    Total packet length (header + payload)
4-7    Device ID (from hello response)
8-11   Stamp (Unix timestamp from hello response — NOT system clock)
12-27  MD5(header[0:16 with checksum=0] + token + payload)
28+    AES-128-CBC encrypted JSON payload
```

**Key derivation** (from raw 16-byte token, computed once at startup):
- `key = md5(token)`
- `iv  = md5(key + token)`

**Handshake:** Send 32-byte zero hello packet → get device ID and stamp. Re-handshake **on error only** (not on a timer) — specifically when a call returns a checksum error or timeout, indicating stamp drift. Do not re-handshake on every call.

**Token format:** 32-char hex string from Mi Home app → decode to 16 bytes in `NewClient`.

**Commands:**
```json
{"id": N, "method": "get_status",        "params": []}
{"id": N, "method": "get_clean_summary", "params": []}
{"id": N, "method": "app_start",         "params": []}
{"id": N, "method": "app_resume",        "params": []}
{"id": N, "method": "app_pause",         "params": []}
{"id": N, "method": "app_charge",        "params": []}
```

**`app_start` vs `app_resume`:** Model-dependent. Strategy:
```go
func (c *Client) StartOrResume(ctx context.Context, paused bool) error {
    if paused {
        err := c.call(ctx, "app_resume", nil)
        if err != nil {
            // fall back to app_start if resume not supported by this model
            return c.call(ctx, "app_start", nil)
        }
        return nil
    }
    return c.call(ctx, "app_start", nil)
}
```

---

## Data Structures

### `internal/verisure/types.go`

```go
type ArmState string

const (
    ArmStateArmedAway ArmState = "ARMED_AWAY"
    ArmStateArmedHome ArmState = "ARMED_HOME"
    ArmStateDisarmed  ArmState = "DISARMED"
)
```

### `internal/roborock/types.go`

```go
type VacuumStateID int

const (
    StateIdle         VacuumStateID = 3
    StateCleaning     VacuumStateID = 5
    StateReturning    VacuumStateID = 6
    StateCharging     VacuumStateID = 8
    StateChargingErr  VacuumStateID = 9
    StatePaused       VacuumStateID = 10
    StateSpotClean    VacuumStateID = 11
    StateError        VacuumStateID = 12
    StateDocking      VacuumStateID = 15
    StateZoneCleaning VacuumStateID = 17
    StateRoomCleaning VacuumStateID = 18
    StateFullyCharged VacuumStateID = 100
)

func (s VacuumStateID) IsActiveClean() bool {
    return s == StateCleaning || s == StateSpotClean ||
        s == StateZoneCleaning || s == StateRoomCleaning
}
func (s VacuumStateID) IsPaused() bool    { return s == StatePaused }
func (s VacuumStateID) IsError() bool     { return s == StateError || s == StateChargingErr }
func (s VacuumStateID) IsInTransit() bool { return s == StateReturning || s == StateDocking }

type VacuumStatus struct {
    State      VacuumStateID `json:"state"`
    InCleaning int           `json:"in_cleaning"`
    CleanTime  int           `json:"clean_time"`  // seconds
    CleanArea  int           `json:"clean_area"`  // cm²
    Battery    int           `json:"battery"`
    ErrorCode  int           `json:"error_code"`
}

// CleanSummary wraps the heterogeneous get_clean_summary response.
// The raw response is a flat JSON array: [total_time, total_area, total_count, [ts1, ts2, ...]]
// This cannot be decoded with a plain struct — use UnmarshalCleanSummary.
type CleanSummary struct {
    TotalTime  int
    TotalArea  int
    TotalCount int
    Records    []int64 // Unix timestamps of completed full cleans, newest first
}

func UnmarshalCleanSummary(raw json.RawMessage) (CleanSummary, error)

// LastCleanTime returns the most recent completed clean timestamp.
// Returns zero time if Records is empty (never cleaned) — the controller
// treats zero as "needs cleaning".
func (s CleanSummary) LastCleanTime() time.Time {
    if len(s.Records) == 0 {
        return time.Time{}
    }
    return time.Unix(s.Records[0], 0)
}
```

### `internal/controller/controller.go` — interfaces

```go
// AlarmSource abstracts Verisure for testing.
type AlarmSource interface {
    ArmState(ctx context.Context) (verisure.ArmState, error)
}

// VacuumCommander abstracts a single Roborock vacuum for testing.
type VacuumCommander interface {
    Name() string
    Status(ctx context.Context) (roborock.VacuumStatus, error)
    CleanSummary(ctx context.Context) (roborock.CleanSummary, error)
    StartOrResume(ctx context.Context, paused bool) error
    Pause(ctx context.Context) error
    Charge(ctx context.Context) error
}
```

---

## Controller Logic

### `Run(ctx context.Context) error`

```
1. reconcileOnStartup(ctx)
2. loop:
   a. Wait pollInterval ticker
   b. Fetch alarm state (retry/backoff on error — do not act on unknown state)
   c. If state changed from previous: call onArmedAway or onDisengaged
   d. Persist new alarm state
   e. Repeat
```

### `onArmedAway(ctx)` — iterates all vacuums

For each `VacuumCommander`:

1. `Status()` → error: log + skip (don't act on unknown state)
2. `IsError()` → log error state + skip
3. `IsInTransit()` → log "returning/docking, skipping"
4. `IsActiveClean()` → already running (manual clean); set `StartedByUs=false`, note `CleaningActive`
5. `CleanSummary()` → error: log + skip (safe default: don't start if history unknown)
6. `lastClean := summary.LastCleanTime()` — zero if never cleaned
7. `lastClean` non-zero and `time.Since(lastClean) < cleanCooldown` → log "recent clean, skipping"
8. Otherwise: `StartOrResume(ctx, status.IsPaused())`, persist `StartedByUs=true`

If any vacuum was started, set overall `ControlState=CleaningActive`.

### `onDisengaged(ctx)` — iterates all vacuums

For each vacuum where `store.Vacuums[host].StartedByUs == true`:

1. `Status()` → error: log + attempt `Pause()` anyway (best-effort)
2. `IsActiveClean()` or `IsPaused()` → `Pause()` then `Charge()`
3. Persist `StartedByUs=false`

Set `ControlState=Idle` after all vacuums processed.

### `reconcileOnStartup(ctx)`

1. Load store
2. Fetch current alarm state from Verisure (retry on error; block until success or ctx done)
3. For each vacuum where `store.Vacuums[host].StartedByUs == true`:
   - `Status()` → error: log, leave state as-is (will self-correct next poll)
   - Alarm DISARMED + vacuum active/paused → `Pause()` + `Charge()` + `StartedByUs=false`
   - Alarm ARMED_AWAY + vacuum idle/error → `StartOrResume()` (re-attempt crashed mid-clean)
   - Alarm ARMED_AWAY + vacuum active → no action, already running
4. Derive and persist `ControlState` from observed reality

---

## Error Handling & Retry

```go
// Exponential backoff — stdlib only
func backoffDuration(attempt int, base, max time.Duration) time.Duration {
    d := base * (1 << min(attempt, 10))
    if d > max { return max }
    return d
}

func withRetry(ctx context.Context, attempts int, fn func() error) error
```

| Concern | Strategy |
|---|---|
| Verisure poll error | Backoff base=5s, max=5m; log warning; skip transition |
| Verisure 401/403 | Re-login once (mutex-protected); if re-login fails, treat as poll error |
| Roborock command error | Retry 3× with base=2s, max=30s backoff |
| miIO stamp rejection | Call `Handshake()` before the retry (not proactively) |
| One vacuum fails | Log + continue; other vacuums unaffected |
| ctx cancelled | Return immediately at every `select` / `time.After` |

No `panic` outside `main` init.

---

## Persistence (`internal/store/store.go`)

Atomic write: write to `{path}.tmp`, `os.Rename` to final path.

```go
type Store struct {
    path string
    mu   sync.Mutex
    s    State
}

func New(path string) (*Store, error)
func (s *Store) Get() State
func (s *Store) SetAlarmState(a string) error
func (s *Store) SetControlState(cs string) error
func (s *Store) SetVacuumStartedByUs(host string, v bool) error
func (s *Store) SetVacuumLastClean(host string, t time.Time) error
```

---

## Configuration

| Env var | Required | Default | Notes |
|---|---|---|---|
| `VERISURE_EMAIL` | yes | — | |
| `VERISURE_PASSWORD` | yes | — | |
| `VERISURE_GIID` | no | auto-discovered | |
| `VERISURE_BASE_URL` | no | `https://e-api01.verisure.com/xbn/2` | |
| `VERISURE_OTP_SECRET` | no | — | Base32 TOTP secret for 2FA |
| `ROBOROCK_0_HOST` | yes | — | First vacuum local IP |
| `ROBOROCK_0_TOKEN` | yes | — | First vacuum 32-char hex token |
| `ROBOROCK_0_NAME` | no | `vacuum-0` | Log label |
| `ROBOROCK_N_HOST/TOKEN/NAME` | no | — | Additional vacuums (N=1,2,...) |
| `POLL_INTERVAL` | no | `60s` | Go duration string |
| `CLEAN_COOLDOWN_ENABLED` | no | `true` | Set `false` to start whenever alarm arms away |
| `CLEAN_COOLDOWN` | no | `24h` | Min time between auto-cleans |
| `ROBOROCK_CONTROL` | no | `cloud` | `cloud` for Xiaomi Cloud RPC, `local` for UDP miIO |
| `ROBOROCK_TIMEOUT` | no | `10s` | Per-UDP-command deadline in local mode |
| `STORE_PATH` | no | `./state.json` | Persist across restarts |
| `HTTP_ADDR` | no | `:8080` | Health + status server |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error` |

`config.Load()` reports **all** missing required vars at once.

---

## HTTP Status Endpoints

`GET /healthz` → `200 OK`

`GET /status` →
```json
{
  "control_state": "cleaning_active",
  "alarm_state": "ARMED_AWAY",
  "vacuums": [
    {
      "name": "kitchen",
      "host": "192.168.1.50",
      "started_by_us": true,
      "last_clean": "2025-04-19T14:00:00Z"
    }
  ]
}
```

---

## Makefile Targets

| Target | Description |
|---|---|
| `make build` | Compile to `dist/verisure-roborock` |
| `make dev` | `go run ./cmd/verisure-roborock` |
| `make test` | `go test ./...` |
| `make lint` | `go vet` + `staticcheck` (if installed) |
| `make build-linux` | Cross-compile `linux/amd64` |
| `make build-linux-arm64` | Cross-compile `linux/arm64` (Pi 4+) |
| `make clean` | Remove `dist/` |

---

## Testing Strategy

### Unit tests

- **`miio_test.go`**: frame encode/decode round-trip, checksum validation, AES encrypt/decrypt with known vectors, `UnmarshalCleanSummary` with normal/empty/single-record responses
- **`client_test.go` (verisure)**: `httptest.Server` mocking arm states, 401 re-auth flow, GIID auto-discovery
- **`controller_test.go`**: table-driven via `AlarmSource` + `VacuumCommander` mocks:
  - armed-away + last clean 2h ago → no start
  - armed-away + last clean 30h ago + idle → `StartOrResume(false)`
  - armed-away + last clean 30h ago + paused → `StartOrResume(true)`
  - armed-away + vacuum in error → skip
  - armed-away + never cleaned (zero time) → start
  - disarmed + `StartedByUs=true` + active → `Pause()` + `Charge()`
  - disarmed + `StartedByUs=false` → no calls
  - reconcile: `StartedByUs=true` + alarm disarmed + active → `Pause()` + `Charge()`
  - reconcile: `StartedByUs=true` + alarm armed + idle → restart
- **`store_test.go`**: round-trip in `t.TempDir()`, multi-vacuum entries, atomic write

### Manual E2E smoke test

1. Copy `.env.example` → `.env`, fill in credentials
2. `make dev` — confirm logs show "Verisure authenticated" and "Roborock handshake ok"
3. Arm-away via Verisure app — all vacuums start within 60s
4. Disarm — all vacuums pause and return to dock

---

## Deployment

**Linux (recommended):** `make build-linux` → copy binary to `/usr/local/bin/`, install systemd unit from `systemd/`.

**Raspberry Pi 4+:** `make build-linux-arm64`

**Requirements:** Cloud mode requires Xiaomi/Mi Home credentials and a resolvable device DID, host, or token. Local mode must be on the same LAN as the Roborock vacuums (local miIO UDP). `STORE_PATH` should point to persistent storage.

---

## Getting Credentials

### Roborock token (Mi Home app)
- **Android:** `adb backup com.xiaomi.smarthome` then extract `_mihome.sqlite` — query `ZDEVICE` table for `ZTOKEN`
- **iOS:** Export `com.xiaomi.mihome` backup, find `_mihome.sqlite`, same query
- The token is a 32-char hex string

### Verisure GIID
- Auto-discovered on first run from `GET /installation`; can be hardcoded in `VERISURE_GIID` to skip discovery
