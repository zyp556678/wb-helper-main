# syntax=docker/dockerfile:1
#
# 三阶段构建：前端产物 → Go 二进制 → 最小运行时。
# 最终镜像只含一个静态二进制，前端资源经 go:embed 已打进它里面。
#
# 基础镜像做成可覆盖的 ARG：Go 的小版本标签有时会滞后于官方发布，
# 遇到 `manifest unknown` 时不用改文件，直接换一个即可：
#   docker build --build-arg GO_IMAGE=golang:alpine -t wb-gateway .
ARG NODE_IMAGE=node:22-alpine
ARG GO_IMAGE=golang:1.27-alpine
ARG RUNTIME_IMAGE=alpine:3.20
#
# 注意：本镜像**不含本机代理**（wb-local-agent）。本机代理要读写宿主机上的客户端
# 配置与会话库，天然属于「本机形态」，把它放进容器既没有宿主文件系统可读，
# 也违背了它「只绑回环、由网关代管」的设计。服务端部署不需要它：
# 网关找不到该二进制时会静默跳过，面板上的「本机客户端」入口自动隐藏。

# -----------------------------------------------------------------------------
# 阶段 1：构建前端
# -----------------------------------------------------------------------------
FROM ${NODE_IMAGE} AS web
WORKDIR /src/web

# 先只拷贝依赖清单：依赖没变时这一层可命中缓存，改业务代码不会重装 node_modules
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY web/ ./
# 产物输出到 /src/web/dist，base 为相对路径，供 go:embed 与任意前缀托管复用
RUN npm run build


# -----------------------------------------------------------------------------
# 阶段 2：编译 Go 二进制
# -----------------------------------------------------------------------------
FROM ${GO_IMAGE} AS build
WORKDIR /src

# 本项目的 go.mod 没有任何外部依赖（全部标准库），因此不需要 go mod download，
# 也就不需要为私有模块配置代理。
COPY go.mod ./
COPY main.go ./
COPY internal/ ./internal/
COPY cmd/ ./cmd/
COPY web/embed.go ./web/embed.go
# go:embed 要求编译时 dist 已存在，从阶段 1 取
COPY --from=web /src/web/dist ./web/dist

# CGO_ENABLED=0：产出静态二进制，运行时镜像无需 glibc
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/workbuddy-gateway .


# -----------------------------------------------------------------------------
# 阶段 3：运行时
# -----------------------------------------------------------------------------
FROM ${RUNTIME_IMAGE}

# ca-certificates 必需（要访问上游 HTTPS）；tzdata 必需（定时任务按本地整点排程）
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 -h /app wbg

WORKDIR /app

COPY --from=build /out/workbuddy-gateway /app/workbuddy-gateway
COPY config.example.json /app/config.example.json

# 数据目录：凭据、config.json、模型缓存、统计都会落在这里，必须挂卷，
# 否则容器一重建账号池就空了。
RUN mkdir -p /app/data && chown -R wbg:wbg /app
VOLUME ["/app/data"]

USER wbg
EXPOSE 8317

ENV TZ=Asia/Shanghai

# /healthz 在鉴权中间件里被显式豁免（见 internal/server/server.go 的 auth），
# 所以设了 API Key 时健康检查依然能过。
# start-period 给足 40 秒：启动时会先同步拉一次模型目录再开始监听端口，
# 这段时间内请求会被连接拒绝，属于预期行为而不是故障。
HEALTHCHECK --interval=30s --timeout=5s --start-period=40s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8317/healthz >/dev/null 2>&1 || exit 1

# --addr 0.0.0.0 是容器内必须的（否则端口映射打不进来）。
# 对外暴露请用 `-p 127.0.0.1:8317:8317` 只绑宿主回环，或设置 -api-key，
# 二选一；两样都不做等于把面板裸奔在公网上。
ENTRYPOINT ["/app/workbuddy-gateway"]
CMD ["serve", "--addr", "0.0.0.0", "--port", "8317"]
