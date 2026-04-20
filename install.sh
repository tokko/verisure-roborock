#!/usr/bin/env bash
# verisure-roborock installer
# Usage: curl -sSL https://raw.githubusercontent.com/tokko/verisure-roborock/master/install.sh | bash
set -euo pipefail

REPO="https://github.com/tokko/verisure-roborock.git"
REPO_RAW="https://raw.githubusercontent.com/tokko/verisure-roborock/master"
INSTALL_DIR="$HOME/verisure-roborock"
BINARY_DIR="/usr/local/bin"
SERVICE_NAME="verisure-roborock"
MIN_GO_MINOR=23

# ── colours ────────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
info()    { echo -e "${CYAN}▶ $*${NC}"; }
success() { echo -e "${GREEN}✓ $*${NC}"; }
warn()    { echo -e "${YELLOW}⚠ $*${NC}"; }
die()     { echo -e "${RED}✗ $*${NC}" >&2; exit 1; }

# ── checks ─────────────────────────────────────────────────────────────────────
check_os() {
  case "$(uname -s)" in
    Linux)  OS=linux ;;
    Darwin) OS=darwin ;;
    *)      die "Unsupported OS: $(uname -s). Run on Linux or macOS." ;;
  esac

  case "$(uname -m)" in
    x86_64)         ARCH=amd64 ;;
    aarch64|arm64)  ARCH=arm64 ;;
    *)              die "Unsupported architecture: $(uname -m)" ;;
  esac
}

