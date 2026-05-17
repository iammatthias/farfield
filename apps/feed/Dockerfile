# Build from the repo root: docker build -t feed -f apps/feed/Dockerfile .
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/feed ./apps/feed

FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/feed /feed
WORKDIR /data
EXPOSE 8788
ENV HOST=0.0.0.0 FEED_PORT=8788
ENTRYPOINT ["/feed"]
