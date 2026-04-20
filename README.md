# verisure-roborock

Automates your Roborock vacuum(s) based on your Verisure home alarm state.

- **Alarm armed-away** → starts all vacuums (if they haven't cleaned in the last 24 h)
- **Alarm disarmed** → pauses any vacuums it started and sends them back to dock

Runs as a single Go binary on any Linux host on your local network. Communicates with vacuums directly over UDP (no Roborock cloud involved day-to-day).

---

## Quick start

```bash
curl -sSL https://raw.githubusercontent.com/tokko/verisure-roborock/master/install.sh | bash
```

The script installs the binary, walks you through configuration, and optionally installs a systemd service.

---

## Manual setup

### Prerequisites

- Go 1.23+ (`go version`)
- Git
- A Linux host on the same LAN as your Roborock vacuum(s)
- A Verisure account
- A Mi Home / Roborock account

### 1. Clone and build

```bash
git clone https://github.com/tokko/verisure-roborock.git
cd verisure-roborock
make build
```

### 2. Configure

```bash
cp .env.example .env
# Edit .env with your Verisure credentials — Roborock tokens come next.
```

### 3. Fetch Roborock device tokens

The vacuum's local token is separate from your account password. Fetch it automatically using your Mi Home credentials:

```bash
make fetch-tokens
```

Output looks like:

```
ROBOROCK_0_HOST=192.168.1.50
ROBOROCK_0_TOKEN=a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4
ROBOROCK_0_NAME=roborock-s5
```

Paste those lines into your `.env`.

### 4. Run

```bash
make dev          # development — reads .env, logs to stdout
make build        # compile to dist/verisure-roborock
./dist/verisure-roborock
```

---

## Configuration

All configuration is via environment variables (or `.env`). Run `make fetch-tokens` first to discover your vacuum IPs and tokens.

| Variable | Required | Default | Description |
|---|---|---|---|
| `VERISURE_EMAIL` | yes | — | Verisure account email |
| `VERISURE_PASSWORD` | yes | — | Verisure account password |
| `VERISURE_GIID` | no | auto | Installation ID (auto-discovered) |
| `VERISURE_BASE_URL` | no | `https://e-api01.verisure.com/xbn/2` | Override for non-EU |
| `VERISURE_MFA_PHONE` | no | — | Phone number for SMS 2FA |
| `XIAOMI_EMAIL` | no | `VERISURE_EMAIL` | Mi Home email (if different) |
| `XIAOMI_PASSWORD` | no | `VERISURE_PASSWORD` | Mi Home password (if different) |
| `XIAOMI_COUNTRY` | no | `de` | Cloud region: `de` EU, `us`, `sg`, `cn` |
| `ROBOROCK_0_HOST` | yes | — | First vacuum local IP |
| `ROBOROCK_0_TOKEN` | yes | — | First vacuum token (from `make fetch-tokens`) |
| `ROBOROCK_0_NAME` | no | `vacuum-0` | Label for logs |
| `ROBOROCK_N_HOST/TOKEN/NAME` | no | — | Additional vacuums (N = 1, 2, …) |
| `POLL_INTERVAL` | no | `60s` | How often to poll Verisure |
| `CLEAN_COOLDOWN` | no | `24h` | Skip vacuum if it cleaned within this window |
| `ROBOROCK_TIMEOUT` | no | `10s` | Per-command UDP timeout |
| `STORE_PATH` | no | `./state.json` | Persistent state file |
| `HTTP_ADDR` | no | `:8080` | Health and status server |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error` |

---

## Verisure 2FA (SMS)

If your account has SMS 2FA enabled:

1. Set `VERISURE_MFA_PHONE` in `.env`
2. On first run, the service will trigger an SMS to that number and log:
   ```
   verisure: SMS code sent — POST the code to /mfa-code to continue
   ```
3. Submit the code:
   ```bash
   curl -X POST http://localhost:8080/mfa-code -d "123456"
   ```
4. The session cookie is persisted in `state.json` — MFA is only required when the session expires (typically weeks to months).

---

## HTTP endpoints

| Endpoint | Description |
|---|---|
| `GET /healthz` | Returns `200 ok` if the process is alive |
| `GET /status` | JSON: current alarm state, control state, per-vacuum status |
| `POST /mfa-code` | Submit Verisure SMS code (body = the 6-digit code) |

Example status response:

```json
{
  "alarm_state": "ARMED_AWAY",
  "control_state": "cleaning_active",
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

## Deployment (systemd)

```bash
# Cross-compile for your Linux host:
make build-linux        # x86_64 server / NAS
make build-linux-arm64  # Raspberry Pi 4+

# Copy to your server:
scp dist/verisure-roborock-linux-arm64 pi@192.168.1.10:/usr/local/bin/verisure-roborock

# Copy and fill in the env file:
scp .env pi@192.168.1.10:/etc/verisure-roborock/env   # chmod 600

# Install the systemd unit:
scp systemd/verisure-roborock.service pi@192.168.1.10:/etc/systemd/system/
ssh pi@192.168.1.10 "systemctl daemon-reload && systemctl enable --now verisure-roborock"

# Check status:
ssh pi@192.168.1.10 "systemctl status verisure-roborock"
curl http://192.168.1.10:8080/status
```

---

## How it works

1. The service polls the Verisure alarm state every 60 s
2. On transition to **ARMED_AWAY**: for each configured vacuum, it checks the last completed clean time via the local miIO protocol. If >24 h ago (or never), it sends `app_start` (or `app_resume` if paused)
3. On transition to **DISARMED** or **ARMED_HOME**: for each vacuum it started, it sends `app_pause` then `app_charge` (return to dock)
4. On restart: reconciles persisted state against current alarm and vacuum states before entering the poll loop — a vacuum will never be left running unattended after a crash

Vacuums are controlled directly over UDP on your LAN using the [miIO binary protocol](https://github.com/rytilahti/python-miio/blob/master/miio/protocol.py). No Roborock cloud involved.

See [ARCHITECTURE.md](ARCHITECTURE.md) for the full design document.

---

## Development

```bash
make test          # go test ./...
make lint          # go vet + staticcheck
make build         # compile
make dev           # go run (reads .env)
make fetch-tokens  # discover Roborock IPs and tokens
```

---

## License

MIT
