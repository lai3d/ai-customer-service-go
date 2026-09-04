#!/usr/bin/env bash
# Fetches the two native artefacts an in-process embedding model needs, plus the model
# itself. Everything lands in gitignored directories; nothing here is committed.
#
# This script is the honest cost of running the embedding model in-process. A service
# that called an embedding API would need none of it -- and would need an API key, a
# second vendor, and a network round trip per query instead.
set -euo pipefail

cd "$(dirname "$0")/.."

TOKENIZERS_VERSION="${TOKENIZERS_VERSION:-v1.27.0}"
MODEL_DIR="model-cache/multilingual-e5-small"
MODEL_REPO="https://huggingface.co/intfloat/multilingual-e5-small/resolve/main"

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)  TOKENIZERS_ASSET="libtokenizers.darwin-arm64.tar.gz" ;;
  Darwin-x86_64) TOKENIZERS_ASSET="libtokenizers.darwin-x86_64.tar.gz" ;;
  Linux-aarch64) TOKENIZERS_ASSET="libtokenizers.linux-arm64.tar.gz" ;;
  Linux-arm64)   TOKENIZERS_ASSET="libtokenizers.linux-arm64.tar.gz" ;;
  Linux-x86_64)  TOKENIZERS_ASSET="libtokenizers.linux-amd64.tar.gz" ;;
  *) echo "no prebuilt tokenizer library for $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac

mkdir -p third_party/lib "$MODEL_DIR"

if [ ! -f third_party/lib/libtokenizers.a ]; then
  echo "==> tokenizer library ($TOKENIZERS_ASSET)"
  curl -fsSL "https://github.com/daulet/tokenizers/releases/download/${TOKENIZERS_VERSION}/${TOKENIZERS_ASSET}" \
    | tar -xz -C third_party/lib
fi

# ONNX Runtime itself is loaded at runtime rather than linked, so a system install is
# fine. On macOS: brew install onnxruntime. On Linux the release tarball works; set
# ONNXRUNTIME_LIB_PATH if it is somewhere this cannot guess.
if [ "$(uname -s)" = "Linux" ] && [ ! -f third_party/onnxruntime/lib/libonnxruntime.so ]; then
  ORT_VERSION="${ORT_VERSION:-1.29.0}"
  case "$(uname -m)" in
    x86_64)  ORT_ARCH="x64" ;;
    aarch64|arm64) ORT_ARCH="aarch64" ;;
    *) echo "no ONNX Runtime build for $(uname -m)" >&2; exit 1 ;;
  esac
  echo "==> ONNX Runtime ${ORT_VERSION} (${ORT_ARCH})"
  mkdir -p third_party/onnxruntime
  curl -fsSL "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/onnxruntime-linux-${ORT_ARCH}-${ORT_VERSION}.tgz" \
    | tar -xz -C third_party/onnxruntime --strip-components=1
fi

# The fp32 export, not a quantised one: the int8 builds are per-architecture and would
# make a container image unportable.
if [ ! -f "$MODEL_DIR/model.onnx" ]; then
  echo "==> multilingual-e5-small (470 MB, once)"
  curl -fsSL -o "$MODEL_DIR/model.onnx" "$MODEL_REPO/onnx/model.onnx"
fi
if [ ! -f "$MODEL_DIR/tokenizer.json" ]; then
  echo "==> tokenizer.json"
  curl -fsSL -o "$MODEL_DIR/tokenizer.json" "$MODEL_REPO/tokenizer.json"
fi

echo "==> ready"
