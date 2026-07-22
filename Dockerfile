# Reference local/CI image. The disposable builder is pinned to an official
# multi-platform manifest; the shipped runtime is a static, package-free
# scratch image. Container signing, a container SBOM, and provenance remain
# release gates; this reference build does not claim those attestations.
FROM golang:1.26.5-alpine3.24@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download \
 && go mod verify
COPY VERSION ./
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg
COPY storage ./storage
RUN version="$(tr -d '\n' < VERSION)" \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${version}" -o /out/rkc ./cmd/rkc \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${version}" -o /out/rkc-mcp ./cmd/rkc-mcp \
 && mkdir -p /rootfs/output /rootfs/state /rootfs/tmp /rootfs/workspace \
 && chmod 1777 /rootfs/tmp \
 && chown -R 65532:65532 /rootfs/output /rootfs/state /rootfs/tmp /rootfs/workspace

FROM scratch
LABEL org.opencontainers.image.source="https://github.com/neuroforge-io/RKC" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.title="Repository Knowledge Compiler" \
      io.neuroforge.rkc.python-ast="disabled-requires-user-systemd"
COPY --from=build --chown=65532:65532 /rootfs/ /
COPY --from=build /out/rkc /usr/local/bin/rkc
COPY --from=build /out/rkc-mcp /usr/local/bin/rkc-mcp
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /usr/share/licenses/rkc/
COPY LICENSES /usr/share/licenses/rkc/LICENSES
COPY third_party/go-modules.lock.json /usr/share/licenses/rkc/third_party/go-modules.lock.json
COPY plugins /opt/rkc/plugins
COPY schemas /opt/rkc/schemas
COPY api /opt/rkc/api
COPY config /opt/rkc/config
ENV RKC_PLUGIN_ROOT=/opt/rkc/plugins
USER 65532:65532
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/rkc"]
CMD ["help"]
