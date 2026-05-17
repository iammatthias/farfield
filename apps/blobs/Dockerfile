# Build from the repo root: docker build -t blobs -f apps/blobs/Dockerfile .
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/blobs ./apps/blobs

FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/blobs /blobs
WORKDIR /data
EXPOSE 8789
ENV HOST=0.0.0.0 BLOBS_PORT=8789
ENTRYPOINT ["/blobs"]
