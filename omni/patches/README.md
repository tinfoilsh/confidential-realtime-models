# vLLM patches

Unified-diff patches applied on top of the base image in `../Dockerfile`.
Applied in filename order at build time.

## Rules

- Diffs are `-p1` rooted at `/`, so paths start with
  `usr/local/lib/python3.12/dist-packages/...`
- Each patch is one reviewable change with a prefixed number
  (`NNNN-short-slug.patch`)
- Each patch includes an in-code comment citing the upstream issue/PR so
  a future reader knows when it can be retired after a base-image bump

## Current patches

### 0001 — voxtral-realtime-fix-token-feedback-timeout-silent-hang

Upstream: [vllm-project/vllm#44461](https://github.com/vllm-project/vllm/pull/44461)
(merged to `main` 2026-07-05, not yet in a release tag as of v0.23.0).

vLLM's Voxtral realtime `feed_tokens()` task wraps `input_stream.get()` in
`asyncio.wait_for(..., VLLM_ENGINE_ITERATION_TIMEOUT_S)` (default 60s). A
realtime transcription session that sits idle for 60s before its first turn —
which the router's eager backend dial + readiness commit makes the default
case — hits this timeout, `feed_tokens()` dies silently, and the WebSocket
stays open while all subsequent audio is silently dropped (no deltas, no
error, no close). See `infra/realtime_zombie/` in tf-test for the repro.

The fix removes the `wait_for` wrapper so `feed_tokens()` waits indefinitely
for the next engine output; the task is still bounded by the request
lifecycle (`token_task.cancel()` in the `finally` block). Retire this patch
once the base image is bumped to a vLLM release that contains #44461.

### 0002 — voxtral-tts-fix-codes-audio-feedback

Upstream: [vllm-project/vllm-omni#4954](https://github.com/vllm-project/vllm-omni/pull/4954)
(merged to `main` 2026-07-08, first released in v0.25.0rc1; not in any
v0.24.x tag).

vllm-omni's inter-stage payload refactor (#4527, in v0.24.0) moved Voxtral
TTS decode-feedback frames from a top-level `audio` key to nested
`codes.audio`, but `tts_preprocess` still read only the old key. Decode
feedback therefore lost all previous audio-code embeddings and
`/v1/audio/speech` returned speech unrelated to the input text (issue #24
in this repo, upstream [#4949](https://github.com/vllm-project/vllm-omni/issues/4949)).

The fix reads `codes.audio` and falls back to the old top-level key. Retire
this patch once the base image is bumped to a vllm-omni release that
contains #4954 (v0.25.0 or later).
