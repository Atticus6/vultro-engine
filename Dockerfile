# 构建期一次性完成编译，运行期只有二进制（distroless：无 shell、无包管理器）。
#
# 用 distroless 而不是 alpine 的理由：引擎只做 HTTP 客户端/服务端与内存处理，
# 不需要任何运行时工具；镜像越小、攻击面越小。排查用 host 侧的网络工具，
# 而不是进容器（distroless 没有 shell，`docker exec` 也进不去）。
FROM golang:1.27-alpine AS build

ARG VERSION=dev
WORKDIR /src

# 先只拷依赖清单：源码改动不会让 `go mod download` 的层缓存失效。
COPY go.mod go.sum ./
RUN go mod download

# 根目录的 **所有** 非测试 .go 文件都要拷：main.go 依赖同包的 args.go
# （parseArgs / usage / printConfig），漏一个就是 `undefined: parseArgs`。
COPY main.go args.go ./
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X github.com/atticus6/vultro-engine/internal/buildinfo.Version=${VERSION}" \
    -o /out/vultro-engine .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/vultro-engine /usr/local/bin/vultro-engine
EXPOSE 8787
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/vultro-engine"]
