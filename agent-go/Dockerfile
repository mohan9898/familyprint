# Go Agent 极简镜像：scratch + 单二进制，镜像 ~8MB，运行内存 ~10MB
FROM golang:1.21-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /fp-agent .

FROM scratch
COPY --from=build /fp-agent /fp-agent
# 时区与 CA 证书（连 wss:// 需要）
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENTRYPOINT ["/fp-agent"]
