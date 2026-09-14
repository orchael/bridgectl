# syntax=docker/dockerfile:1
FROM golang:1.26 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -o /out/bridgectl ./cmd/bridgectl

FROM alpine:3.22

RUN apk add --no-cache ca-certificates && \
    addgroup -S collector && \
    adduser -S -G collector collector && \
    mkdir -p /data && \
    chown collector:collector /data

COPY --from=build /out/bridgectl /usr/local/bin/bridgectl

USER collector
VOLUME ["/data"]
EXPOSE 9464

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
  CMD /usr/local/bin/bridgectl telemetry health --target 127.0.0.1:9464 --insecure || exit 1

ENTRYPOINT ["/usr/local/bin/bridgectl", "telemetry", "collect"]
CMD ["--listen", "0.0.0.0:9464", "--spool-dir", "/data/segments", "--max-segment-bytes", "10485760", "--max-disk-space", "1GB"]
