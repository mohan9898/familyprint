# Go Agent 极简镜像：scratch + 单二进制，镜像 ~8MB，运行内存 ~10MB
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# 拷贝全部 .go 源文件（便于后续拆分源文件时不会漏拷）
COPY *.go ./
# 版本号由 CI 通过 --build-arg 传入，让容器内启动横幅显示真实版本而不是 dev
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.buildVersion=${VERSION}" -o /fp-agent .

FROM scratch
COPY --from=build /fp-agent /fp-agent
# 时区与 CA 证书（连 wss:// 需要）
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENTRYPOINT ["/fp-agent"]
