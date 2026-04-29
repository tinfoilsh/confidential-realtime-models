#!/usr/bin/env python3
"""
Thin FastAPI front for vLLM with two responsibilities:

  1. /v1/realtime  — transparent WebSocket bridge to vLLM's native realtime
     endpoint. Echoes back exactly one client-offered subprotocol on the 101
     handshake so Chromium/Firefox don't abort with close 1006. Auth-bearing
     subprotocols (openai-insecure-api-key.*, openai-organization.*,
     openai-project.*) are filtered out so they aren't reflected in response
     headers (see pick_realtime_subprotocol() below).

  2. /v1/chat/completions — pass-through with an explicit MAX_BODY_SIZE cap
     (Starlette has no native body cap; vLLM doesn't enforce one either).

Health/metrics are forwarded for convenience (the Go shim can also expose
upstream vLLM directly if this proxy is later removed).

Audio format conversion (FFmpeg/librosa/scipy/soundfile/pydub) used to live
here to work around vLLM #26808 (M4A/WebM uploads returning 500). That bug
was fixed upstream in vLLM 0.18.0 by PR #35109 and the dependency hygiene
was cleaned up in vLLM 0.20.0 (#39524, #39079, #39997). On vLLM >= 0.20 the
conversion is unnecessary, so the /v1/audio/transcriptions and
/v1/audio/translations endpoints, the convert_to_wav helper, and the
process_messages_audio chat-multimodal preprocessor have been removed.
"""

import asyncio
import json
import os
from urllib.parse import urlparse, urlunparse

import httpx
import websockets
from fastapi import FastAPI, HTTPException, Request, WebSocket, WebSocketDisconnect
from fastapi.responses import JSONResponse, PlainTextResponse, StreamingResponse
from websockets.exceptions import ConnectionClosed

app = FastAPI(docs_url=None, redoc_url=None, openapi_url=None)

VLLM_URL = os.environ.get("VLLM_URL", "http://127.0.0.1:8001")
MAX_BODY_SIZE = int(os.environ.get("MAX_BODY_SIZE_MB", "1024")) * 1024 * 1024


def to_websocket_url(http_url: str) -> str:
    parsed = urlparse(http_url)
    scheme = "wss" if parsed.scheme == "https" else "ws"
    return urlunparse((scheme, parsed.netloc, parsed.path, parsed.params, parsed.query, parsed.fragment))


VLLM_WS_URL = to_websocket_url(VLLM_URL)


# ---------------------------------------------------------------------------
# Chat completions proxy (body-size cap is the only reason we still proxy)
# ---------------------------------------------------------------------------

@app.post("/v1/chat/completions")
async def chat_completions(request: Request):
    """Body-size-capped pass-through for vLLM /v1/chat/completions.

    Both streaming (SSE) and non-streaming JSON responses are forwarded.
    """
    body_bytes = await request.body()
    if len(body_bytes) > MAX_BODY_SIZE:
        raise HTTPException(
            status_code=413,
            detail=f"Request body too large. Maximum size: {MAX_BODY_SIZE // (1024*1024)}MB",
        )
    body = json.loads(body_bytes)

    is_streaming = body.get("stream", False)
    print(f"[audio_proxy] Chat completion: model={body.get('model')}, streaming={is_streaming}", flush=True)

    if is_streaming:
        client = httpx.AsyncClient(timeout=httpx.Timeout(300.0, connect=10.0))
        try:
            req = client.build_request("POST", f"{VLLM_URL}/v1/chat/completions", json=body)
            resp = await client.send(req, stream=True)
        except httpx.RequestError as e:
            await client.aclose()
            raise HTTPException(status_code=502, detail=f"vLLM connection error: {str(e)}")

        async def event_stream():
            try:
                async for chunk in resp.aiter_bytes():
                    yield chunk
            finally:
                await resp.aclose()
                await client.aclose()

        return StreamingResponse(
            event_stream(),
            status_code=resp.status_code,
            media_type="text/event-stream",
            headers={"Cache-Control": "no-cache"},
        )

    async with httpx.AsyncClient(timeout=300.0) as client:
        try:
            resp = await client.post(f"{VLLM_URL}/v1/chat/completions", json=body)
        except httpx.RequestError as e:
            raise HTTPException(status_code=502, detail=f"vLLM connection error: {str(e)}")

    return JSONResponse(
        status_code=resp.status_code,
        content=(
            resp.json()
            if resp.headers.get("content-type", "").startswith("application/json")
            else {"error": resp.text}
        ),
    )


