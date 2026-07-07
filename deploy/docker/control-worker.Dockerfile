# syntax=docker/dockerfile:1
#
# control-worker — the batch consumer (:9100 health/metrics). Same multi-stage
# recipe as gateway-api: pinned Go toolchain, static CGO-free binary, minimal
# non-root final image.

# --- build stage ---------------------------------------------------------
FROM golang:1.25.11 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/control-worker ./cmd/control-worker

# --- final stage ---------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/control-worker /usr/local/bin/control-worker
EXPOSE 9100
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/control-worker"]
