# One Go binary, two images:
#   --target pg   Postgres 16 + spillway siteagent (one per site)
#   (default)     spillway app/edge/control/dbrouter/probe
FROM golang:1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/spillway ./cmd/spillway

FROM postgres:16 AS pg
COPY --from=build /out/spillway /usr/local/bin/spillway
RUN mkdir -p /var/lib/postgresql/data /var/run/postgresql \
 && chown -R postgres:postgres /var/lib/postgresql /var/run/postgresql
USER postgres
ENV PGDATA=/var/lib/postgresql/data/pgdata
EXPOSE 5432 7000
ENTRYPOINT ["/usr/local/bin/spillway", "siteagent"]

FROM alpine:3.22 AS app
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/spillway /usr/local/bin/spillway
# /data holds the control plane's audit log; a named volume mounted here
# inherits this ownership.
RUN mkdir -p /data && chown 65532:65532 /data
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/spillway"]
CMD ["app"]
