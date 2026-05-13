# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build

WORKDIR /src
RUN apk add --no-cache ca-certificates tzdata

COPY go.mod ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -trimpath \
    -buildvcs=false \
    -ldflags "-X main.version=${VERSION} -s -w" \
    -o /out/verisure-roborock ./cmd/verisure-roborock

FROM python:3.12-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir python-roborock==5.10.0 \
    && useradd --system --uid 10001 --home-dir /nonexistent --no-create-home app

WORKDIR /app
COPY --from=build /out/verisure-roborock /usr/local/bin/verisure-roborock
COPY scripts/roborock_cloud.py /app/scripts/roborock_cloud.py

RUN mkdir -p /data && chown app:app /data
USER app

ENV STORE_PATH=/data/state.json
ENV ROBOROCK_AUTH_PATH=/data/roborock-auth.json
ENV ROBOROCK_HELPER=/app/scripts/roborock_cloud.py
ENV ROBOROCK_PYTHON=python3
ENV PYTHONDONTWRITEBYTECODE=1
ENV XIAOMI_AUTH_PATH=/data/xiaomi-auth.json
ENV HTTP_ADDR=:8080

VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/healthz', timeout=2).read()" || exit 1

ENTRYPOINT ["/usr/local/bin/verisure-roborock"]
