FROM golang:1.27.1-bookworm AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /orpheus ./cmd/orpheus
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p /opt/orpheus && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /opt/orpheus/hook-runner-amd64 ./cmd/orpheus-hook-runner && \
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /opt/orpheus/hook-runner-arm64 ./cmd/orpheus-hook-runner
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /orpheus /orpheus
COPY --from=build /opt/orpheus /opt/orpheus
COPY --from=build /app/migrations ./migrations
USER 65532:65532
EXPOSE 8000
HEALTHCHECK --interval=10s --timeout=5s --start-period=10s CMD ["/orpheus", "healthcheck"]
ENTRYPOINT ["/orpheus"]
CMD ["serve"]
