#!/bin/sh
# Installs the RackList probe agent as a systemd service.
#
#   sudo PROBE_TOKEN=pb_... PROBE_API=https://racklist.eu/api/v1/probe sh install.sh
#
# Idempotent: re-running it upgrades the binary and restarts the service without
# touching the existing configuration.
set -eu

BINARY_NAME="racklist-probe-agent"
INSTALL_PATH="/usr/local/bin/${BINARY_NAME}"
CONFIG_DIR="/etc/racklist-probe"
CONFIG_FILE="${CONFIG_DIR}/agent.env"
SERVICE_NAME="racklist-probe"
SERVICE_USER="racklist-probe"
REPO="RackList/probe-agent"

die() {
    echo "error: $*" >&2
    exit 1
}

[ "$(id -u)" -eq 0 ] || die "run this as root (sudo)"
command -v systemctl >/dev/null 2>&1 || die "systemd is required; use the Docker image instead"

# Only required on a first install: an upgrade reuses the existing config.
if [ ! -f "${CONFIG_FILE}" ]; then
    [ -n "${PROBE_TOKEN:-}" ] || die "PROBE_TOKEN is required on a first install"
    [ -n "${PROBE_API:-}" ] || die "PROBE_API is required on a first install"
fi

case "$(uname -m)" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    armv7l|armv7) ARCH="armv7" ;;
    *) die "unsupported architecture: $(uname -m). Build from source with: go build -o ${BINARY_NAME} ." ;;
esac

VERSION="${PROBE_VERSION:-latest}"
if [ "${VERSION}" = "latest" ]; then
    DOWNLOAD_URL="https://github.com/${REPO}/releases/latest/download/${BINARY_NAME}-linux-${ARCH}"
else
    DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${VERSION}/${BINARY_NAME}-linux-${ARCH}"
fi

echo "Downloading ${BINARY_NAME} (linux/${ARCH})..."
TMP_BINARY="$(mktemp)"
# shellcheck disable=SC2064 # expand the path now, the trap must survive $TMP_BINARY going out of scope
trap "rm -f '${TMP_BINARY}'" EXIT

if command -v curl >/dev/null 2>&1; then
    curl -fsSL "${DOWNLOAD_URL}" -o "${TMP_BINARY}" || die "download failed: ${DOWNLOAD_URL}"
elif command -v wget >/dev/null 2>&1; then
    wget -qO "${TMP_BINARY}" "${DOWNLOAD_URL}" || die "download failed: ${DOWNLOAD_URL}"
else
    die "curl or wget is required"
fi

# Checksums are published alongside every release. A binary that runs unattended
# on your machine is worth verifying, and a failed check aborts the install.
CHECKSUM_URL="${DOWNLOAD_URL}.sha256"
if command -v sha256sum >/dev/null 2>&1; then
    EXPECTED=""
    if command -v curl >/dev/null 2>&1; then
        EXPECTED="$(curl -fsSL "${CHECKSUM_URL}" 2>/dev/null | awk '{print $1}')"
    else
        EXPECTED="$(wget -qO- "${CHECKSUM_URL}" 2>/dev/null | awk '{print $1}')"
    fi

    if [ -n "${EXPECTED}" ]; then
        ACTUAL="$(sha256sum "${TMP_BINARY}" | awk '{print $1}')"
        [ "${EXPECTED}" = "${ACTUAL}" ] || die "checksum mismatch: expected ${EXPECTED}, got ${ACTUAL}"
        echo "Checksum verified."
    else
        echo "warning: no published checksum found for this release, skipping verification" >&2
    fi
fi

install -m 0755 "${TMP_BINARY}" "${INSTALL_PATH}"

if ! id "${SERVICE_USER}" >/dev/null 2>&1; then
    echo "Creating service user ${SERVICE_USER}..."
    useradd --system --no-create-home --shell /usr/sbin/nologin "${SERVICE_USER}" 2>/dev/null \
        || adduser --system --no-create-home --shell /usr/sbin/nologin "${SERVICE_USER}" 2>/dev/null \
        || die "could not create the service user"
fi

mkdir -p "${CONFIG_DIR}"
chmod 0750 "${CONFIG_DIR}"
chown root:"${SERVICE_USER}" "${CONFIG_DIR}"

if [ ! -f "${CONFIG_FILE}" ]; then
    echo "Writing ${CONFIG_FILE}..."
    umask 077
    cat > "${CONFIG_FILE}" <<EOF
# RackList probe agent configuration.
# The token is a bearer secret: keep this file readable by root and the service
# user only. Rotate it from your account if it ever leaks.
PROBE_TOKEN=${PROBE_TOKEN}
PROBE_API=${PROBE_API}
EOF
    chmod 0640 "${CONFIG_FILE}"
    chown root:"${SERVICE_USER}" "${CONFIG_FILE}"
else
    echo "Keeping the existing ${CONFIG_FILE}."
fi

SERVICE_SRC="$(dirname "$0")/${SERVICE_NAME}.service"
if [ -f "${SERVICE_SRC}" ]; then
    install -m 0644 "${SERVICE_SRC}" "/etc/systemd/system/${SERVICE_NAME}.service"
else
    echo "Downloading the systemd unit..."
    UNIT_URL="https://raw.githubusercontent.com/${REPO}/main/packaging/${SERVICE_NAME}.service"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "${UNIT_URL}" -o "/etc/systemd/system/${SERVICE_NAME}.service" || die "could not fetch the systemd unit"
    else
        wget -qO "/etc/systemd/system/${SERVICE_NAME}.service" "${UNIT_URL}" || die "could not fetch the systemd unit"
    fi
    chmod 0644 "/etc/systemd/system/${SERVICE_NAME}.service"
fi

systemctl daemon-reload
systemctl enable "${SERVICE_NAME}" >/dev/null 2>&1 || true
systemctl restart "${SERVICE_NAME}"

echo
echo "Done. The probe is running."
echo "  status : systemctl status ${SERVICE_NAME}"
echo "  logs   : journalctl -u ${SERVICE_NAME} -f"
echo
echo "It will appear in your account within a few minutes, with its network"
echo "operator and country read from the IP it connects from."
