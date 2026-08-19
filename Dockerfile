# glm-cc-gateway 容器镜像(零第三方依赖,静态编译)。
# 运行时只需挂载 /app/data:配置(首启自动生成)、伪装身份、会话池、用量全在其中。
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/glm-cc-gateway .

FROM alpine:3.20
WORKDIR /app
COPY --from=build /out/glm-cc-gateway /usr/local/bin/glm-cc-gateway
RUN mkdir -p /app/data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["glm-cc-gateway"]
# 默认读取 /app/data/config.json(不存在时自动生成默认配置);
# 自定义路径:docker run ... glm-cc-gateway -config /path/to/config.json
