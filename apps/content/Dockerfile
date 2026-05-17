# Build from the repo root: docker build -t content -f apps/content/Dockerfile .
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/content ./apps/content

FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/content /content
WORKDIR /data
EXPOSE 8787
ENV HOST=0.0.0.0 CONTENT_PORT=8787
ENTRYPOINT ["/content"]
