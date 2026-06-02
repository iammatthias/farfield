# Build from the repo root: docker build -t ba2d -f apps/ba2d/Dockerfile .
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/ba2d ./apps/ba2d

FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/ba2d /ba2d
EXPOSE 8795
ENV HOST=0.0.0.0 BA2D_PORT=8795
ENTRYPOINT ["/ba2d"]
