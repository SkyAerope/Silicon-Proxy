# Silicon-Proxy

高性能、自动化的 API 代理工具，专为 AI 后端服务（如 SiliconFlow）设计，通过多源 SOCKS5 代理池实现高可用请求转发。

## 核心特性

- **多源代理抓取**：支持从远程 URL（如 GitHub 地址）或本地文件自动定期同步 SOCKS5 代理列表。
- **并发健康检查**：支持大规模并发检测代理存活性，自动维护存活池并剔除失效节点。
- **智能 Sticky Routing**：基于 `Authorization` Header 将特定 API Key 绑定到固定代理，确保对话会话的连贯性。
- **自动故障转移 & 重试**：当下层代理请求失败时，自动在毫秒级切换到另一个可用代理并重试，最高 3 次（可调）。
- **Redis 持久化存储**：使用 Redis 存储代理状态、健康信息及 Key 绑定关系，支持容器重启及水平扩展。
- **JSONC 配置格式**：支持带注释的 JSON 配置文件，配置层级清晰。
- **Docker 化部署**：提供完整的 Dockerfile 与 docker-compose 方案，开箱即用。

## 配置说明 (`config.jsonc`)

| 参数 | 说明 | 默认值 |
| :--- | :--- | :--- |
| `listen` | 代理监听端口 | `:8080` |
| `backend` | 默认转发的目标后端域名 | `https://api.siliconflow.cn/` |
| `max_auth_per_proxy` | 单个代理最大挂载的 Auth Key 数量 | `1` |
| `max_request_retries` | 失败后自动切换代理的重试次数 | `3` |
| `request_timeout` | 单次请求总超时时长 | `10s` |
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

确保本地已安装 Go 1.22+ 及 Redis 服务。

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
  -d '{"model":"deepseek-ai/DeepSeek-V3","messages":[{"role":"user","content":"Hi"}]}'
```

## 项目架构

- `cmd/silicon-proxy`: 程序的启动入口与信号处理。
- `internal/config`: 解析 JSONC 格式并处理配置热加载。
- `internal/health`: 代理池探活器，支持自定义探活目标。
- `internal/pool`: 核心路由逻辑，管理 Transport 缓存及基于负载的代理选取。
- `internal/proxy`: 处理 HTTP 转发、Header 解析、Body 缓存及重试机制。
- `internal/source`: 抽象化的代理数据源（URL、Local 文件）。
- `internal/store`: Redis 数据存取层，负责分布式锁（TODO）及负载统计。

## 已知问题
- 对非流式支持不佳。因为无法判断是代理无响应还是AI正在回答。
