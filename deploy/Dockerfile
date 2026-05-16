# Farfield — multi-stage build. CGO is off (modernc.org/sqlite is pure Go),
# so the binaries are fully static and run on a minimal distroless base.
#
# Build from the repo root:  docker build -f deploy/Dockerfile -t farfield .

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/content ./apps/content && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/feed    ./apps/feed && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/blobs   ./apps/blobs && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/apex    ./apps/apex && \
    mkdir /out/data

# All four static binaries on one tiny image; docker-compose picks the
# command per service.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/content /out/feed /out/blobs /out/apex /
COPY schemas /schemas
# /data is the volume mountpoint for the SQLite stores. Ship it in the image
# owned by nonroot: a fresh named volume inherits the mountpoint's ownership,
# and distroless has no shell to chown at runtime.
COPY --from=build --chown=nonroot:nonroot /out/data /data
EXPOSE 8787 8788 8789 8790
CMD ["/content"]
