# llama-swap Slim Image

A minimal Docker image for llama-swap that ships **only the llama-swap binary** with Docker CLI and `nvidia-smi` for sidecar orchestration and GPU monitoring. No AI inference engines (llama.cpp, whisper.cpp, stable-diffusion.cpp) are included.

## What's included

| Tool | Purpose |
|------|---------|
| `llama-swap` | The main OpenAI-compatible API server |
| `docker` | CLI for managing sidecar containers |
| `nvidia-smi` | GPU monitoring (from `nvidia-utils`) |
| `curl` | HTTP calls |
| `jq` | JSON parsing |

## What's NOT included

- `llama-server` / `llama-cli`
- `whisper-server` / `whisper-cli`
- `sd-server` / `sd-cli`
- CUDA development toolkit
- Python, numpy, sentencepiece

## Build

### Using the build script

```bash
./docker/build-image.sh --slim
```

### Manually with buildx (multi-arch)

```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -f docker/slim/Dockerfile \
  -t llama-swap:slim \
  .
```

### Single architecture

```bash
docker build \
  -f docker/slim/Dockerfile \
  -t llama-swap:slim \
  .
```

## Run

```bash
docker run -it --rm \
  --gpus all \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /path/to/models:/models \
  -p 8080:8080 \
  llama-swap:slim \
  -config /etc/llama-swap/config/config.yaml \
  -listen 0.0.0.0:8080
```

### Required mounts

| Mount | Purpose |
|-------|---------|
| `/var/run/docker.sock` | Docker socket for sidecar container management |
| `/path/to/models` | Model files (mount to whatever path your config expects) |

## Configuration

An example config is pre-loaded at `/etc/llama-swap/config/config.yaml`. Override it by mounting your own:

```bash
-v /path/to/my-config.yaml:/etc/llama-swap/config/config.yaml
```

## Image size

Approximately **150-250 MB**, compared to ~8-12 GB for the unified image with all AI engines.

## Sidecar setup

Since this image does not include inference engines, you must configure llama-swap to use external inference backends. This is typically done by:

1. Pointing llama-swap's config to inference servers running on the host or in separate containers
2. Using llama-swap's sidecar management features to start/stop inference containers as needed

See the main [llama-swap documentation](../../README.md) for configuration details.
