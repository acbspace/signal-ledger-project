# Must stay at or above go.mod's toolchain floor: an older base would make the
# build silently download a second toolchain, or fail outright without a network.
FROM golang:1.26-alpine AS build

WORKDIR /src

# go.sum belongs in the dependency layer with go.mod: without it `go mod download`
# resolves modules with nothing to check them against, so the build silently
# accepts whatever the proxy serves. `go mod verify` then re-checks the unpacked
# cache against those recorded hashes.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

FROM alpine:3.24 AS runtime

# Pre-create the shared data directory owned by the runtime user so a freshly
# initialized named volume inherits writable ownership.
RUN addgroup -S app && adduser -S app -G app \
    && mkdir -p /var/lib/signalledger/documents \
    && chown -R app:app /var/lib/signalledger
USER app

FROM runtime AS api
COPY --from=build --chown=app:app /out/api /app/api
EXPOSE 8080
ENTRYPOINT ["/app/api"]

FROM runtime AS worker
COPY --from=build --chown=app:app /out/worker /app/worker
ENTRYPOINT ["/app/worker"]
