# syntax=docker/dockerfile:1.7
# go-core: ядро тренажёра (API + раздача SPA + TLS контура). Контекст сборки — КОРЕНЬ репозитория
# (нужны db/migrations и frontend):  docker build -f go-core/Dockerfile -t lct/go-core .
# Цели: go-core (по умолчанию) и fakeai (имитатор ai-service для стенда без Python).

ARG GO_VERSION=1.26
ARG NODE_VERSION=22

# ---------------------------------------------------------------- Go
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src/go-core
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go-core/go.mod go-core/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY go-core/ ./
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/gocore ./cmd/gocore && \
    go build -ldflags "-s -w" -o /out/fakeai ./cmd/fakeai

# ---------------------------------------------------------------- SPA (VITE_USE_MOCKS=false — реальный API)
FROM node:${NODE_VERSION}-alpine AS web
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund
COPY frontend/ ./
ENV VITE_USE_MOCKS=false VITE_API_BASE=/api/v1
RUN npm run build

# ---------------------------------------------------------------- fakeai
FROM alpine:3.22 AS fakeai
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app && mkdir -p /data/tts && chown app /data/tts
COPY --from=build /out/fakeai /usr/local/bin/fakeai
USER app
ENV FAKEAI_ADDR=:8000 TTS_DIR=/data/tts
EXPOSE 8000
ENTRYPOINT ["fakeai"]

# ---------------------------------------------------------------- go-core (по умолчанию)
FROM alpine:3.22 AS go-core
# pg_dump той же мажорной версии, что сервер PostgreSQL (бэкапы, ТЗ: не реже раза в сутки)
RUN apk add --no-cache ca-certificates tzdata postgresql16-client && adduser -D -u 10001 app && \
    mkdir -p /data/tts /data/backups /data/tls && chown -R app /data
COPY --from=build /out/gocore /usr/local/bin/gocore
COPY db/migrations /app/migrations
COPY --from=web /src/frontend/dist /app/web
USER app
ENV GOCORE_HTTP_ADDR=:8080 \
    GOCORE_HTTPS_ADDR=:8443 \
    GOCORE_MIGRATIONS_DIR=/app/migrations \
    GOCORE_STATIC_DIR=/app/web \
    GOCORE_TLS_DIR=/data/tls \
    GOCORE_BACKUP_DIR=/data/backups \
    TTS_DIR=/data/tts \
    GOCORE_LOG_FORMAT=json \
    TZ=Europe/Moscow
EXPOSE 8080 8443
HEALTHCHECK --interval=15s --timeout=3s --start-period=20s --retries=5 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["gocore"]
CMD ["serve"]
