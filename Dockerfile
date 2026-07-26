# syntax=docker/dockerfile:1
# One Dockerfile for every farfield app — compose passes APP per service:
#   docker build --build-arg APP=content -t farfield-content .
#
# The BuildKit cache mounts keep the module download and build caches across
# builds, so a deploy recompiles only what changed instead of cold-building
# modernc.org/sqlite eleven times.
FROM golang:1.25-bookworm AS build
ARG APP
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/app ./apps/${APP}

# The :nonroot variant runs as uid/gid 65532 instead of root. The apps bind
# ./data from the host, so a root container would write host files as root —
# and a compromise would own them. See docker-compose.yml for the one-time
# chown this requires on an existing deployment.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /bin/app /app
WORKDIR /data
ENV HOST=0.0.0.0
# Every app answers `/app health` by probing its own /status — that is what
# the compose healthcheck execs (distroless: no shell, no curl).
ENTRYPOINT ["/app"]
