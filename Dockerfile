# syntax=docker/dockerfile:1
# PageHut 多阶段构建：产物为静态二进制，运行镜像极小。
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/pageport .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 1000 pageport \
    && mkdir -p /data && chown pageport:pageport /data
COPY --from=build /out/pageport /usr/local/bin/pageport
USER pageport
WORKDIR /data
VOLUME /data
EXPOSE 8080 80 443
ENTRYPOINT ["pageport"]
CMD ["-data", "/data", "-http", ":8080"]
