# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# CGO stays off: the SQLite driver is modernc.org/sqlite, a pure-Go
# translation, so the runtime image needs no libc. The zone database is
# compiled into the binary (see time/tzdata in main.go), which is what lets the
# runtime image carry nothing at all.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /quicksurvey ./cmd/quicksurvey
# An empty directory to seed /data with, so a fresh named volume inherits its
# ownership from the image rather than being created root-owned.
RUN mkdir -p /empty

# Distroless: no shell, no package manager, no libc. The only executable in the
# image is quicksurvey itself, which is also what answers the health check.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /quicksurvey /usr/local/bin/quicksurvey
COPY --from=build --chown=65532:65532 /empty /data
USER 65532:65532
VOLUME /data
EXPOSE 8080
ENV QS_DATA_DIR=/data QS_ADDR=:8080
# The container filesystem can be mounted read-only: /data is the only path
# written, and temporary files are created inside it rather than in /tmp.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD ["quicksurvey", "healthcheck"]
ENTRYPOINT ["quicksurvey"]
CMD ["serve"]
