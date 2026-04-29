# Thin layer over upstream vllm-omni that bakes in colocation-friendly stage
# configs for the realtime stack. Used by the qwen3-tts and voxtral-tts
# containers in tinfoil-config.yml. Voxtral-mini-realtime uses the audio image
# instead (no omni features needed for STT realtime).
#
# Stock vllm-omni stage configs claim ~30-90% gpu_memory_utilization per stage,
# which prevents colocating multiple TTS models on a single H200. Our trimmed
# variants live under stage_configs/ and are mounted at /opt/realtime-configs/
# so the container command can reference them with a stable path.
FROM vllm/vllm-omni:v0.18.0@sha256:6412a0c6cb03321761a40a65301cdcd87a4ef6bc7a75510c69b039c301cdbf8f

COPY stage_configs/ /opt/realtime-configs/
