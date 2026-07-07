# syntax=docker/dockerfile:1
#
# gateway-api — the HTTP control plane (:8080). Multi-stage: a pinned Go
# toolchain builds a static, CGO-free binary; the final image is minimal and
# non-root. Migrations are embedded in the binary (//go:embed), so no SQL
# files are copied into the runtime image.

# --- build stage ---------------------------------------------------------
FROM golang:1.25.11 AS build
WORKDIR /src

# Download modules in their own layer so source edits don't re-fetch deps.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO off → a fully static binary that runs on distroless/static.
# -trimpath keeps build paths out of the binary; -s -w drop debug info.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/gateway-api ./cmd/gateway-api

# --- final stage ---------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway-api /usr/local/bin/gateway-api
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/gateway-api"]
