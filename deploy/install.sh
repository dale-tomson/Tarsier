#!/usr/bin/env bash
# ==============================================================================
# Tarsier - Standalone Deployment Bundle Installer
# ==============================================================================
# Downloads docker-compose.yml, .env.example, tarsier.example.yml, and README.md.
# Automatically initializes .env and tarsier.yaml if they do not already exist.
#
# Quick usage:
#   curl -fsSL https://raw.githubusercontent.com/dale-tomson/Tarsier/main/deploy/install.sh | bash
#
# Custom version & directory:
#   curl -fsSL https://raw.githubusercontent.com/dale-tomson/Tarsier/main/deploy/install.sh | bash -s -- -v v0.1.0 -d /opt/tarsier
# ==============================================================================

set -euo pipefail

REPO="dale-tomson/Tarsier"
IMAGE_NAME="ghcr.io/dale-tomson/tarsier"
VERSION="latest"
TARGET_DIR=""
AUTO_START=false

# ANSI color codes (disabled if stdout is not a tty)
if [ -t 1 ]; then
  BOLD=$'\033[1m'
  GREEN=$'\033[32m'
  BLUE=$'\033[34m'
  YELLOW=$'\033[33m'
  RED=$'\033[31m'
  RESET=$'\033[0m'
else
  BOLD=""
  GREEN=""
  BLUE=""
  YELLOW=""
  RED=""
  RESET=""
fi

log_info() {
  printf "${BLUE}${BOLD}[INFO]${RESET} %s\n" "$1"
}

log_success() {
  printf "${GREEN}${BOLD}[SUCCESS]${RESET} %s\n" "$1"
}

log_warn() {
  printf "${YELLOW}${BOLD}[WARN]${RESET} %s\n" "$1"
}

log_error() {
  printf "${RED}${BOLD}[ERROR]${RESET} %s\n" "$1" >&2
}

print_help() {
  cat << EOF
Tarsier Deployment Installer

Usage:
  install.sh [options]

Options:
  -v, --version <tag>    Tarsier container version tag (default: latest)
  -d, --dir <path>       Target installation directory (default: ./tarsier)
  -s, --start            Automatically start Tarsier with 'docker compose up -d'
  -h, --help             Show this help message and exit

Examples:
  ./install.sh
  ./install.sh -v v0.1.0 -d /opt/tarsier
  ./install.sh --start
EOF
}

# Parse command line options
while [[ $# -gt 0 ]]; do
  case "$1" in
    -v|--version)
      VERSION="$2"
      shift 2
      ;;
    -d|--dir)
      TARGET_DIR="$2"
      shift 2
      ;;
    -s|--start)
      AUTO_START=true
      shift 1
      ;;
    -h|--help)
      print_help
      exit 0
      ;;
    *)
      log_error "Unknown option: $1"
      print_help
      exit 1
      ;;
  esac
done

# Resolve default target directory if not explicitly provided
if [ -z "$TARGET_DIR" ]; then
  CURRENT_DIR_NAME="$(basename "$PWD")"
  if [ "$CURRENT_DIR_NAME" = "tarsier" ] || [ "$CURRENT_DIR_NAME" = "deploy" ] || [ -f "docker-compose.yml" ]; then
    TARGET_DIR="."
  else
    TARGET_DIR="./tarsier"
  fi
fi

# Locate script directory if running from a local clone
SCRIPT_DIR=""
if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
  SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || echo "")"
fi

# Detect download tool (curl or wget)
DOWNLOAD_BIN=""
if command -v curl >/dev/null 2>&1; then
  DOWNLOAD_BIN="curl"
elif command -v wget >/dev/null 2>&1; then
  DOWNLOAD_BIN="wget"
fi

