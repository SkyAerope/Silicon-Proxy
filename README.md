# Silicon-Proxy

高性能、自动化的 API 代理工具，通过多源 SOCKS5 代理池实现高可用请求转发，规避后端风控。

## 核心特性

- **多源代理抓取**：支持从远程URL或本地文件自动定期拉取 SOCKS5 代理列表。
- **并发代理测活**：支持大规模并发检测代理存活性，自动维护代理池并剔除失效代理。
- **死代理冷却机制**：被剔除的代理会存在一个集合里，默认24h后再删除，抓取代理时过滤掉。
- **key与代理绑定**：在 `Authorization` Header 里的 API Key 会绑定到固定代理。
- **自动故障转移 & 重试**：当代理请求失败时，自动在毫秒级切换到另一个可用代理并重试，默认最高 3 次（可调）。
- **Redis**：存储代理状态及 Key 与代理绑定关系，支持容器重启及水平扩展。
- **Docker 化部署**：提供完整的 Dockerfile 与 docker-compose 方案，开箱即用。

## 配置说明 (`config.jsonc`)

| 参数 | 说明 | 默认值 |
| :--- | :--- | :--- |
| `listen` | 代理监听端口 | `:8080` |
| `backend` | 默认转发的目标后端域名 | `https://api.siliconflow.cn/` |
| `max_auth_per_proxy` | 单个代理最大挂载的 Auth Key 数量 | `1` |
| `max_request_retries` | 失败后自动切换代理的重试次数 | `3` |
| `request_timeout` | 连接预算：从请求开始到成功连上后端（含重试、拨号、TLS 握手）的最大耗时；连上后不再用它中断流式响应 | `10s` |
| `health_check.dead_proxy_ttl` | 被剔除的代理冷却期（到期后允许再次加入） | `24h` |
| `sources` | 代理获取源配置（支持 URL/Local） | - |
| `health_check` | 探活频率、超时及并发数 | - |
| `pool` | HTTP 连接池配置 | - |

## 快速开始

### 1. 使用 Docker Compose (推荐)

```bash
# 生成配置文件
cp config.example.jsonc config.jsonc
# 根据需要修改其中的代理源或监听端口
# 启动项目
docker compose up -d
```

### 2. 本地直接运行

确保本地已安装 Go 1.24+ 及 Redis 服务。

```bash
# 编译
go build -o silicon-proxy ./cmd/silicon-proxy
# 运行
./silicon-proxy --config config.jsonc
```

## 使用方式

只需将原本访问第三方服务的 URL 修改为代理地址即可，以 OpenAI 兼容接口为例：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-xxxxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-ai/DeepSeek-V3.2","messages":[{"role":"user","content":"Hi"}]}'
```

## 项目架构

- `cmd/silicon-proxy`: 程序的启动入口与信号处理。
- `internal/config`: 解析 JSONC 格式并处理配置热加载。
- `internal/health`: 代理池探活器，支持自定义探活目标。
- `internal/pool`: 核心路由逻辑，管理 Transport 缓存及基于负载的代理选取。
- `internal/proxy`: 处理 HTTP 转发、Header 解析、Body 缓存及重试机制。
- `internal/source`: 抽象化的代理数据源（URL、Local 文件）。
- `internal/store`: Redis 数据存取层，负责分布式锁（TODO）及负载统计。

## 注意
- 请在调整配置文件时充分考虑：代理测活是否会对后端造成cc攻击。
