# Build from the repo root: docker build -t qr -f apps/qr/Dockerfile .
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/qr ./apps/qr

FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/qr /qr
WORKDIR /data
EXPOSE 8794
ENV HOST=0.0.0.0 QR_PORT=8794
ENTRYPOINT ["/qr"]
