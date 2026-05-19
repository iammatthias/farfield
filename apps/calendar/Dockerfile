# Build from the repo root: docker build -t calendar -f apps/calendar/Dockerfile .
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/calendar ./apps/calendar

FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/calendar /calendar
WORKDIR /data
EXPOSE 8792
ENV HOST=0.0.0.0 CALENDAR_PORT=8792
ENTRYPOINT ["/calendar"]
