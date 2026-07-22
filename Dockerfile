# Reference local/CI image. Both stages are pinned to official multi-platform
# manifest digests; release publication additionally signs the image and emits
# the SBOM/provenance receipts described by the release policy.
FROM golang:1.26.5-alpine3.24@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
WORKDIR /src
COPY go.mod ./
COPY VERSION ./
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg
RUN version="$(tr -d '\n' < VERSION)" \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${version}" -o /out/rkc ./cmd/rkc \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${version}" -o /out/rkc-mcp ./cmd/rkc-mcp

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
LABEL org.opencontainers.image.source="https://github.com/neuroforge-io/RKC" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.title="Repository Knowledge Compiler" \
      io.neuroforge.rkc.python-ast="disabled-requires-user-systemd"
RUN addgroup -S rkc && adduser -S -G rkc -u 65532 rkc \
 && mkdir -p /output /state /usr/share/licenses/rkc \
 && chown -R rkc:rkc /output /state
COPY --from=build /out/rkc /usr/local/bin/rkc
COPY --from=build /out/rkc-mcp /usr/local/bin/rkc-mcp
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /usr/share/licenses/rkc/
COPY LICENSES /usr/share/licenses/rkc/LICENSES
COPY plugins /opt/rkc/plugins
COPY schemas /opt/rkc/schemas
COPY api /opt/rkc/api
COPY config /opt/rkc/config
ENV RKC_PLUGIN_ROOT=/opt/rkc/plugins
USER rkc
WORKDIR /workspace
ENTRYPOINT ["rkc"]
CMD ["help"]
