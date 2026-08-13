#!/bin/sh
# Generate a long-lived Nomad token scoped to the endpoints routed-cni needs.
# Usage:
#   NOMAD_TOKEN=<management-token> ./generate-token.sh [output-file]
# Default output: /etc/cni/net.d/routed-cni.token

set -eu

OUT="${1:-/etc/cni/net.d/routed-cni.token}"
POLICY_NAME="${POLICY_NAME:-routed-cni}"
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
POLICY_FILE="${POLICY_FILE:-$SCRIPT_DIR/routed-cni.policy.hcl}"

if [ -z "${NOMAD_TOKEN:-}" ]; then
  echo "NOMAD_TOKEN (management or policy-write token) is required" >&2
  exit 1
fi

if ! command -v nomad >/dev/null 2>&1; then
  echo "nomad CLI not found in PATH" >&2
  exit 1
fi

nomad acl policy apply -description "routed-cni service discovery" \
  "$POLICY_NAME" "$POLICY_FILE"

# Create a non-expiring client token bound only to that policy.
TOKEN=$(nomad acl token create \
  -name="routed-cni" \
  -type="client" \
  -policy="$POLICY_NAME" \
  -global \
  -json | sed -n 's/.*"SecretID": *"\([^"]*\)".*/\1/p' | head -1)

if [ -z "$TOKEN" ]; then
  echo "failed to create token" >&2
  exit 1
fi

umask 077
printf '%s\n' "$TOKEN" >"$OUT"
chmod 0600 "$OUT"
echo "wrote scoped token to $OUT"
echo "set nomadTokenFile=$OUT in the CNI conflist"
