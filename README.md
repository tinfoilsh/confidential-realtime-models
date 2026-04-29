# confidential-realtime-models

Tinfoil enclave that colocates three audio models on a single H200 plus a
subdomain-routing reverse proxy. Three container images are built from this
repo in addition to the upstream model packs.

## Layout

```
.
├── router/                # Go reverse proxy that dispatches by subdomain
├── audio/                 # FastAPI proxy in front of vLLM /v1/realtime
│                          # (fixes the WebSocket subprotocol-echo bug that
│                          #  closes Chromium/Firefox connections with 1006)
├── omni/                  # Thin layer over vllm/vllm-omni:v0.18.0 that bakes
│                          # in colocation-friendly stage configs for TTS
├── tinfoil-config.yml     # CVM model packs + container manifest
└── .github/workflows/
    ├── test.yml           # router go test + audio_proxy syntax/picker tests
    ├── tinfoil-build.yml  # Multi-image release: builds router + audio + omni,
    │                      # then update-container-action @ multiple-images
    │                      # bumps all three digests in tinfoil-config.yml
    └── tinfoil-release.yml # measure-image + attestation publish
```

## Subdomain routing

The router dispatches on the leftmost label of the request's host. Configured
via `DOMAIN` (default `realtime.tinfoil.sh`):

| Subdomain                                         | Backend                          | Port |
|---------------------------------------------------|----------------------------------|------|
| `qwen3-tts.realtime.tinfoil.sh`                   | `qwen3-tts` container            | 8505 |
| `voxtral-tts.realtime.tinfoil.sh`                 | `voxtral-tts` container          | 8605 |
| `voxtral-mini-4b-realtime.realtime.tinfoil.sh`    | `voxtral-mini-4b-realtime`       | 8402 |
| `realtime.tinfoil.sh` (root)                      | `/health` only                   | n/a  |

The router prefers `X-Forwarded-Host` over `Host`, so the shim can terminate
TLS for `*.realtime.tinfoil.sh` and forward plain HTTP without the router
having to understand TLS. The wildcard cert is provisioned by the shim via
`tls-wildcard: true` in `tinfoil-config.yml`. `DOMAIN` itself is read from
`external-config.yml` (matching the model-router pattern), so the same image
works for staging and prod.

## Containers

### router

`ghcr.io/tinfoilsh/confidential-realtime-models`

Go reverse proxy (~280 lines). Builds from `router/`. Env knobs:

- `LISTEN_ADDR` (default `:8080`)
- `DOMAIN` (default `realtime.tinfoil.sh`)
- `QWEN_TTS_URL` / `VOXTRAL_TTS_URL` / `VOXTRAL_MINI_REALTIME_URL` (defaults
  point at loopback ports inside the enclave)

### audio

`ghcr.io/tinfoilsh/confidential-realtime-models-audio`

`FROM vllm/vllm-openai:v0.20.0-cu130` plus a thin FastAPI proxy
(`audio_proxy.py`) that fixes the realtime-WebSocket subprotocol-echo bug
described in the file's header. Used by the `voxtral-mini-4b-realtime`
container only — the TTS containers don't need a proxy.

This image was previously published from the standalone
`tinfoilsh/vllm-openai-audio` repo. Consolidated here, the audio proxy was
also slimmed (vLLM 0.18 -> 0.20 fixes the M4A/WebM bugs that justified the
old `convert_to_wav` / FFmpeg / librosa stack — see the cleanup commit for
the full list).

### omni

`ghcr.io/tinfoilsh/confidential-realtime-models-omni`

`FROM vllm/vllm-omni:v0.18.0` plus colocation-friendly stage configs at
`/opt/realtime-configs/`:

- `qwen3_tts_realtime.yaml` (`gpu_memory_utilization: 0.10` per stage)
- `voxtral_tts_realtime.yaml` (`0.10` + `0.05` per stage)

The stock vllm-omni stage configs claim 30-90% of one H200 per stage, which
prevents colocating multiple TTS models. These trims fit the realtime stack
(qwen3-tts + voxtral-tts + voxtral-mini-realtime) under ~80 GiB on H200.

This image replaces the deprecated `tinfoilsh/vllm-omni`
`v0.17.0rc1-tinfoil.2` fork — we now consume upstream vllm-omni directly.

## Local development

```bash
# Router unit tests + build
cd router && docker run --rm -v "$PWD":/work -w /work golang:1.25-alpine \
  sh -c "go test ./... && go build ./..."

# Audio proxy syntax check + picker tests
cd audio && python3 -c "import ast; ast.parse(open('audio_proxy.py').read())"
# (Full picker test cases live in .github/workflows/test.yml)
```

For end-to-end tests against the three live models on a single H200, see
[`tinfoilsh/experiments-multi-model-vllm`](https://github.com/tinfoilsh/experiments-multi-model-vllm)
— specifically `scripts/run_tts_trio_stack.sh`.

## Release

Manual via Actions:

1. Run `tinfoil-build.yml` with a `vX.Y.Z` version. It builds all three images
   in parallel, then `tinfoilsh/update-container-action@dmccanns/multiple-images`
   bumps the three `image:` digests in `tinfoil-config.yml`, opens & merges
   the PR, and tags `vX.Y.Z`.
2. `tinfoil-release.yml` is auto-triggered by step 1 against the new tag and
   runs `measure-image-action` to attest the released config.

## Background

- Bug fix: WebSocket subprotocol-echo (closes Chromium/Firefox with 1006 against
  vanilla vLLM). The fix lives in `audio/audio_proxy.py`'s
  `pick_realtime_subprotocol()` and is also worth upstreaming to
  vllm-project/vllm `vllm/entrypoints/openai/realtime/connection.py:916`.
- vllm-omni dropped: `tinfoilsh/vllm-omni` was a fork of `vllm-project/vllm-omni`
  on `v0.17.0rc1` carrying tinfoil packaging. `tinfoilsh/vllm-omni:main` is now
  in sync with upstream `v0.18.0+`, and we consume `vllm/vllm-omni:v0.18.0`
  directly instead of building our own.
- vLLM bumped 0.18 -> 0.20 in the audio image. Per
  [vLLM 0.18 PR #35109](https://github.com/vllm-project/vllm/pull/35109)
  M4A/WebM uploads work natively, and 0.20 cleaned up audio deps
  (#39524, #39079, #39997). The 350+ lines of conversion code in the previous
  `audio_proxy.py` are gone.