# ---------------------------------------------------------------------------
# Realtime WebSocket bridge with RFC 6455-compliant subprotocol echo
# ---------------------------------------------------------------------------

# Subprotocols that carry credentials and must never be echoed back in the
# server's handshake response (RFC 6455 echoes go in plaintext headers).
_SENSITIVE_SUBPROTOCOL_PREFIXES = (
    "openai-insecure-api-key.",
    "openai-organization.",
    "openai-project.",
)


def pick_realtime_subprotocol(offered: list[str] | None) -> str | None:
    """Pick the subprotocol to echo back on a Realtime WebSocket handshake.

    Chrome/Firefox enforce RFC 6455 strictly: when the client offers any
    subprotocols, the server MUST echo back exactly one of them in the 101
    response, otherwise the browser aborts with close code 1006. Safari is
    lenient and accepts a missing echo, which is why this only manifested on
    Chromium-based browsers.

    We prefer the "realtime" tag (matching OpenAI's own Realtime API) and
    explicitly avoid echoing any auth-bearing subprotocol so the API key
    isn't reflected in response headers.
    """
    if not offered:
        return None
    safe = [
        proto for proto in offered
        if proto and not proto.startswith(_SENSITIVE_SUBPROTOCOL_PREFIXES)
    ]
    if "realtime" in safe:
        return "realtime"
    return safe[0] if safe else None


@app.websocket("/v1/realtime")
async def realtime_proxy(websocket: WebSocket):
    """Transparent WebSocket bridge to vLLM /v1/realtime.

    The client must already send PCM16 chunks in the format expected by vLLM.
    """
    chosen_subprotocol = pick_realtime_subprotocol(websocket.scope.get("subprotocols"))
    await websocket.accept(subprotocol=chosen_subprotocol)
    backend_url = f"{VLLM_WS_URL}/v1/realtime"
    print(f"[audio_proxy] Realtime proxy connect: {backend_url}", flush=True)

    try:
        async with websockets.connect(backend_url, max_size=None) as backend:
            async def client_to_backend():
                while True:
                    message = await websocket.receive()
                    msg_type = message.get("type")
                    if msg_type == "websocket.disconnect":
                        break
                    if message.get("text") is not None:
                        await backend.send(message["text"])
                    elif message.get("bytes") is not None:
                        await backend.send(message["bytes"])

            async def backend_to_client():
                while True:
                    response = await backend.recv()
                    if isinstance(response, bytes):
                        await websocket.send_bytes(response)
                    else:
                        await websocket.send_text(response)

            tasks = [
                asyncio.create_task(client_to_backend()),
                asyncio.create_task(backend_to_client()),
            ]
            done, pending = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
            for task in pending:
                task.cancel()
            for task in done:
                exc = task.exception()
                if exc and not isinstance(exc, (WebSocketDisconnect, ConnectionClosed, asyncio.CancelledError)):
                    raise exc
    except WebSocketDisconnect:
        pass
    except Exception as e:
        print(f"[audio_proxy] Realtime proxy error: {type(e).__name__}: {e}", flush=True)
        await websocket.close(code=1011, reason="backend error")


# ---------------------------------------------------------------------------
# Health / metrics passthroughs
# ---------------------------------------------------------------------------

@app.get("/health")
async def health():
    async with httpx.AsyncClient(timeout=10.0) as client:
        try:
            resp = await client.get(f"{VLLM_URL}/health")
            return JSONResponse(status_code=resp.status_code, content=resp.json() if resp.text else {})
        except httpx.RequestError as e:
            raise HTTPException(status_code=503, detail=f"vLLM health check failed: {str(e)}")


@app.get("/metrics")
async def metrics():
    async with httpx.AsyncClient(timeout=30.0) as client:
        try:
            resp = await client.get(f"{VLLM_URL}/metrics")
            return PlainTextResponse(content=resp.text, status_code=resp.status_code)
        except httpx.RequestError as e:
            raise HTTPException(status_code=503, detail=f"vLLM metrics failed: {str(e)}")


if __name__ == "__main__":
    import uvicorn

    port = int(os.environ.get("PROXY_PORT", "8082"))
    uvicorn.run(app, host="0.0.0.0", port=port)
