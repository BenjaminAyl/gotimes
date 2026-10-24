# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# Pure-Go build (no CGO; sqlite is modernc.org/sqlite) so the binary runs on a minimal image.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/rss ./cmd/rss
# distroless has no shell, so create the data dir here; a new named volume inherits its ownership.
RUN mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/rss /app/rss
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV ADDR=:4444 DATA_DIR=/data
EXPOSE 4444
VOLUME /data
USER nonroot:nonroot
ENTRYPOINT ["/app/rss"]
