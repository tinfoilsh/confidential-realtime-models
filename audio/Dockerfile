FROM vllm/vllm-openai:v0.20.0-cu130@sha256:04563c302537a91aa49ebdfbceda96111c5712275999b7e8804fa598f0b5641d

# vLLM 0.20+ ships pyav/soundfile in common requirements (see vllm-project/vllm#39997)
# and handles M4A/WebM uploads natively (see vllm-project/vllm#35109), so the
# previous FFmpeg + librosa/scipy/soundfile/pydub workaround stack is gone.

COPY requirements.txt .
RUN uv pip install --system --require-hashes -r requirements.txt

COPY audio_proxy.py /app/audio_proxy.py
COPY entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

# Proxy listens on 8082 by default; backend port is inferred from --port.
ENV PROXY_PORT=8082

ENTRYPOINT ["/app/entrypoint.sh"]
