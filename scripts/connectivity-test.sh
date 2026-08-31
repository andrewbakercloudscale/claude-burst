#!/bin/zsh
# Smoke-tests the two upstreams claude-burst depends on, independent of the
# gateway's own routing logic (oauth-passthrough only fails over on genuine
# subscription-limit signals, so there's no safe way to force a real request
# through the secondary path without an actual exhausted subscription).
#
#   1. gateway    - /healthz responds
#   2. Anthropic  - reachable, and a real call if ANTHROPIC_API_KEY is set
#   3. Together   - a real minimal chat-completions call using the key in
#                   macOS Keychain (service claude-burst-together), matching
#                   whatever config.json's secondary.model is set to
#   4. OpenRouter - same shape, service claude-burst-openrouter -- but a
#                   WARN rather than a FAIL if that key isn't stored, unlike
#                   Together above. keychain-set can hold several providers'
#                   keys side by side (see README's Credential storage and
#                   naming section) without any of them being the currently
#                   configured secondary, and this machine's isn't
#                   OpenRouter -- Together still is, so its check stays a
#                   hard failure the way it always has been.
#
# Exit code is nonzero if any check fails.
set -uo pipefail

fail=0
pass() { echo "PASS: $1"; }
warn() { echo "WARN: $1"; }
bad()  { echo "FAIL: $1"; fail=1; }

echo "== gateway =="
# HTTPS-only in transparent mode, HTTP-only in base-url mode -- -k because
# the gateway presents our own local CA, which curl doesn't read from
# NODE_EXTRA_CA_CERTS. Liveness check, not a trust check. See
# transparent-root.sh's probe_gateway for the original fix; deploy.sh and
# watchdog.sh had the same http-only bug until 2026-08-31.
if curl -skf -m 3 https://127.0.0.1:7777/healthz >/dev/null 2>&1 \
  || curl -sf -m 3 http://127.0.0.1:7777/healthz >/dev/null 2>&1; then
  pass "gateway /healthz responding"
else
  bad "gateway /healthz not responding (is it enabled? launchctl list ninja.andrewbaker.claude-burst)"
fi

echo "== Anthropic =="
if [[ -n "${ANTHROPIC_API_KEY:-}" ]]; then
  code=$(curl -s -o /tmp/claude-burst-anthropic-test.json -w '%{http_code}' \
    https://api.anthropic.com/v1/messages \
    -H "x-api-key: $ANTHROPIC_API_KEY" \
    -H "anthropic-version: 2023-06-01" \
    -H "content-type: application/json" \
    -d '{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}')
  if [[ "$code" == "200" ]]; then
    pass "Anthropic API reachable and authenticated (real call, HTTP 200)"
  else
    bad "Anthropic API call failed (HTTP $code): $(cat /tmp/claude-burst-anthropic-test.json 2>/dev/null | head -c 300)"
  fi
else
  code=$(curl -s -o /dev/null -w '%{http_code}' -m 5 \
    https://api.anthropic.com/v1/messages \
    -H "content-type: application/json" -d '{}')
  if [[ "$code" == "401" || "$code" == "400" ]]; then
    pass "Anthropic API reachable (HTTP $code with no credentials -- network path is open)"
  else
    bad "Anthropic API unreachable or unexpected response (HTTP $code) -- check network/DNS/proxy"
  fi
  warn "no ANTHROPIC_API_KEY in env; skipped a real authenticated call (subscription oauth-passthrough mode has no standalone key to test with -- rely on Claude Code itself for that path)"
fi

echo "== Together AI (GLM secondary) =="
TOGETHER_KEY="${TOGETHER_API_KEY:-}"
if [[ -z "$TOGETHER_KEY" ]]; then
  TOGETHER_KEY="$(security find-generic-password -a "$(whoami)" -s claude-burst-together -w 2>/dev/null || true)"
fi
MODEL="$(python3 -c "import json;print(json.load(open('$HOME/.config/claude-burst/config.json')).get('secondary',{}).get('model','zai-org/GLM-5.3'))" 2>/dev/null || echo "zai-org/GLM-5.3")"

if [[ -z "$TOGETHER_KEY" ]]; then
  bad "no Together API key found (env TOGETHER_API_KEY or Keychain service claude-burst-together) -- run: claude-burst keychain-set --provider together"
else
  resp=$(curl -s -m 15 -o /tmp/claude-burst-together-test.json -w '%{http_code}' \
    https://api.together.xyz/v1/chat/completions \
    -H "Authorization: Bearer $TOGETHER_KEY" \
    -H "Content-Type: application/json" \
    -d "{\"model\":\"$MODEL\",\"max_tokens\":1,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
  if [[ "$resp" == "200" ]]; then
    pass "Together AI reachable and authenticated for model $MODEL (real call, HTTP 200)"
  else
    bad "Together AI call failed for model $MODEL (HTTP $resp): $(cat /tmp/claude-burst-together-test.json 2>/dev/null | head -c 300)"
  fi
fi

echo "== OpenRouter (openai-compatible secondary alternative) =="
OPENROUTER_KEY="${OPENROUTER_API_KEY:-}"
if [[ -z "$OPENROUTER_KEY" ]]; then
  OPENROUTER_KEY="$(security find-generic-password -a "$(whoami)" -s claude-burst-openrouter -w 2>/dev/null || true)"
fi

if [[ -z "$OPENROUTER_KEY" ]]; then
  warn "no OpenRouter key found (env OPENROUTER_API_KEY or Keychain service claude-burst-openrouter) -- optional, not the active secondary on this machine; run: claude-burst keychain-set --provider openrouter"
else
  OR_MODEL="${OPENROUTER_MODEL:-z-ai/glm-5.3}"
  resp=$(curl -s -m 15 -o /tmp/claude-burst-openrouter-test.json -w '%{http_code}' \
    https://openrouter.ai/api/v1/chat/completions \
    -H "Authorization: Bearer $OPENROUTER_KEY" \
    -H "Content-Type: application/json" \
    -d "{\"model\":\"$OR_MODEL\",\"max_tokens\":1,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
  if [[ "$resp" == "200" ]]; then
    pass "OpenRouter reachable and authenticated for model $OR_MODEL (real call, HTTP 200)"
  else
    bad "OpenRouter call failed for model $OR_MODEL (HTTP $resp): $(cat /tmp/claude-burst-openrouter-test.json 2>/dev/null | head -c 300)"
  fi
fi

echo
if [[ "$fail" -eq 0 ]]; then
  echo "All checks passed."
else
  echo "One or more checks failed -- see FAIL lines above."
fi
exit "$fail"
