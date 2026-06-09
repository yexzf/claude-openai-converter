# Claude OpenAI Converter

独立的 Anthropic/Claude -> OpenAI-compatible 转换器。

它只做协议转换，不包含账号管理、计费、路由、数据库，也不修改官方 `sub2api` 镜像。

## 架构

```text
Claude Code / Claude Client
        |
        v
claude-openai-converter
        |
        v
OpenAI-compatible upstream
  - 官方 sub2api 镜像
  - Kimi 2.6 / 其他 OpenAI 兼容服务
```

## 解决什么问题

- 你可以继续单独更新官方 `sub2api` 镜像
- 转换器单独发版，不和官方镜像绑在一起
- Claude 侧仍然可以用 Anthropic 接口风格
- 上游只要是 OpenAI-compatible 就能接

## 支持范围

- `POST /v1/messages`
- `GET /v1/models`
- `/healthz`

当前实现重点是 `messages` 流程，支持流式和非流式返回。

## 环境变量

| 变量 | 必填 | 说明 |
|---|---:|---|
| `LISTEN_ADDR` | 否 | 监听地址，默认 `:8787` |
| `UPSTREAM_BASE_URL` | 是 | 上游 OpenAI-compatible 基地址，如 `http://127.0.0.1:8080` 或 `https://api.xxx.com` |
| `UPSTREAM_API_KEY` | 否 | 上游 Bearer Key |
| `UPSTREAM_MODEL` | 否 | 默认上游模型名 |
| `MODEL_MAP_JSON` | 否 | 请求模型到上游模型的映射 JSON，例如 `{"*":"moonshotai/Kimi-K2.6"}` |

### 模型映射

如果你想把 Claude 模型统一转到 Kimi 2.6：

```bash
export MODEL_MAP_JSON='{"*":"moonshotai/Kimi-K2.6"}'
```

如果你只想指定默认上游模型，也可以只配：

```bash
export UPSTREAM_MODEL='moonshotai/Kimi-K2.6'
```

## Docker 运行

```bash
docker run -d \
  --network host \
  --name claude-openai-converter \
  -e UPSTREAM_BASE_URL=http://127.0.0.1:8080 \
  -e UPSTREAM_API_KEY=your-upstream-key \
  -e MODEL_MAP_JSON='{"*":"moonshotai/Kimi-K2.6"}' \
  yexzf/claude-openai-converter:latest
```

## Claude 侧配置

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8787/v1"
export ANTHROPIC_AUTH_TOKEN="anything"
```

## Docker Compose

见 `docker-compose.yml`。默认用 host 网络，适合 Linux 服务器。

## 更新策略

独立更新，不要混在一起：

1. 官方 `sub2api` 镜像单独升级
2. 转换器镜像单独升级
3. 只改 `.env`，不用改官方仓库代码

示例：

```bash
# 更新官方 sub2api（在它自己的部署目录里执行）
docker compose pull
docker compose up -d

# 更新转换器
docker compose pull
docker compose up -d
```

## 备注

- 这个仓库不是官方 `sub2api` 的 fork
- 如果你后面继续用官方仓库，只需要把它当上游服务
- 如果上游不支持 `POST /v1/chat/completions`，这个转换器就不适配
