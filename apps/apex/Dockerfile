# Build from the repo root: docker build -t apex -f apps/apex/Dockerfile .
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/apex ./apps/apex

FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/apex /apex
EXPOSE 8790
ENV HOST=0.0.0.0 APEX_PORT=8790
ENTRYPOINT ["/apex"]
