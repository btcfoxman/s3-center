#!/usr/bin/env bash
set -euo pipefail

# Compatibility filename: Cloudflare's public OAuth scope catalog deliberately
# does not expose Account API Tokens Write, so onboarding uses the isolated
# browser connector plus a one-time dashboard-created bootstrap token instead.

: "${DEPLOY_HOST:?DEPLOY_HOST is required}"
: "${DEPLOY_USER:?DEPLOY_USER is required}"
: "${DEPLOY_PATH:?DEPLOY_PATH is required}"

deploy_port="${DEPLOY_PORT:-22}"
ssh_base=(ssh -i ~/.ssh/deploy_key -p "$deploy_port" "$DEPLOY_USER@$DEPLOY_HOST")

remote_env_set() {
  local key="$1" value="$2" encoded
  encoded="$(printf '%s' "$value" | base64 -w0)"
  "${ssh_base[@]}" "set -eu; cd '$DEPLOY_PATH'; umask 077; value=\$(printf '%s' '$encoded' | base64 -d); tmp=\$(mktemp .env.connector.XXXXXX); awk -v key='$key' -v value=\"\$value\" 'BEGIN{found=0} index(\$0,key \"=\") == 1 {print key \"=\" value; found=1; next} {print} END{if(!found) print key \"=\" value}' .env > \"\$tmp\"; chmod 600 \"\$tmp\"; mv \"\$tmp\" .env"
}

remote_env_set CF_OAUTH_ENABLED true
echo "Cloudflare isolated browser connector is enabled."
