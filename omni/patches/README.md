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
