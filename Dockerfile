# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X example.com/oci-pull-proxy/internal/app.Version=${VERSION}" -o /out/oci-pull-proxy ./cmd/oci-pull-proxy
RUN mkdir -p /state/cache /state/acme && chown -R 65532:65532 /state && chmod 700 /state/cache /state/acme

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/hi2code/oci-pull-proxy" \
      org.opencontainers.image.description="OCI pull proxy with automatic HTTPS, authentication, and disk caching" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/oci-pull-proxy /oci-pull-proxy
COPY --from=build --chown=65532:65532 /state/ /data/
USER 65532:65532
EXPOSE 8080 8443
ENTRYPOINT ["/oci-pull-proxy"]
