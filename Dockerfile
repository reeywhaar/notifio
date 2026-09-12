# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# COPY path by path rather than a .dockerignore: an allowlist cannot admit a local config.json
# or data/ holding real credentials, and a new top-level directory fails the build loudly.
COPY main.go ./
COPY internal ./internal
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X notifio/internal/app.Version=$VERSION" -o /out/notifio .

FROM alpine:3.22
# Load-bearing both ways: without it every Telegram call fails verification and every STARTTLS
# upgrade fails too.
RUN apk add --no-cache ca-certificates
COPY --from=build /out/notifio /usr/local/bin/notifio
ENV NOTIFIO_DATA_DIR=/data NOTIFIO_CONFIG=/data/config.json

# No `VOLUME /data` and no mkdir, deliberately. Declaring it would make Docker create an
# anonymous volume, /data would always exist, and the check for a mounted one could never fire
# — leaving the credentials somewhere that disappears with the container. See docs/deploy.md.

EXPOSE 80
HEALTHCHECK --interval=30s --timeout=5s --start-period=3s CMD ["notifio", "healthcheck"]
ENTRYPOINT ["notifio"]
CMD ["serve"]
