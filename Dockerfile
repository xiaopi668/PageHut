# syntax=docker/dockerfile:1
# PageHut 多阶段构建：产物为静态二进制，运行镜像极小。
# 用 BUILDPLATFORM 让编译在本机构架下完成，再按 TARGETARCH 交叉编译，
# 避免 arm64 镜像在 amd64 构建机上走全量 QEMU 模拟。
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/pagehut .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 1000 pagehut \
    && mkdir -p /data && chown pagehut:pagehut /data
COPY --from=build /out/pagehut /usr/local/bin/pagehut
USER pagehut
WORKDIR /data
VOLUME /data
EXPOSE 8080 80 443
# alpine 自带 wget（busybox），无需额外安装 curl
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["pagehut"]
CMD ["-data", "/data", "-http", ":8080"]
