# confidential-realtime-models

A tiny in-enclave reverse proxy for the collocated realtime stack.

It routes fixed API paths to the validated local model ports:

- `/v1/audio/speech` -> `qwen3-tts`
- `/v1/realtime` -> `voxtral-mini-realtime`

## Configuration

Environment variables:

- `LISTEN_ADDR` default `:8080`
- `QWEN_URL` default `http://127.0.0.1:8505`
- `VOXTRAL_URL` default `http://127.0.0.1:8402`

## Local build

```bash
docker build -t confidential-realtime-models .
```

## Test

```bash
docker run --rm -v "$PWD":/app -w /app golang:1.25-alpine sh -lc 'go test ./...'
```
