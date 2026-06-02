# Build from the repo root: docker build -t bard -f apps/bard/Dockerfile .
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/bard ./apps/bard

FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/bard /bard
EXPOSE 8795
ENV HOST=0.0.0.0 BARD_PORT=8795
ENTRYPOINT ["/bard"]
