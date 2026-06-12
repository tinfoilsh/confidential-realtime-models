# confidential-realtime-models

Tinfoil enclave that colocates four audio models on a single H200 plus a
subdomain-routing reverse proxy. Two container images are built from this
repo in addition to the upstream model packs.

## Layout

```
.
├── router/                # Go reverse proxy: subdomain dispatch + WebSocket
│                          # subprotocol-echo fix (the only reason vanilla
│                          # vLLM /v1/realtime closes 1006 in Chromium).
├── omni/                  # Thin layer over vllm/vllm-omni:v0.18.0 with
│                          # colocation-friendly stage configs at
│                          # /opt/realtime-configs/. Used by all three vLLM
│                          # containers (qwen3-tts, voxtral-tts, voxtral-mini).
├── tinfoil-config.yml     # CVM model packs + container manifest
└── .github/workflows/
    ├── test.yml           # router go vet + go test + go build
    ├── tinfoil-build.yml  # parallel router + omni build, multi-image release
    └── tinfoil-release.yml # measure-image + attestation publish
```

## Subdomain routing

The router dispatches on the leftmost label of the request's host. Configured
via `DOMAIN` (read from `external-config.yml`):

| Subdomain                                         | Backend                          | Port |
|---------------------------------------------------|----------------------------------|------|
| `qwen3-tts.<DOMAIN>`                              | `qwen3-tts` container            | 8505 |
| `voxtral-tts.<DOMAIN>`                            | `voxtral-tts` container          | 8605 |
| `voxtral-mini-4b-realtime.<DOMAIN>`               | `voxtral-mini-4b-realtime`       | 8401 |
| `whisper-large-v3-turbo.<DOMAIN>`                 | `whisper-large-v3-turbo`         | 8001 |
| `<DOMAIN>` (root)                                 | `/health` only                   | n/a  |

The router prefers `X-Forwarded-Host` over `Host`, so the shim can terminate
TLS for `*.<DOMAIN>` and forward plain HTTP without the router having to
understand TLS. The wildcard cert is provisioned by the shim via
`tls-wildcard: true` in `tinfoil-config.yml`.

## WebSocket subprotocol-echo fix

vLLM's native `/v1/realtime` handler calls `websocket.accept()` without a
subprotocol argument, so the 101 response carries no `Sec-WebSocket-Protocol`
header even when the client offered subprotocols. RFC 6455 makes that fatal in
Chromium and Firefox (close 1006); Safari is lenient. The router patches it on
the way back via `httputil.ReverseProxy.ModifyResponse`:

- 101 responses get `Sec-WebSocket-Protocol` injected with one of the
  client's offered subprotocols, preferring `"realtime"`.
- Auth-bearing subprotocols (`openai-insecure-api-key.*`, `openai-organization.*`,
  `openai-project.*`) are filtered out so the API key never lands in plaintext
  response headers.
- Non-101 responses are untouched, so the hook is safe to apply to every backend.

See `router/main.go:fixRealtimeSubprotocolEcho` and the picker tests in
`router/main_test.go:TestPickRealtimeSubprotocol`.

## OpenAI Realtime compatibility (voxtral-mini-4b-realtime)

vLLM's `/v1/realtime` speaks its own minimal dialect, not the OpenAI Realtime
API. For `voxtral-mini-4b-realtime` only, the router carries a translation
layer (`router/openai_compat.go`) so stock OpenAI Realtime transcription
clients work by changing just the URL. Connections with `?intent=transcription`
get the translated session; connections with `?model=` keep the vLLM dialect
passthrough.

What the layer does:

- `session.created` / `session.update` / `session.updated` handshake; the
  client's declared input format is honored (PCM16 at 24kHz by default,
  resampled in-router to the model's 16kHz; 16kHz declared passes through;
  non-PCM formats are rejected).
- `transcription.delta` → `conversation.item.input_audio_transcription.delta`.
- Each client `input_audio_buffer.commit` becomes a vLLM final commit; the
  resulting `transcription.done` is emitted as `input_audio_buffer.committed`
  plus `conversation.item.input_audio_transcription.completed`. vLLM sessions
  are dead after a final commit, so the backend is re-dialed transparently for
  the next turn.
- An empty commit returns the standard `input_audio_buffer_commit_empty` error.
- Client credentials stop at this layer: auth and billing are enforced
  upstream, so the backend dial carries no Authorization header and
  credential-bearing subprotocols are never forwarded.

**⚠️ Voice activity detection is NOT emulated.** The OpenAI API defaults to
`server_vad` turn detection: the server detects silence, commits turns itself,
and emits `speech_started` / `speech_stopped` events. This layer does none of
that — a `turn_detection` config is accepted without effect (and deliberately
not echoed in `session.updated`), no speech events are emitted, and turns end
**only** when the client sends `input_audio_buffer.commit`. Push-to-talk
dictation clients, which commit once per utterance, work exactly as expected.
Hands-free clients that stream continuously and wait for the server to end
turns will see deltas accumulate but never receive a `completed` event.

See `router/openai_compat.go` and `router/openai_compat_test.go` (fake vLLM
backend matching production behavior).

## Containers

### router

`ghcr.io/tinfoilsh/confidential-realtime-models`

Go reverse proxy. `DOMAIN`, `LISTEN_ADDR`, and the per-backend URLs are env-driven.
Defaults are loopback-only and `DOMAIN=localhost` so misconfigured deployments
fail closed.

### omni

`ghcr.io/tinfoilsh/confidential-realtime-models-omni`

`FROM vllm/vllm-omni:v0.18.0` plus colocation-friendly stage configs at
`/opt/realtime-configs/{qwen3_tts,voxtral_tts}_realtime.yaml`. The voxtral-mini
container uses the same image without `--omni`/`--stage-configs-path`, since
vLLM's native `/v1/realtime` ships in the underlying vllm 0.18 base.

KV cache is pinned via `kv_cache_memory_bytes` (in stage configs for the TTS
models) or `--kv-cache-memory-bytes` (CLI for voxtral-mini), so startup is
deterministic regardless of which engine wins the GPU memory race.

## Local development

```bash
# Router unit tests + build
cd router && docker run --rm -v "$PWD":/work -w /work --network=host \
  golang:1.25-alpine sh -c "go vet ./... && go test ./... && go build ./..."

# Build the omni image (FROM vllm/vllm-omni:v0.18.0 + stage configs)
docker build -t crm-omni:test ./omni
```

For end-to-end tests against the live stack on a single H200, see
[`tinfoilsh/experiments-multi-model-vllm`](https://github.com/tinfoilsh/experiments-multi-model-vllm)
and `tinfoilsh/tf-test`'s `infra/realtime_naked` and `infra/speech_naked`.

## Release

Manual via Actions:

1. Run `tinfoil-build.yml` with a `vX.Y.Z` version. It builds router + omni in
   parallel, then `tinfoilsh/update-container-action@dmccanns/multiple-images`
   bumps both digests in `tinfoil-config.yml`, opens & merges the PR, and tags
   `vX.Y.Z`.
2. `tinfoil-release.yml` is auto-triggered by step 1 against the new tag and
   runs `measure-image-action` to attest the released config.
