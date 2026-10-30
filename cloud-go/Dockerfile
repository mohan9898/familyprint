# 云端 Go 版：scratch + 单二进制，镜像 ~17MB，运行内存 ~15MB
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
COPY static ./static
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /fp-cloud .

FROM scratch
COPY --from=build /fp-cloud /fp-cloud
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
WORKDIR /app
ENTRYPOINT ["/fp-cloud"]
