#!/usr/bin/env bash
#
# gen-envs.sh - generate a copy-paste env file for running Carteiro on Coolify.
#
# It asks a few questions, generates the DKIM RSA key pair (and optionally a
# self-signed TLS certificate), and writes one .txt file with every
# CARTEIRO_* variable ready to paste into Coolify. The DKIM public key (p=)
# for the DNS record is printed at the end.
#
# Usage:
#   ./scripts/gen-envs.sh [output-file]
#     default: ~/Desktop/carteiro-envs.txt on macOS, ~/carteiro-envs.txt elsewhere
#
set -euo pipefail

# ---- platform detection ---------------------------------------------------

OS="$(uname -s)"

if [ "$OS" = "Darwin" ]; then
  DEFAULT_OUT="$HOME/Desktop/carteiro-envs.txt"
else
  DEFAULT_OUT="$HOME/carteiro-envs.txt"
fi

# ---- helpers -------------------------------------------------------------

b64_file() { openssl base64 -A -in "$1"; }

ask() { # ask <var> <prompt>
  local __var=$1 __prompt=$2
  local __input
  printf '%s: ' "$__prompt" >&2
  IFS= read -r __input || true
  printf -v "$__var" '%s' "$__input"
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: '$1' is required but was not found in PATH" >&2
    exit 1
  }
}

require_nonempty() { # require_nonempty <var> <what>
  local __var=$1 __what=$2 __value
  printf -v __value '%s' "${!__var}"
  if [ -z "$__value" ]; then
    echo "error: $__what is required" >&2
    exit 1
  fi
}

require_cmd openssl
require_cmd grep
require_cmd tr

echo ""
echo "Carteiro env generator"
echo "======================"
echo ""

# ---- questions -----------------------------------------------------------

echo "-- SMTP server --"
ask HOSTNAME "SMTP hostname (EHLO/banner/PTR)"
require_nonempty HOSTNAME "SMTP hostname"

echo ""
echo "-- Sender account --"
ask EMAIL "Sender account email"
require_nonempty EMAIL "sender account email"
ask PASSWORD "Account password (leave empty to generate a random one)"
if [ -z "$PASSWORD" ]; then
  PASSWORD=$(openssl rand -base64 18 | tr -d '/+=' | head -c 24)
  echo ">> generated password: $PASSWORD" >&2
fi

echo ""
echo "-- DKIM --"
ask DKIM_DOMAIN "DKIM domain (sender domain)"
require_nonempty DKIM_DOMAIN "DKIM domain"
ask SELECTOR "DKIM selector"
require_nonempty SELECTOR "DKIM selector"

echo ""
echo "-- TLS (STARTTLS) --"
echo "  1) none            - AUTH in plain text (private network only)"
echo "  2) self-signed     - generate a cert for $HOSTNAME now (test phase)"
echo "  3) Let's Encrypt   - use an existing cert/key pair (fullchain + privkey)"
ask TLS_CHOICE "Choose 1, 2 or 3"
echo ""

TLS_MODE=starttls
REQUIRE_TLS=true
CERT_B64=""
KEY_B64=""
TMPDIR_SAFE=""

case "$TLS_CHOICE" in
  1)
    echo ">> no TLS: AUTH will travel in plain text"
    REQUIRE_TLS=false
    ;;
  2)
    TMPDIR_SAFE=$(mktemp -d)
    openssl req -x509 -newkey rsa:2048 -nodes -days 825 \
      -keyout "$TMPDIR_SAFE/tls.key" -out "$TMPDIR_SAFE/tls.crt" \
      -subj "/CN=$HOSTNAME" -addext "subjectAltName=DNS:$HOSTNAME" \
      >/dev/null 2>&1
    CERT_B64=$(b64_file "$TMPDIR_SAFE/tls.crt")
    KEY_B64=$(b64_file "$TMPDIR_SAFE/tls.key")
    echo ">> self-signed certificate generated for $HOSTNAME"
    ;;
  3)
    ask CERT_FILE "Let's Encrypt fullchain.pem path"
    require_nonempty CERT_FILE "certificate path"
    ask KEY_FILE "Let's Encrypt privkey.pem path"
    require_nonempty KEY_FILE "private key path"
    if [ ! -f "$CERT_FILE" ] || [ ! -f "$KEY_FILE" ]; then
      echo "error: both certificate and key files must exist" >&2
      exit 1
    fi
    CERT_B64=$(b64_file "$CERT_FILE")
    KEY_B64=$(b64_file "$KEY_FILE")
    echo ">> using existing certificate"
    ;;
  *)
    echo "error: choose 1, 2 or 3" >&2
    exit 1
    ;;
