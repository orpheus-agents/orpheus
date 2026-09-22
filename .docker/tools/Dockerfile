FROM hadolint/hadolint:v2.15.1 AS hadolint
FROM aquasec/trivy:0.74.0 AS trivy
FROM ghcr.io/astral-sh/ruff:0.16.8 AS ruff
FROM node:24.14.0-bookworm-slim AS node
RUN npm install --global @redocly/cli@2.54.0
FROM python:3.10.19-slim-bookworm AS python
FROM sqlc/sqlc:1.31.1 AS sqlc
FROM golang:1.27.1-bookworm
COPY --from=sqlc /workspace/sqlc /usr/local/bin/sqlc
COPY --from=hadolint /bin/hadolint /usr/local/bin/hadolint
COPY --from=trivy /usr/local/bin/trivy /usr/local/bin/trivy
COPY --from=ruff /ruff /usr/local/bin/ruff
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
COPY --from=python /usr/local /usr/local
RUN ln -s /usr/local/lib/node_modules/@redocly/cli/bin/cli.js /usr/local/bin/redocly
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 && \
    go install golang.org/x/vuln/cmd/govulncheck@v1.8.0 && \
    go install golang.org/x/tools/cmd/goimports@v0.48.0
WORKDIR /app
ARG ORPHEUS_UID=1000
ARG ORPHEUS_GID=1000
RUN (getent group "${ORPHEUS_GID}" || groupadd --gid "${ORPHEUS_GID}" orpheus) && \
    useradd --create-home --uid "${ORPHEUS_UID}" --gid "${ORPHEUS_GID}" orpheus && \
    mkdir -p /go/pkg/mod /home/orpheus/.cache/go-build && chown -R "${ORPHEUS_UID}:${ORPHEUS_GID}" /go /home/orpheus
ENV GOCACHE=/home/orpheus/.cache/go-build
ENV RUFF_CACHE_DIR=/home/orpheus/.cache/ruff
ENV PYTHONDONTWRITEBYTECODE=1
USER ${ORPHEUS_UID}:${ORPHEUS_GID}
CMD ["go", "test", "./..."]