download_file() {
  local url="$1"
  local dest="$2"
  local filename="$(basename "$dest")"

  # If running from a local repo directory containing the file, copy directly
  if [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/$filename" ]; then
    cp "$SCRIPT_DIR/$filename" "$dest"
    return 0
  fi

  if [ -z "$DOWNLOAD_BIN" ]; then
    log_error "Neither 'curl' nor 'wget' was found on your system. Please install curl or wget."
    exit 1
  fi

  if [ "$DOWNLOAD_BIN" = "curl" ]; then
    if ! curl -fsSL "$url" -o "$dest"; then
      return 1
    fi
  else
    if ! wget -q "$url" -O "$dest"; then
      return 1
    fi
  fi
  return 0
}

# Resolve Git ref for file downloads and container image version
DOWNLOAD_REF="main"
IMAGE_TAG="$VERSION"

if [ "$VERSION" != "latest" ]; then
  DOWNLOAD_REF="$VERSION"
else
  # Query GitHub release API to check if a specific latest tag is available
  if [ -n "$DOWNLOAD_BIN" ]; then
    LATEST_API_URL="https://api.github.com/repos/${REPO}/releases/latest"
    API_RESPONSE=""
    if [ "$DOWNLOAD_BIN" = "curl" ]; then
      API_RESPONSE="$(curl -fsSL -H "Accept: application/vnd.github.v3+json" "$LATEST_API_URL" 2>/dev/null || true)"
    else
      API_RESPONSE="$(wget -qO- "$LATEST_API_URL" 2>/dev/null || true)"
    fi
    RESOLVED_TAG="$(echo "$API_RESPONSE" | grep '"tag_name":' | head -n 1 | sed -E 's/.*"([^"]+)".*/\1/' || true)"
    if [ -n "$RESOLVED_TAG" ]; then
      log_info "Resolved latest release: ${RESOLVED_TAG}"
      DOWNLOAD_REF="$RESOLVED_TAG"
    fi
  fi
fi

RAW_BASE_URL="https://raw.githubusercontent.com/${REPO}/${DOWNLOAD_REF}/deploy"

log_info "Preparing Tarsier installation in: ${BOLD}${TARGET_DIR}${RESET}"
log_info "Container image: ${BOLD}${IMAGE_NAME}:${IMAGE_TAG}${RESET}"

# Create destination directory
mkdir -p "$TARGET_DIR"

# Download distribution bundle files
FILES=("docker-compose.yml" ".env.example" "tarsier.example.yml" "README.md")
for file in "${FILES[@]}"; do
  DEST="${TARGET_DIR}/${file}"
  FILE_URL="${RAW_BASE_URL}/${file}"
  log_info "Fetching ${file}..."
  if ! download_file "$FILE_URL" "$DEST"; then
    # Fallback to main branch if release tag didn't have the deploy folder yet
    if [ "$DOWNLOAD_REF" != "main" ]; then
      FALLBACK_URL="https://raw.githubusercontent.com/${REPO}/main/deploy/${file}"
      if ! download_file "$FALLBACK_URL" "$DEST"; then
        log_error "Failed to download ${file} from ${FILE_URL} and ${FALLBACK_URL}"
        exit 1
      fi
    else
      log_error "Failed to download ${file} from ${FILE_URL}"
      exit 1
    fi
  fi
done

# Initialize .env if it does not exist
ENV_FILE="${TARGET_DIR}/.env"
ENV_EXAMPLE="${TARGET_DIR}/.env.example"
if [ ! -f "$ENV_FILE" ]; then
  cp "$ENV_EXAMPLE" "$ENV_FILE"

  # Generate a cryptographically secure 32-byte session secret
  SESSION_SECRET=""
  if command -v openssl >/dev/null 2>&1; then
    SESSION_SECRET="$(openssl rand -hex 32 2>/dev/null || true)"
  fi
  if [ -z "$SESSION_SECRET" ] && [ -r /dev/urandom ]; then
    SESSION_SECRET="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' 2>/dev/null || true)"
  fi

  if [ -n "$SESSION_SECRET" ]; then
    awk -v secret="$SESSION_SECRET" '
      /^TARSIER_SESSION_SECRET=/ { print "TARSIER_SESSION_SECRET=" secret; next }
      { print }
    ' "$ENV_FILE" > "${ENV_FILE}.tmp" && mv "${ENV_FILE}.tmp" "$ENV_FILE"
  fi

  # Set custom version tag if specified
  if [ "$IMAGE_TAG" != "latest" ]; then
    awk -v ver="$IMAGE_TAG" '
      /^TARSIER_VERSION=/ { print "TARSIER_VERSION=" ver; next }
      { print }
    ' "$ENV_FILE" > "${ENV_FILE}.tmp" && mv "${ENV_FILE}.tmp" "$ENV_FILE"
  fi

  log_success "Created .env with secure session secret"
else
  log_warn "Existing .env found. Keeping your current configuration."
fi

# Initialize tarsier.yaml if it does not exist
CONFIG_FILE="${TARGET_DIR}/tarsier.yaml"
CONFIG_EXAMPLE="${TARGET_DIR}/tarsier.example.yml"
if [ ! -f "$CONFIG_FILE" ]; then
  cp "$CONFIG_EXAMPLE" "$CONFIG_FILE"
  log_success "Created tarsier.yaml from tarsier.example.yml"
else
  log_warn "Existing tarsier.yaml found. Keeping your current configuration."
fi

# Ensure persistent data directory exists
mkdir -p "${TARGET_DIR}/data"

# Check Docker and Docker Compose availability
COMPOSE_CMD=""
if command -v docker >/dev/null 2>&1; then
  if docker compose version >/dev/null 2>&1; then
    COMPOSE_CMD="docker compose"
  elif command -v docker-compose >/dev/null 2>&1; then
    COMPOSE_CMD="docker-compose"
  fi
fi

if [ -z "$COMPOSE_CMD" ]; then
  log_warn "Docker Compose was not detected on this machine."
  log_warn "Install Docker Engine & Compose to run Tarsier: https://docs.docker.com/engine/install/"
fi

# Auto-start if requested
if [ "$AUTO_START" = true ]; then
  if [ -z "$COMPOSE_CMD" ]; then
    log_error "Cannot start Tarsier: Docker Compose is not installed."
    exit 1
  fi

  log_info "Pulling and launching Tarsier..."
  (cd "$TARGET_DIR" && $COMPOSE_CMD pull && $COMPOSE_CMD up -d)
  log_success "Tarsier container started successfully!"
fi

# Final summary instructions
echo ""
printf "${GREEN}${BOLD}==============================================================================${RESET}\n"
printf "${GREEN}${BOLD} Tarsier deployment bundle is ready in: ${TARGET_DIR}${RESET}\n"
printf "${GREEN}${BOLD}==============================================================================${RESET}\n"
echo ""
echo "Next steps:"
if [ "$TARGET_DIR" != "." ]; then
  echo "  1. Navigate to directory:"
  echo "     cd ${TARGET_DIR}"
fi
echo "  2. (Optional) Review and edit credentials in .env and tarsier.yaml:"
echo "     - TARSIER_PORT (default: 8080)"
echo "     - TARSIER_ADMIN_USER (default: admin)"
echo "     - TARSIER_ADMIN_PASSWORD_HASH (default: 'admin' - generate bcrypt hash for production)"
echo "  3. Start Tarsier:"
echo "     docker compose up -d"
echo "  4. Access Web Dashboard:"
echo "     http://localhost:8080"
echo ""