check_deps() {
  local missing=()
  for cmd in git curl; do
    command -v "$cmd" &>/dev/null || missing+=("$cmd")
  done
  [[ ${#missing[@]} -eq 0 ]] || die "Missing required tools: ${missing[*]}"
}

check_or_install_go() {
  if command -v go &>/dev/null; then
    local ver
    ver=$(go version | grep -oP '\d+\.\d+' | head -1)
    local minor
    minor=$(echo "$ver" | cut -d. -f2)
    if [[ "$minor" -ge "$MIN_GO_MINOR" ]]; then
      success "Go $ver found"
      return
    fi
    warn "Go $ver is too old (need 1.${MIN_GO_MINOR}+). Installing latest..."
  else
    info "Go not found. Installing..."
  fi

  # Install Go via the official tarball.
  local go_version
  go_version=$(curl -sSL "https://go.dev/VERSION?m=text" | head -1)
  local tarball="${go_version}.${OS}-${ARCH}.tar.gz"
  local url="https://go.dev/dl/${tarball}"

  info "Downloading $tarball..."
  curl -sSL "$url" -o "/tmp/$tarball"

  if [[ "$OS" == "linux" ]]; then
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "/tmp/$tarball"
    export PATH="/usr/local/go/bin:$PATH"
    # Persist for future shells.
    local profile="$HOME/.profile"
    grep -q '/usr/local/go/bin' "$profile" 2>/dev/null || \
      echo 'export PATH="/usr/local/go/bin:$PATH"' >> "$profile"
  else
    # macOS: use the pkg installer is complex; just extract to /usr/local.
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "/tmp/$tarball"
    export PATH="/usr/local/go/bin:$PATH"
  fi

  rm "/tmp/$tarball"
  success "Go $(go version | awk '{print $3}') installed"
}

# ── clone / update ─────────────────────────────────────────────────────────────
clone_or_update() {
  if [[ -d "$INSTALL_DIR/.git" ]]; then
    info "Updating existing clone at $INSTALL_DIR..."
    git -C "$INSTALL_DIR" pull --ff-only
  else
    info "Cloning into $INSTALL_DIR..."
    git clone "$REPO" "$INSTALL_DIR"
  fi
  success "Source at $INSTALL_DIR"
}

# ── configure ──────────────────────────────────────────────────────────────────
configure() {
  local env_file="$INSTALL_DIR/.env"

  if [[ -f "$env_file" ]]; then
    warn ".env already exists — skipping interactive setup"
    return
  fi

  cp "$INSTALL_DIR/.env.example" "$env_file"

  echo ""
  echo -e "${CYAN}━━━  Configuration  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
  echo ""

  read -rp "  Verisure email:    " verisure_email
  read -rsp "  Verisure password: " verisure_password; echo ""

  sed -i "s|^VERISURE_EMAIL=.*|VERISURE_EMAIL=${verisure_email}|" "$env_file"
  sed -i "s|^VERISURE_PASSWORD=.*|VERISURE_PASSWORD=${verisure_password}|" "$env_file"

  read -rp "  SMS 2FA phone (leave blank if not enabled): " mfa_phone
  if [[ -n "$mfa_phone" ]]; then
    sed -i "s|^VERISURE_MFA_PHONE=.*|VERISURE_MFA_PHONE=${mfa_phone}|" "$env_file"
  fi

  echo ""
  read -rp "  Mi Home email (press Enter if same as Verisure): " xiaomi_email
  if [[ -n "$xiaomi_email" ]]; then
    sed -i "s|^# XIAOMI_EMAIL=.*|XIAOMI_EMAIL=${xiaomi_email}|" "$env_file"
  fi
  read -rsp "  Mi Home password (press Enter if same as Verisure): " xiaomi_password; echo ""
  if [[ -n "$xiaomi_password" ]]; then
    sed -i "s|^# XIAOMI_PASSWORD=.*|XIAOMI_PASSWORD=${xiaomi_password}|" "$env_file"
  fi

  echo ""
  info "Mi Home cloud region (for token fetching):"
  echo "    de = Europe (default)   us = USA   sg = Asia   cn = China"
  read -rp "  Region [de]: " xiaomi_country
  xiaomi_country="${xiaomi_country:-de}"
  sed -i "s|^XIAOMI_COUNTRY=.*|XIAOMI_COUNTRY=${xiaomi_country}|" "$env_file"

  chmod 600 "$env_file"
  success ".env written"
}

# ── fetch tokens ───────────────────────────────────────────────────────────────
fetch_tokens() {
  echo ""
  echo -e "${CYAN}━━━  Fetching Roborock device tokens  ━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
  echo ""
  info "Logging into Mi Home cloud to retrieve device tokens..."
  echo ""

  local token_output
  token_output=$(cd "$INSTALL_DIR" && go run ./cmd/fetch-tokens 2>/dev/null) || {
    warn "Token fetch failed. You can run it manually later: cd $INSTALL_DIR && make fetch-tokens"
    return
  }

  echo "$token_output"
  echo ""

  # Extract and append ROBOROCK_* lines that aren't already in .env.
  local env_file="$INSTALL_DIR/.env"
  while IFS= read -r line; do
    [[ "$line" =~ ^ROBOROCK_ ]] || continue
    local key="${line%%=*}"
    if ! grep -q "^${key}=" "$env_file"; then
      echo "$line" >> "$env_file"
    else
      sed -i "s|^${key}=.*|${line}|" "$env_file"
    fi
  done <<< "$token_output"

  success "Roborock tokens written to .env"
}

# ── build ──────────────────────────────────────────────────────────────────────
build() {
  echo ""
  info "Building..."
  (cd "$INSTALL_DIR" && GOOS="$OS" GOARCH="$ARCH" go build -ldflags="-s -w" -o dist/verisure-roborock ./cmd/verisure-roborock)
  success "Binary: $INSTALL_DIR/dist/verisure-roborock"
}

# ── install binary ─────────────────────────────────────────────────────────────
install_binary() {
  local src="$INSTALL_DIR/dist/verisure-roborock"
  local dst="$BINARY_DIR/verisure-roborock"

  if [[ -w "$BINARY_DIR" ]]; then
    cp "$src" "$dst"
  else
    sudo cp "$src" "$dst"
  fi
  sudo chmod +x "$dst"
  success "Installed to $dst"
}

# ── systemd ────────────────────────────────────────────────────────────────────
install_systemd() {
  [[ "$OS" != "linux" ]] && return
  ! command -v systemctl &>/dev/null && return

  echo ""
  read -rp "Install systemd service? [Y/n]: " ans
  [[ "${ans:-Y}" =~ ^[Nn] ]] && return

  # Env file location.
  local env_dir="/etc/${SERVICE_NAME}"
  sudo mkdir -p "$env_dir"
  if [[ ! -f "${env_dir}/env" ]]; then
    sudo cp "$INSTALL_DIR/.env" "${env_dir}/env"
    sudo chmod 600 "${env_dir}/env"
    info "Env file installed to ${env_dir}/env"
    warn "Edit ${env_dir}/env to update credentials if needed"
  fi

  # Service unit.
  sudo cp "$INSTALL_DIR/systemd/${SERVICE_NAME}.service" \
    "/etc/systemd/system/${SERVICE_NAME}.service"
  sudo systemctl daemon-reload
  sudo systemctl enable --now "$SERVICE_NAME"
  success "systemd service enabled and started"
  echo ""
  info "Check status:  systemctl status $SERVICE_NAME"
  info "Follow logs:   journalctl -u $SERVICE_NAME -f"
}

# ── main ───────────────────────────────────────────────────────────────────────
main() {
  echo ""
  echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
  echo -e "${CYAN}   verisure-roborock installer                                ${NC}"
  echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
  echo ""

  check_os
  check_deps
  check_or_install_go
  clone_or_update
  configure
  fetch_tokens
  build
  install_binary
  install_systemd

  echo ""
  echo -e "${GREEN}━━━  Done  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
  echo ""
  echo "  Run manually:   verisure-roborock"
  echo "  Health check:   curl http://localhost:8080/healthz"
  echo "  Status:         curl http://localhost:8080/status"
  if [[ -n "${mfa_phone:-}" ]]; then
    echo "  Submit MFA:     curl -X POST http://localhost:8080/mfa-code -d '123456'"
  fi
  echo ""
  echo "  Source:  $INSTALL_DIR"
  echo "  Config:  $INSTALL_DIR/.env"
  echo ""
}

main "$@"
