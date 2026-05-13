# verisure-roborock

Automates your Roborock vacuum(s) based on your Verisure home alarm state.

- **Alarm armed-away** → starts every configured vacuum
- **Alarm disarmed** → pauses any vacuums it started and sends them back to dock

Runs as a single Go binary or Docker Swarm service. By default it controls Roborock-app vacuums through the Roborock cloud helper, with Xiaomi/Mi Home cloud and older local UDP miIO transports still available for legacy devices.

Roborock-app robots are prepared for balanced vacuum-only cleaning before each full or room clean: balanced suction, vacuum-only clean mode where supported, and mop water off.

---

## Quick start

```bash
curl -sSL https://raw.githubusercontent.com/tokko/verisure-roborock/master/install.sh | bash
```

The script installs the binary, walks you through configuration, and optionally installs a systemd service. For the current Raspberry Pi deployment, use the Docker Swarm flow below instead.

---

## Manual setup

### Prerequisites

- Go 1.23+ (`go version`)
- Git
- A Linux host on the same LAN as your Roborock vacuum(s)
- A Verisure account
- A Roborock account for Roborock-app devices, or a Mi Home/Xiaomi account for legacy Mi Home-only devices

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

### 3. Configure Roborock devices

For Roborock-app devices, set `ROBOROCK_CONTROL=roborock` and configure each vacuum by name, host/IP, or Roborock device ID. The service uses `ROBOROCK_AUTH_PATH` to cache the Roborock cloud sign-in.

Legacy Mi Home-only robots can use `ROBOROCK_N_BACKEND=xiaomi`. Local UDP mode is still available with `ROBOROCK_CONTROL=local`, but it requires the vacuum's local token, which is separate from your account password. Fetch local/Xiaomi details with:

```bash
make fetch-tokens
```

Output looks like:

```
ROBOROCK_0_HOST=192.168.1.50
ROBOROCK_0_DID=123456789
ROBOROCK_0_TOKEN=a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4
ROBOROCK_0_NAME=roborock-s5
```

Paste the relevant lines into your `.env`.

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
| `ROBOROCK_EMAIL` | no | `VERISURE_EMAIL` | Roborock account email (if different from Verisure) |
| `ROBOROCK_PASSWORD` | no | `VERISURE_PASSWORD` | Roborock account password (if different) |
| `ROBOROCK_CONTROL` | no | `roborock` | `roborock`/`cloud` for Roborock app, `xiaomi` for Mi Home, `mixed`, or `local` |
| `ROBOROCK_AUTH_PATH` | no | `./roborock-auth.json` | Cached Roborock-app auth file |
| `ROBOROCK_HELPER` | no | `./scripts/roborock_cloud.py` | Roborock app cloud helper script |
| `ROBOROCK_PYTHON` | no | `python3` | Python executable for the helper |
| `XIAOMI_EMAIL` | no | `ROBOROCK_EMAIL` | Mi Home email (Mi Home app users only) |
| `XIAOMI_PASSWORD` | no | `ROBOROCK_PASSWORD` | Mi Home password (Mi Home app users only) |
| `XIAOMI_COUNTRY` | no | `de` | Cloud region: `de` EU, `us`, `sg`, `cn` |
| `XIAOMI_AUTH_PATH` | no | `./xiaomi-auth.json` | Cached Mi Home/Xiaomi auth file |
| `ROBOROCK_0_HOST` | local: yes | — | Vacuum local IP; also used as the persisted store key |
| `ROBOROCK_0_DID` | no | — | Roborock DUID or Xiaomi device ID |
| `ROBOROCK_0_TOKEN` | local: yes | — | First vacuum token (from `make fetch-tokens`) |
| `ROBOROCK_0_NAME` | no | `vacuum-0` | Label for logs |
| `ROBOROCK_0_BACKEND` | no | `ROBOROCK_CONTROL` | Per-vacuum backend: `roborock`, `xiaomi`, or `local` |
| `ROBOROCK_N_HOST/DID/TOKEN/NAME` | no | — | Additional vacuums (N = 1, 2, …) |
| `POLL_INTERVAL` | no | `60s` | How often to poll Verisure |
| `CLEAN_COOLDOWN_ENABLED` | no | `true` | Set `false` to ignore recent clean history |
| `CLEAN_COOLDOWN` | no | `24h` | Skip vacuum if it cleaned within this window |
| `ROBOROCK_TIMEOUT` | no | `10s` | Per-command UDP timeout when `ROBOROCK_CONTROL=local` |
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
| `GET /robots` | Lists configured robots and whether this service started them |
| `POST /robots/{name-or-host}/start` | Starts or resumes one robot |
| `POST /robots/{name-or-host}/stop` | Pauses one robot and sends it back to dock |
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

