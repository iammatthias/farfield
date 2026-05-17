# Build from the repo root: docker build -t backup -f apps/backup/Dockerfile .
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/backup ./apps/backup

FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/backup /backup
WORKDIR /data
EXPOSE 8791
ENV HOST=0.0.0.0 BACKUP_PORT=8791
ENTRYPOINT ["/backup"]
