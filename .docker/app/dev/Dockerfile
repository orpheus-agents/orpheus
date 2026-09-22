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
EXPOSE 8000
USER ${ORPHEUS_UID}:${ORPHEUS_GID}
CMD ["go", "run", "./cmd/orpheus", "serve"]
