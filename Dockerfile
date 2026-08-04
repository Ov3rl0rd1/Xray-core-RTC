# syntax=docker/dockerfile:1
#
# Root Dockerfile for manual/local builds of this Xray fork (incl. the olcrtc
# proxy). `docker build .` produces a small distroless image.
#
# The CI image lives at .github/docker/Dockerfile and is pushed automatically by
# .github/workflows/docker.yml on release; this file is the convenient
# hand-build equivalent. Multi-arch aware (works with `docker buildx`).

# ---- build stage ------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26 AS build

WORKDIR /src

# Download modules first so this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Cross-compile targets provided by buildx (default to the build host otherwise).
ARG TARGETOS
ARG TARGETARCH
# Stamp shown by `xray version`; pass --build-arg XRAY_BUILD=$(git rev-parse HEAD).
ARG XRAY_BUILD=docker

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /out/xray -trimpath -buildvcs=false \
      -ldflags="-X github.com/xtls/xray-core/core.build=${XRAY_BUILD} -s -w -buildid=" \
      ./main

# Routing data for geoip:/geosite: rules.
ADD https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/geoip.dat   /out/geoip.dat
ADD https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/geosite.dat /out/geosite.dat

# Placeholder config so the container starts even before you mount your own.
RUN printf '{}\n' > /out/config.json

# ---- runtime stage ----------------------------------------------------------
FROM gcr.io/distroless/static:nonroot

COPY --from=build --chmod=755 /out/xray                 /usr/local/bin/xray
COPY --from=build --chmod=644 /out/geoip.dat /out/geosite.dat /usr/local/share/xray/
COPY --from=build --chmod=644 /out/config.json          /etc/xray/config.json

# Tell xray where the .dat assets live.
ENV XRAY_LOCATION_ASSET=/usr/local/share/xray

# Mount your real config here (a file or a -confdir directory).
VOLUME /etc/xray

# Common inbound / gRPC-API ports (documentation only; publish with -p).
EXPOSE 443 10085

ENTRYPOINT ["/usr/local/bin/xray"]
CMD ["run", "-c", "/etc/xray/config.json"]
