# syntax=docker/dockerfile:1
#
# mock-llm — the deterministic fake OpenAI-compatible backend. Compose runs
# two instances of this one image ("fast" and "cheap") differentiated only by
# environment. Same multi-stage recipe as the other binaries.

# --- build stage ---------------------------------------------------------
FROM golang:1.25.11 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/mock-llm ./cmd/mock-llm

# --- final stage ---------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mock-llm /usr/local/bin/mock-llm
EXPOSE 8081
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/mock-llm"]
