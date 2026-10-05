# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/logrift ./cmd/server
RUN mkdir -p /out/data && chown 65532:65532 /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/logrift /usr/local/bin/logrift
COPY --from=build --chown=nonroot:nonroot /out/data /data
# Container default: bind all interfaces (the file default 127.0.0.1 is not
# reachable from outside the container) and keep data on the /data volume.
ENV LOGRIFT_ADDR=0.0.0.0:8787 \
    LOGRIFT_DATA_DIR=/data
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8787
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/logrift"]