esac

API_TOKEN=$(openssl rand -hex 24)

# ---- generate DKIM key pair ----------------------------------------------

DKIM_TMP=$(mktemp -d)
trap 'rm -rf "$DKIM_TMP" "$TMPDIR_SAFE"' EXIT
openssl genrsa -out "$DKIM_TMP/dkim.key" 2048 2>/dev/null
DKIM_KEY_B64=$(b64_file "$DKIM_TMP/dkim.key")
DKIM_PUB=$(openssl rsa -in "$DKIM_TMP/dkim.key" -pubout -outform DER 2>/dev/null | openssl base64 -A)
DKIM_KEYS_VALUE="${DKIM_DOMAIN}:${SELECTOR}:${DKIM_KEY_B64}"

# ---- write the env file ---------------------------------------------------

OUT_FILE="${1:-$DEFAULT_OUT}"
mkdir -p "$(dirname "$OUT_FILE")"

{
  echo "# Carteiro env vars for Coolify - generated $(date '+%Y-%m-%d %H:%M')"
  echo "# Paste each VALUE into its own variable (name is before the '=')."
  echo "# DKIM value: ${#DKIM_KEYS_VALUE} chars | TLS cert: ${#CERT_B64} chars | TLS key: ${#KEY_B64} chars"
  echo ""
  echo "CARTEIRO_HOSTNAME=$HOSTNAME"
  echo "CARTEIRO_LISTEN=:587"
  echo "CARTEIRO_ACCOUNTS=$EMAIL:$PASSWORD"
  echo "CARTEIRO_DKIM_KEYS=$DKIM_KEYS_VALUE"
  if [ -n "$CERT_B64" ]; then
    echo "CARTEIRO_TLS_CERT=$CERT_B64"
    echo "CARTEIRO_TLS_KEY=$KEY_B64"
    echo "CARTEIRO_TLS_MODE=$TLS_MODE"
  fi
  echo "CARTEIRO_REQUIRE_TLS=$REQUIRE_TLS"
  echo "CARTEIRO_API_TOKEN=$API_TOKEN"
  echo "# Web dashboard listener (defaults to :8080; expose 8080 in Coolify)"
  echo "CARTEIRO_WEB_LISTEN=8080"
  echo "# Admin API listener (loopback by default; expose 9090 only if you need it outside)"
  echo "CARTEIRO_API_LISTEN=9090"
} > "$OUT_FILE"
chmod 600 "$OUT_FILE"

echo ""
echo "env file written to: $OUT_FILE"
echo ""

# ---- DNS records to publish ----------------------------------------------

echo "================================================================"
echo " DNS records to add"
echo "================================================================"
echo ""
echo "1) A record for $HOSTNAME -> <server public IP> (DNS only, grey cloud)"
echo ""
echo "2) DKIM TXT record (zone $DKIM_DOMAIN):"
echo "   name:    $SELECTOR._domainkey.$DKIM_DOMAIN"
echo "   content: v=DKIM1; k=rsa; p=$DKIM_PUB"
echo ""
echo "3) SPF: add a:$HOSTNAME (or ip4:<server IP>) to the TXT record of"
echo "   $DKIM_DOMAIN if the server is not already authorized."
echo ""
echo "4) PTR (rDNS) at your hosting provider: <server IP> -> $HOSTNAME"
echo ""
echo "================================================================"
echo ""

# try to open the file in the default editor, when a desktop is available
case "$OS" in
  Darwin)
    open "$OUT_FILE"
    ;;
  Linux|*)
    if command -v xdg-open >/dev/null 2>&1 && [ -n "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ]; then
      xdg-open "$OUT_FILE" >/dev/null 2>&1 || true
    else
      echo "open $OUT_FILE  (no graphical session detected; edit it manually)" >&2
    fi
    ;;
esac
