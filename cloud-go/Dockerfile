# 云端 Go 版：scratch + 单二进制，镜像 ~17MB，运行内存 ~15MB
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# 拷贝全部 .go 源文件（新增源文件无需再改这里，避免漏拷导致 undefined 编译错误）
COPY *.go ./
COPY static ./static
# 版本号由 CI 通过 --build-arg 传入，让容器内启动横幅显示真实版本而不是 dev
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.buildVersion=${VERSION}" -o /fp-cloud .

FROM scratch
COPY --from=build /fp-cloud /fp-cloud
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
WORKDIR /app
ENTRYPOINT ["/fp-cloud"]
