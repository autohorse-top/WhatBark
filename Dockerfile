# whatbark server image (A12).
#
# The whatbark binary is a single static Go binary (modernc.org/sqlite, pure Go,
# no CGO) with the web/ SPA embedded via go:embed — the image only runs `serve`.
#
# Build:  docker build -t whatbark:latest .
# Run:    docker run --rm -p 8080:8080 -v "$PWD/.whatbark-data":/data whatbark:latest

# ---- build stage -----------------------------------------------------------
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/whatbark .

# ---- runtime stage ---------------------------------------------------------
FROM alpine:3.20
# Dedicated unprivileged runtime user; /data holds the SQLite DB (WAL included).
RUN adduser -D -u 10001 whatbark \
    && mkdir -p /data \
    && chown whatbark:whatbark /data
COPY --from=build /out/whatbark /usr/local/bin/whatbark
USER whatbark
VOLUME ["/data"]
EXPOSE 8080
# busybox wget is available in alpine; /healthz is served before anything else.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/whatbark", "serve"]
CMD ["-listen", "0.0.0.0:8080", "-data", "/data/whatbark.db"]
