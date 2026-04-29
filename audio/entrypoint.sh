#!/bin/bash
# Entrypoint that runs both the audio proxy and vLLM

set -e

# Infer the backend vLLM port from CLI args unless explicitly provided.
VLLM_INTERNAL_PORT="${VLLM_PORT:-}"
if [ -z "${VLLM_INTERNAL_PORT}" ]; then
  prev=""
  for arg in "$@"; do
    if [ "${prev}" = "--port" ]; then
      VLLM_INTERNAL_PORT="${arg}"
      break
    fi
    prev="${arg}"
  done
fi
VLLM_INTERNAL_PORT="${VLLM_INTERNAL_PORT:-8001}"
export VLLM_URL="${VLLM_URL:-http://127.0.0.1:${VLLM_INTERNAL_PORT}}"

# Start the FastAPI proxy that fixes the realtime-WebSocket subprotocol echo
# (Chrome/Firefox close 1006 against vanilla vLLM otherwise — see audio_proxy.py
# header comment). Detached session so terminal signals don't kill it.
echo "Starting realtime audio proxy on port ${PROXY_PORT:-8082}..."
setsid python3 /app/audio_proxy.py &
PROXY_PID=$!

sleep 2

# Execute vLLM with all passed arguments (mimics vLLM's ENTRYPOINT ["vllm" "serve"])
echo "Starting vLLM..."
exec vllm serve "$@"
