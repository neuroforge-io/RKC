# Reference local/CI image. Release binaries are built with -trimpath and no CGO.
# Production publication should additionally pin base-image digests, sign the
# image, attach an SBOM, and publish provenance as described in docs/OPERATIONS.md.
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=0.3.0-reference" -o /out/rkc ./cmd/rkc \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/rkc-mcp ./cmd/rkc-mcp

FROM python:3.11-alpine
RUN addgroup -S rkc && adduser -S -G rkc -u 65532 rkc
COPY --from=build /out/rkc /usr/local/bin/rkc
COPY --from=build /out/rkc-mcp /usr/local/bin/rkc-mcp
COPY plugins /opt/rkc/plugins
COPY schemas /opt/rkc/schemas
COPY api /opt/rkc/api
COPY config /opt/rkc/config
ENV PYTHONUNBUFFERED=1 \
    RKC_PLUGIN_ROOT=/opt/rkc/plugins
USER rkc
WORKDIR /workspace
ENTRYPOINT ["rkc"]
CMD ["help"]
