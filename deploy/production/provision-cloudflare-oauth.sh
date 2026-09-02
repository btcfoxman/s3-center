#!/usr/bin/env bash
set -euo pipefail

: "${CF_API_TOKEN:?CF_API_TOKEN is required}"
: "${CF_ACCOUNT_ID:?CF_ACCOUNT_ID is required}"
: "${DEPLOY_HOST:?DEPLOY_HOST is required}"
: "${DEPLOY_USER:?DEPLOY_USER is required}"
: "${DEPLOY_PATH:?DEPLOY_PATH is required}"

deploy_port="${DEPLOY_PORT:-22}"
api="https://api.cloudflare.com/client/v4"
auth="Authorization: Bearer ${CF_API_TOKEN}"
ssh_base=(ssh -i ~/.ssh/deploy_key -p "$deploy_port" "$DEPLOY_USER@$DEPLOY_HOST")
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

remote_env_get() {
  local key="$1"
  "${ssh_base[@]}" "cd '$DEPLOY_PATH' && awk -F= -v key='$key' '\$1 == key {print substr(\$0, index(\$0, \"=\") + 1); exit}' .env"
}

remote_env_set() {
  local key="$1" value="$2" encoded
  encoded="$(printf '%s' "$value" | base64 -w0)"
  "${ssh_base[@]}" "set -eu; cd '$DEPLOY_PATH'; umask 077; value=\$(printf '%s' '$encoded' | base64 -d); tmp=\$(mktemp .env.oauth.XXXXXX); awk -v key='$key' -v value=\"\$value\" 'BEGIN{found=0} index(\$0,key \"=\") == 1 {print key \"=\" value; found=1; next} {print} END{if(!found) print key \"=\" value}' .env > \"\$tmp\"; chmod 600 \"\$tmp\"; mv \"\$tmp\" .env"
}

disable_oauth() {
  remote_env_set CF_OAUTH_ENABLED false
  echo "Cloudflare OAuth remains disabled: $1"
  exit 0
}

scopes_file="$tmp_dir/scopes.json"
if ! curl -fsS "$api/oauth/scopes" -H "$auth" > "$scopes_file"; then
  disable_oauth "the deployment token could not list OAuth scopes"
fi

scope_id() {
  local name="$1"
  jq -er --arg name "$name" '.result[] | select(.name == $name) | .id' "$scopes_file" | head -n1
}

account_scope="$(scope_id 'Account Read' || true)"
r2_scope="$(scope_id 'Workers R2 Storage Read' || true)"
token_scope="$(scope_id 'Account API Tokens Write' || true)"
if [[ -z "$account_scope" || -z "$r2_scope" || -z "$token_scope" ]]; then
  disable_oauth "one or more required OAuth scopes are unavailable"
fi

client_id="$(remote_env_get CF_OAUTH_CLIENT_ID || true)"
client_secret_present="$(remote_env_get CF_OAUTH_CLIENT_SECRET || true)"
client_file="$tmp_dir/client.json"
client_exists=false
if [[ -n "$client_id" && -n "$client_secret_present" ]]; then
  if curl -fsS "$api/accounts/$CF_ACCOUNT_ID/oauth_clients/$client_id" -H "$auth" > "$client_file"; then
    client_exists=true
  fi
fi

