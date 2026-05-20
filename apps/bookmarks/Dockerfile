# Build from the repo root: docker build -t bookmarks -f apps/bookmarks/Dockerfile .
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/bookmarks ./apps/bookmarks

FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/bookmarks /bookmarks
WORKDIR /data
EXPOSE 8793
ENV HOST=0.0.0.0 BOOKMARKS_PORT=8793
ENTRYPOINT ["/bookmarks"]
