# syntax=docker/dockerfile:1.7
# Single image for every role: `exchange serve --role=<role>` (docs/plan-v1.0.md §9, §11).

FROM golang:1.26.8-bookworm AS build
WORKDIR /src
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
# Dependency layer first so it is cached independently of source changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o /out/ ./cmd/exchange ./cmd/exchangectl

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/exchange /out/exchangectl /
USER nonroot:nonroot
EXPOSE 8080 8081 8082 9100
# distroless has no shell: exec form only, and the binary must live at /exchange.
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=6 \
  CMD ["/exchange", "healthcheck", "--url", "http://127.0.0.1:9100/readyz"]
ENTRYPOINT ["/exchange"]
CMD ["serve", "--role=all"]
