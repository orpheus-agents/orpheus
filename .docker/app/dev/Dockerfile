FROM golang:1.27.1-bookworm
ARG ORPHEUS_UID=1000
ARG ORPHEUS_GID=1000
RUN (getent group "${ORPHEUS_GID}" || groupadd --gid "${ORPHEUS_GID}" orpheus) && \
    useradd --create-home --uid "${ORPHEUS_UID}" --gid "${ORPHEUS_GID}" orpheus && \
    mkdir -p /go/pkg/mod /home/orpheus/.cache/go-build && chown -R "${ORPHEUS_UID}:${ORPHEUS_GID}" /go /home/orpheus
ENV GOCACHE=/home/orpheus/.cache/go-build
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN mkdir -p /opt/orpheus && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /opt/orpheus/hook-runner-amd64 ./cmd/orpheus-hook-runner && \
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /opt/orpheus/hook-runner-arm64 ./cmd/orpheus-hook-runner
EXPOSE 8000
USER ${ORPHEUS_UID}:${ORPHEUS_GID}
CMD ["go", "run", "./cmd/orpheus", "serve"]
