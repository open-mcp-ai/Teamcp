# 单二进制镜像。整个部署就是一个文件 + 一份 actors.json。
#
# CGO 关掉是刻意的：产出静态二进制，才能塞进 scratch，也才能在任意架构上交叉编译。
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/teamcp ./cmd/teamcp

FROM scratch
# scratch 里没有 /etc/passwd，用数字 UID 声明非 root 运行。
COPY --from=build /out/teamcp /teamcp
USER 65532:65532
# 只监听回环：TLS 与认证都在前面的反向代理上（ADR-012）。
EXPOSE 8080
ENTRYPOINT ["/teamcp"]
CMD ["-listen", "0.0.0.0:8080", "-data-dir", "/data"]
