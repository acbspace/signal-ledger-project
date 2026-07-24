FROM golang:1.25-alpine AS build

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

FROM alpine:3.21 AS runtime

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