if [[ "$client_exists" != true ]]; then
  payload_file="$tmp_dir/payload.json"
  jq -n \
    --arg account "$account_scope" --arg r2 "$r2_scope" --arg tokens "$token_scope" \
    '{client_name:"S3 Center R2 Orchestrator",grant_types:["authorization_code"],response_types:["code"],token_endpoint_auth_method:"client_secret_basic",redirect_uris:["https://s3-center.aiid.qzz.io/ui/oauth/cloudflare/callback"],scopes:[$account,$r2,$tokens],client_uri:"https://s3-center.aiid.qzz.io/ui/oauth/cloudflare/info",logo_uri:"https://s3-center.aiid.qzz.io/ui/static/oauth-logo.svg",policy_uri:"https://s3-center.aiid.qzz.io/ui/oauth/cloudflare/info",tos_uri:"https://s3-center.aiid.qzz.io/ui/oauth/cloudflare/info"}' > "$payload_file"
  if ! curl -sS "$api/accounts/$CF_ACCOUNT_ID/oauth_clients" -X POST -H "$auth" -H 'Content-Type: application/json' --data-binary "@$payload_file" > "$client_file"; then
    disable_oauth "the OAuth client API was unreachable"
  fi
  if ! jq -e '.success == true and (.result.client_id | length > 0) and (.result.client_secret | length > 0)' "$client_file" >/dev/null; then
    reason="$(jq -r '[.errors[].message] | join("; ")' "$client_file")"
    disable_oauth "client creation failed (${reason:-unknown Cloudflare error}); grant OAuth Clients Write to CF_API_TOKEN"
  fi
  client_id="$(jq -r '.result.client_id' "$client_file")"
  remote_env_set CF_OAUTH_CLIENT_ID "$client_id"
  remote_env_set CF_OAUTH_CLIENT_SECRET "$(jq -r '.result.client_secret' "$client_file")"
fi

remote_env_set CF_OAUTH_SCOPE_ACCOUNT_READ "$account_scope"
remote_env_set CF_OAUTH_SCOPE_R2_READ "$r2_scope"
remote_env_set CF_OAUTH_SCOPE_TOKEN_WRITE "$token_scope"

verification_status="$(jq -r '.result.client_uri_verification.status // "pending"' "$client_file")"
verification_text="$(jq -r '.result.client_uri_verification.text // empty' "$client_file")"
if [[ "$verification_status" != verified && -n "$verification_text" ]]; then
  zone_id="$(curl -fsS "$api/zones?name=aiid.qzz.io" -H "$auth" | jq -er '.result[0].id')"
  existing_record="$(curl -fsS "$api/zones/$zone_id/dns_records?type=TXT&name=s3-center.aiid.qzz.io" -H "$auth" | jq -r --arg content "$verification_text" '.result[] | select(.content == $content) | .id' | head -n1)"
  if [[ -z "$existing_record" ]]; then
    jq -n --arg content "$verification_text" '{type:"TXT",name:"s3-center.aiid.qzz.io",content:$content,ttl:60}' \
      | curl -fsS "$api/zones/$zone_id/dns_records" -X POST -H "$auth" -H 'Content-Type: application/json' --data-binary @- \
      | jq -e '.success == true' >/dev/null
  fi
  for _ in $(seq 1 20); do
    sleep 3
    curl -fsS "$api/accounts/$CF_ACCOUNT_ID/oauth_clients/$client_id" -H "$auth" > "$client_file"
    verification_status="$(jq -r '.result.client_uri_verification.status // "pending"' "$client_file")"
    [[ "$verification_status" == verified ]] && break
  done
fi

if [[ "$verification_status" != verified ]]; then
  disable_oauth "publisher-domain verification is still $verification_status; a later deployment will resume it"
fi

visibility="$(jq -r '.result.visibility // "private"' "$client_file")"
if [[ "$visibility" != public ]]; then
  if ! jq -n '{visibility:"public"}' | curl -sS "$api/accounts/$CF_ACCOUNT_ID/oauth_clients/$client_id" -X PATCH -H "$auth" -H 'Content-Type: application/json' --data-binary @- > "$client_file"; then
    disable_oauth "the client visibility update was unreachable"
  fi
  if ! jq -e '.success == true and .result.visibility == "public"' "$client_file" >/dev/null; then
    reason="$(jq -r '[.errors[].message] | join("; ")' "$client_file")"
    disable_oauth "public promotion failed (${reason:-unknown Cloudflare error})"
  fi
fi

remote_env_set CF_OAUTH_ENABLED true
echo "Cloudflare OAuth client is public and enabled."
