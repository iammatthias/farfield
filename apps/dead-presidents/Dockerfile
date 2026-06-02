# Build from the repo root: docker build -t dead-presidents -f apps/dead-presidents/Dockerfile .
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/dead-presidents ./apps/dead-presidents

FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/dead-presidents /dead-presidents
EXPOSE 8796
ENV HOST=0.0.0.0 DEAD_PRESIDENTS_PORT=8796
ENTRYPOINT ["/dead-presidents"]
