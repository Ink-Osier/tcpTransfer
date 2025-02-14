# 第一阶段：构建
FROM golang:1.21-alpine AS builder

# 设置工作目录
WORKDIR /app

# 复制源代码
COPY main.go .

# 构建可执行文件
RUN go build -o proxy main.go

# 第二阶段：运行
FROM alpine:latest

WORKDIR /app

# 从构建阶段复制可执行文件
COPY --from=builder /app/proxy .

# 暴露默认端口
EXPOSE 8080

# 设置入口点
ENTRYPOINT ["./proxy"]