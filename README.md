# glm-cc-gateway

把下游 Anthropic 协议请求伪装成**真实 Claude Code CLI** 发往固定上游端点的轻量网关。

- 上游 `base_url` 固定配置;API key 由下游传入(`Authorization: Bearer <key>` 或 `x-api-key`),按上游要求的方案转发
- **伪装身份按 key 隔离**:每个下游 key 在上游侧呈现为独立的一个真人 CC 用户(稳定 device_id + 会话派生),身份持久化在本地文件
- 伪装层(常量/算法/header wire casing/session 派生)移植自 [sub2api](https://github.com/Wei-Shaw/sub2api),见 `internal/mimic` 各文件头部注释的来源索引
- 零第三方依赖,标准库实现

## 快速开始

```bash
cp config.example.json config.json   # 改 upstream_base_url 等
go run .
# 下游客户端配置:
#   ANTHROPIC_BASE_URL=http://127.0.0.1:8080
#   ANTHROPIC_AUTH_TOKEN=<你的上游key>
```

验证:配置 `dump_dir`(默认关闭,调试时设为 `data/dumps`)后,每个请求落盘 3 份对照文件(下游原始 / 改写后 / 上游头),可与真实 CC 抓包做字段级 diff;`dump_retention_hours`(默认 72,0=不清理)自动清理过期 dump。注意 dump 含对话明文,仅调试时开启。

## 当前伪装能力(阶段 1)

| 层 | 状态 | 说明 |
|---|---|---|
| Header 全量重建 | ✅ | 丢弃下游全部业务头;注入 claude-cli UA、x-stainless 全套、x-app、anthropic-version、Accept、accept-encoding;wire casing 按 sub2api 抓包表还原;每请求随机 x-client-request-id;流式加 x-stainless-helper-method;重试递增 X-Stainless-Retry-Count |
| anthropic-beta | ✅ | 固定 API-key 伪装集(sub2api FullClaudeCodeMimicryBetas 去掉 oauth-2025-04-20) |
| 认证 | ✅ | 下游 key 原样转发,bearer / x-api-key 方案可配 |
| metadata.user_id | ✅ | 统一重写为按 key 持久化的伪装身份(64hex device_id + 空 account_uuid + 确定性 session_id);X-Claude-Code-Session-Id 头与 body session 同步;下游自带身份一律重写不透传 |
| 真 CC 直连分流 | ✅ | system 含 billing block 的请求只做 cc_version→UA 版本同步,不重写 system(保护其 prompt cache,对齐 sub2api isClaudeCode 跳过) |
| system 3-block 重写 | ✅ | 第三方客户端流量:billing block(fp 按内容计算)+ CC banner + sub2api 中性扩充段(ephemeral ttl=5m);原 system 降级注入 messages 开头 `[System Instructions]` user 消息 + "Understood..." assistant 对 |
| 参数归一化 | ✅ | 缺省补齐(不覆盖客户端值):tools[]、temperature=1、max_tokens=128000、thinking 时 context_management(对齐 sub2api normalizeClaudeOAuthRequestBody) |
| cache_control 限制 | ✅ | ≤4 块;thinking 块非法 cache_control 一律删;超限按 tools(后往前)→ messages(前往后)→ system 顺序删(对齐 sub2api enforceCacheControlLimit) |
| 模型映射 | ✅ | 配置 `model_map`(精确名 + `"*"` 兜底);body.model 替换 + 响应流(含 SSE)模型名还原为请求名 |
| 流式 | ✅ | SSE 事件级重组透传(保持事件边界 + 实时 flush,顺带解析 usage);含 count_tokens 端点(beta 追加 token-counting) |

## 行为收敛(阶段 3,`behavior` 配置段,各项 0=关闭)

| 能力 | 默认建议 | 说明 |
|---|---|---|
| 并发门 `max_concurrency` | 2 | 每 key 同时发往上游的请求数上限,超出排队(真人 CC 串行为主) |
| RPM `rpm_limit` | 60 | 每分钟请求上限,滑动窗口,超出排队 |
| 排队超时 `queue_timeout_seconds` | 120 | 超时返回 429 |
| 会话池 `session_pool_size` | 3 | 上游同时可见的 session_id 数;下游会话稳定映射槽位 |
| 会话轮换 `session_rotate_min/max_minutes` | 120/360 | 槽位寿命随机区间(隔几小时换新 session,像真人) |
| 日预算 `daily_token_budget` | 0(关) | 每 key 每日 input+cache+output 合计上限,超出 429;按本地日期重置 |
| 画像 `GET /stats` | — | 每 key 实时:并发/RPM/活跃会话数/今日 token 与请求数,核对"单人包线" |

状态落盘:`data/identity.json`(伪装身份)、`data/sessions.json`(会话池)、`data/usage.json`(用量)。

## 未完成(后续)

- 工具名混淆 + 响应还原(sub2api gateway_tool_rewrite 方案;下游真 CC 直连流量不需要,边际价值待定)
- ⚠ 已知偏差:真实 CLI(2.1.195/2.1.235 抓包验证)的 billing 指纹算法已与 sub2api 算法不一致,且已用 4 组真实向量证明新 fp 是 `(首条user文本, 版本)` 的**确定性函数**(相同输入→相同 fp,不含随机/时间成分);`cmd/reverse` 是逆向工具,已排除 3/2/4 字符索引、切片、session/device 拼接、多取材位置等常见结构,疑似新版更换了盐值——需分析新版 CLI 源码后再试。阶段 2 沿用 sub2api 算法保持自洽(关键信号是"缺失 billing block",fp 数值精确度影响次之)

## 配置项

见 `config.example.json`:`listen` / `upstream_base_url` / `upstream_auth_scheme`(bearer|x-api-key)/ `upstream_path` / `count_tokens_path` / `identity_file` / `dump_dir` / `api_keys_allow`(可选白名单)/ `max_body_bytes` / `max_upstream_retries`。

## 测试

```bash
go test ./...
```
