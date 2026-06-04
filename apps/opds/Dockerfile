# Build from the repo root: docker build -t opds -f apps/opds/Dockerfile .
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/opds ./apps/opds

FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/opds /opds
WORKDIR /data
EXPOSE 8797
ENV HOST=0.0.0.0 OPDS_PORT=8797
ENTRYPOINT ["/opds"]
