# Build from the repo root: docker build -t library -f apps/library/Dockerfile .
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/library ./apps/library

FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/library /library
WORKDIR /data
EXPOSE 8797
ENV HOST=0.0.0.0 LIBRARY_PORT=8797
ENTRYPOINT ["/library"]
