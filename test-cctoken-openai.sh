#!/bin/sh
set -eu

# Minimal OpenAI-compatible request against the cctoken CLIProxyAPI endpoint.
# Required: export CCTOKEN_API_KEY="your access key"
# Optional: export CCTOKEN_BASE_URL="https://cli.cctoken.fun"
# Optional: export CCTOKEN_OPENAI_MODEL="gpt-5.4"

BASE_URL="${CCTOKEN_BASE_URL:-https://cli.cctoken.fun}"
API_KEY="${CCTOKEN_API_KEY:-${CCTOKEN_ACCESS_TOKEN:-}}"
MODEL="${CCTOKEN_OPENAI_MODEL:-gpt-5.4}"

if [ -z "$API_KEY" ]; then
  echo "Missing API key. Set CCTOKEN_API_KEY first." >&2
  exit 1
fi

response="$(
  curl -sS "${BASE_URL%/}/v1/chat/completions" \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    --data-binary @- <<EOF
{
  "model": "${MODEL}",
  "messages": [
    {
      "role": "user",
      "content": "请只回复 ok"
    }
  ],
  "stream": false
}
EOF
)"

if command -v jq >/dev/null 2>&1; then
  printf '%s\n' "$response" | jq .
else
  printf '%s\n' "$response"
fi
