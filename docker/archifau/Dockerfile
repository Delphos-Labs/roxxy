# archifau — origin-server build of roxxy (docker registry + PyPI + file
# store behind GitHub OIDC auth). Build from the repository root:
#
#   docker build -f docker/archifau/Dockerfile -t archifau .
#
# or use cmd/archifau/docker-compose.yaml. Runtime configuration is via
# environment (see cmd/archifau/main.go); mount the data dir at /data and the
# TLS cert/key wherever ARCHIFAU_TLS_CERT/ARCHIFAU_TLS_KEY point.
#
# Environments behind TLS-intercepting proxies (roxxy itself, corporate
# MITM): drop the intercepting CA as a *.crt file into
# docker/archifau/ca-certs/ before building — both stages trust it.

FROM golang:1.25-alpine AS build
COPY docker/archifau/ca-certs/ /usr/local/share/ca-certificates/
RUN apk add --no-cache ca-certificates && update-ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN cp "$(go env GOROOT)/LICENSE" /src/GO_LICENSE
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /archifau ./cmd/archifau

FROM alpine:3.21
# ca-certificates: archifau dials GitHub (JWKS + api.github.com) over TLS.
COPY docker/archifau/ca-certs/ /usr/local/share/ca-certificates/
RUN apk add --no-cache ca-certificates && update-ca-certificates \
    && adduser -D -u 65532 archifau \
    && mkdir -p /data && chown archifau:archifau /data
USER archifau
COPY --from=build /archifau /usr/local/bin/archifau
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/share/licenses/archifau/
COPY --from=build /src/GO_LICENSE /usr/share/licenses/archifau/GO_LICENSE
ENV ARCHIFAU_DATA_DIR=/data
VOLUME /data
EXPOSE 8443
# Probe HTTPS first, then HTTP for ARCHIFAU_INSECURE_HTTP=1 test setups.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q --no-check-certificate -O /dev/null https://127.0.0.1:8443/healthz \
   || wget -q -O /dev/null http://127.0.0.1:8443/healthz
ENTRYPOINT ["/usr/local/bin/archifau"]
