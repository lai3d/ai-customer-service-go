# A container for a service with two native dependencies and a 470 MB model.
#
# The model is baked into the image rather than downloaded at startup or mounted. A cold
# start then reaches ready in a couple of seconds and needs no network, which is what a
# Kubernetes readiness probe wants; the cost is an image whose size is dominated by one
# file. Mounting it instead would trade image size for a volume that has to exist,
# be populated, and stay in step with the code that expects a particular model.
#
# The honest number is in the last stage's comment.

# ---- 1. native dependencies -------------------------------------------------------
FROM debian:bookworm-slim AS native
ARG ONNXRUNTIME_VERSION=1.29.0
ARG TOKENIZERS_VERSION=v1.27.0
ARG TARGETARCH

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /native

# ONNX Runtime is loaded at runtime rather than linked, so the shared library travels
# into the final image; the tokenizer is a Rust static library and is linked in stage 2.
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) ORT_ARCH=x64;      TOK_ASSET=libtokenizers.linux-amd64.tar.gz ;; \
      arm64) ORT_ARCH=aarch64;  TOK_ASSET=libtokenizers.linux-arm64.tar.gz ;; \
      *) echo "unsupported architecture ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    mkdir -p onnxruntime lib; \
    curl -fsSL "https://github.com/microsoft/onnxruntime/releases/download/v${ONNXRUNTIME_VERSION}/onnxruntime-linux-${ORT_ARCH}-${ONNXRUNTIME_VERSION}.tgz" \
      | tar -xz -C onnxruntime --strip-components=1; \
    curl -fsSL "https://github.com/daulet/tokenizers/releases/download/${TOKENIZERS_VERSION}/${TOK_ASSET}" \
      | tar -xz -C lib

# ---- 2. the embedding model -------------------------------------------------------
# Its own stage so a code change does not re-download 470 MB, and so the layer is
# cached independently of the build.
FROM debian:bookworm-slim AS model
ARG MODEL_REPO=https://huggingface.co/intfloat/multilingual-e5-small/resolve/main
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /model
# The fp32 export, not a quantised one: the int8 builds are per-architecture and would
# make this image unportable.
RUN curl -fsSL -o model.onnx "${MODEL_REPO}/onnx/model.onnx" \
 && curl -fsSL -o tokenizer.json "${MODEL_REPO}/tokenizer.json"

# ---- 3. build ---------------------------------------------------------------------
FROM golang:1.26-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends g++ \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=native /native/lib/libtokenizers.a third_party/lib/libtokenizers.a

# CGO_ENABLED=1 is not a default worth relying on when cross-building, and it is the
# whole reason this image needs a compiler at all. The binary is dynamically linked
# against libc and loads libonnxruntime at runtime, so it is not portable to a scratch
# image -- which is why the final stage is debian-slim rather than distroless static.
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- 4. runtime -------------------------------------------------------------------
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --create-home app

COPY --from=native /native/onnxruntime/lib/libonnxruntime.so* /usr/local/lib/
RUN ldconfig

WORKDIR /app
COPY --from=build  /out/server                       /app/server
COPY --from=model  /model/model.onnx                 /app/model-cache/multilingual-e5-small/model.onnx
COPY --from=model  /model/tokenizer.json             /app/model-cache/multilingual-e5-small/tokenizer.json
COPY               corpus/faq.json                   /app/corpus/faq.json

ENV ONNXRUNTIME_LIB_PATH=/usr/local/lib/libonnxruntime.so \
    EMBEDDING_MODEL_PATH=/app/model-cache/multilingual-e5-small/model.onnx \
    EMBEDDING_TOKENIZER_PATH=/app/model-cache/multilingual-e5-small/tokenizer.json \
    FAQ_CORPUS_PATH=/app/corpus/faq.json \
    HTTP_ADDR=:8081

USER 10001
EXPOSE 8081

# 1.1 GB, measured rather than estimated, and it is worth knowing where it goes:
#
#   470 MB  model.onnx            the fp32 export of multilingual-e5-small
#    74 MB  libonnxruntime.so
#    52 MB  the Go binary         cgo, so not a small static one
#    17 MB  tokenizer.json        a SentencePiece vocabulary is not small either
#   ~90 MB  debian-slim + certs
#
# Calling an embedding API instead would cut this to roughly 60 MB and add a vendor, a
# key, and a network round trip per query. docs/retrieval.md has that trade measured
# rather than argued: the in-process model answers in 2 ms and costs nothing per query.
HEALTHCHECK --interval=15s --timeout=3s --start-period=20s --retries=3 \
  CMD ["/app/server", "-healthcheck"]

ENTRYPOINT ["/app/server"]
