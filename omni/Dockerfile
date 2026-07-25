# Thin layer over upstream vllm-omni that bakes in colocation-friendly stage
# configs for the realtime stack. Used by the qwen3-tts and voxtral-tts
# containers in tinfoil-config.yml. Voxtral-mini-realtime uses the audio image
# instead (no omni features needed for STT realtime).
#
# Stock vllm-omni stage configs claim ~30-90% gpu_memory_utilization per stage,
# which prevents colocating multiple TTS models on a single H200. Our trimmed
# variants live under stage_configs/ and are mounted at /opt/realtime-configs/
# so the container command can reference them with a stable path.
#
# Patches in patches/ are -p1 unified diffs rooted at /; see patches/README.md.
# They target usr/local/lib/python3.12/dist-packages/... to match the base
# image. Each patch cites its upstream PR so it can be retired after a bump.
FROM vllm/vllm-omni:v0.24.0@sha256:ffb9a2a8e78a848e80ce40674ab7f36d7653358b30e616629f3ba9a235873b79

COPY patches/ /tmp/tinfoil-patches/
RUN set -eux; \
    test -x /usr/bin/patch; \
    cd /; \
    for p in /tmp/tinfoil-patches/*.patch; do \
        echo "Applying $(basename "$p")"; \
        /usr/bin/patch -p1 --no-backup-if-mismatch < "$p"; \
    done; \
    find /usr/local/lib/python3.12/dist-packages/vllm -name '__pycache__' -type d -exec rm -rf {} + || true; \
    rm -rf /tmp/tinfoil-patches; \
    python3 -c "import vllm; print('vllm', vllm.__version__, 'with tinfoil realtime patches')"

COPY stage_configs/ /opt/realtime-configs/