Manual start can also request specific rooms/segments when the selected backend supports Roborock's `app_segment_clean` command:

```bash
curl -X POST http://localhost:8080/robots/upstairs/start \
  -H 'Content-Type: application/json' \
  -d '{"rooms":[16,17],"repeat":1}'
```

Room IDs are the Roborock segment IDs from the robot's map. Classic/local miIO, Xiaomi cloud RPC, and Roborock-app V1 command paths use `app_segment_clean`; B01/Q10 models currently return an unsupported-command error until their room-clean DP mapping is confirmed.

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

## Deployment (Docker Swarm)

Build the image on the Raspberry Pi swarm manager, then deploy the stack:

```bash
cd /home/pi/verisure-roborock
mkdir -p data
docker build -t verisure-roborock:latest .
docker stack deploy -c docker-compose.yml --resolve-image never verisure-roborock
```

The stack pins the service to a manager node and bind-mounts `/home/pi/verisure-roborock/data` to `/data` for persistent `state.json`, `roborock-auth.json`, and `xiaomi-auth.json`.

When rebuilding a local `verisure-roborock:latest` image, force the service to restart after the build so Swarm runs the new local image:

```bash
docker service update --force verisure-roborock_verisure-roborock
```

To remove the old bare-metal service after the stack is healthy:

```bash
sudo systemctl disable --now verisure-roborock
```

## Xiaomi Cloud Sign-In

Cloud mode first tries cached Xiaomi credentials from `XIAOMI_AUTH_PATH` (default `./xiaomi-auth.json`). If `XIAOMI_USER_ID`, `XIAOMI_SSECURITY`, and `XIAOMI_SERVICE_TOKEN` are set in the environment, the app imports them once and writes the cache file.

Headless password login is kept as a fallback, but Xiaomi frequently requires captcha or browser verification. The reliable one-time route is browser-assisted token import:

1. On tokstation, open `https://account.xiaomi.com/pass/serviceLogin?sid=xiaomiio&_json=true`.
2. If the page returns JSON with a `location` field, copy that `location` URL into the address bar.
3. Open browser developer tools, enable network logging, and complete the Xiaomi sign-in page.
4. Capture:
   - `userId` and `serviceToken` from the `https://sts.api.io.mi.com/sts` response cookies.
   - `ssecurity` from the `serviceLoginAuth2` response payload.
5. Add these once to `/home/pi/verisure-roborock/.env` as `XIAOMI_USER_ID`, `XIAOMI_SSECURITY`, and `XIAOMI_SERVICE_TOKEN`.
6. Start the stack; the app writes `/data/xiaomi-auth.json`. After that, remove the three one-time variables from `.env` if desired.

There is not a stable Xiaomi callback URL that this service can receive directly, so opening a link on tokstation is supported as a manual browser login/import flow rather than as an OAuth-style redirect back to the service.

---

## How it works

1. The service polls the Verisure alarm state every 60 s
2. On transition to **ARMED_AWAY**: for each configured vacuum, it checks the last completed clean time through the selected transport when `CLEAN_COOLDOWN_ENABLED=true`. If the cooldown is disabled, it starts eligible vacuums every time the alarm arms away.
3. On transition to **DISARMED** or **ARMED_HOME**: for each vacuum it started, it sends `app_pause` then `app_charge` (return to dock)
4. On restart: reconciles persisted state against current alarm and vacuum states before entering the poll loop — a vacuum will never be left running unattended after a crash

If one vacuum starts but another fails transiently, the controller keeps retrying the pending vacuum while the alarm remains armed. Roborock-app cloud control commands are serialized and spaced by 30 seconds to avoid the common `request too frequency` / response `9002` rate limit; if the cloud still returns that error, start/pause/dock commands retry with backoff.

Default vacuum control uses the Roborock app cloud helper. Set `ROBOROCK_CONTROL=xiaomi` for Mi Home/Xiaomi Cloud devices or `ROBOROCK_CONTROL=local` to use the direct UDP miIO transport on your LAN.

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
