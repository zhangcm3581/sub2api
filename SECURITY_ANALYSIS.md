# sub2api 封号风险安全分析报告

> 分析日期：2026-03-25
> 分析范围：TLS 指纹模拟、HTTP 层指纹、遥测缺失、请求头伪装
> 结论：存在多个层级的检测风险，遥测缺失和 HTTP 层指纹是最关键的封号原因

---

## 目录

1. [概述](#1-概述)
2. [P0 — 遥测数据完全缺失](#2-p0--遥测数据完全缺失)
3. [P0 — Go net/http Header 大小写暴露](#3-p0--go-nethttp-header-大小写暴露)
4. [P1 — TLS 指纹已过时（OpenSSL vs BoringSSL）](#4-p1--tls-指纹已过时openssl-vs-boringssl)
5. [P1 — 静态指纹聚集](#5-p1--静态指纹聚集)
6. [P1 — HTTP Header 顺序不可控](#6-p1--http-header-顺序不可控)
7. [P2 — 版本号滞后](#7-p2--版本号滞后)
8. [附录：真实 Claude Code 网络行为](#8-附录真实-claude-code-网络行为)
9. [修复建议](#9-修复建议)

---

## 1. 概述

本项目作为 Claude Code / OpenAI / Gemini 等大模型的反向代理，面临服务商主动检测反代流量的风险。经过对代码库的深入审查以及与真实 Claude Code 客户端行为的对比分析，发现以下主要问题：

| 优先级 | 问题 | 检测难度（对服务商） | 影响 |
|--------|------|---------------------|------|
| **P0** | 遥测数据完全缺失 | 极低（二元判断） | **直接封号** |
| **P0** | Go net/http Header 大小写 | 极低（模式匹配） | **直接封号** |
| **P1** | TLS 指纹过时 | 中等（需指纹库） | 标记可疑 |
| **P1** | 静态指纹聚集 | 低（统计分析） | 批量封禁 |
| **P1** | Header 顺序随机 | 中等（序列分析） | 标记可疑 |
| **P2** | 版本号滞后 | 低 | 辅助判断 |

---

## 2. P0 — 遥测数据完全缺失

### 问题描述

真实的 Claude Code 客户端在运行时会持续向 Anthropic 发送心跳、遥测事件、日志等非 API 流量。本项目仅代理了 `/v1/messages` 等 API 端点，完全不生成或转发遥测流量。

参考：[Issue #1238](https://github.com/Wei-Shaw/sub2api/issues/1238)

### 证据

根据 eBPF 逆向分析（[来源](https://medium.com/@yunwei356/reverse-engineering-claude-codes-ssl-traffic-with-ebpf-1dde03bcc7ef)），真实 Claude Code v2.1.39 会向以下端点发送请求：

| 目标主机 | 端点 | 方法 | 用途 |
|----------|------|------|------|
| `api.anthropic.com` | `/api/hello` | GET | 心跳探活 |
| `api.anthropic.com` | `/api/event_logging/batch` | POST | 遥测事件批量上报 |
| `api.anthropic.com` | `/v1/messages` | POST | API 对话（唯一被代理的端点） |
| `http-intake.logs.us5.datadoghq.com` | `/api/v2/logs` | POST | Datadog 日志 |
| `statsig.anthropic.com` | 多个 | POST/GET | Statsig 分析 |

遥测 payload 包含：`session_id`、`device_id`、事件类型（`tengu_permission_request_option_selected`、`tengu_unary_event`）、模型信息、Growthbook 实验事件、成本阈值事件等。

### 代码现状

项目在 `backend/internal/server/routes/common.go` 中注册了一个面向下游客户端的遥测"黑洞"：

```go
// Claude Code 遥测日志（忽略，直接返回200）
r.POST("/api/event_logging/batch", func(c *gin.Context) {
    c.Status(http.StatusOK)
})
```

这仅仅是让下游客户端不报错，但遥测数据被丢弃，**从未转发给 Anthropic**。同时完全没有模拟心跳 (`/api/hello`)、Datadog 日志、Statsig 分析等请求。

### Anthropic 的检测视角

```
OAuth Token sk-ant-oat01-XXXX:
  ✅ /v1/messages 请求: 1000+ 次/天
  ❌ /api/hello 心跳: 0 次
  ❌ /api/event_logging/batch 遥测: 0 次
  ❌ Datadog 日志: 0 次
  ❌ Statsig 分析: 0 次
  → 判定：非真实 Claude Code 客户端
```

在真实场景中，30 秒的 eBPF 捕获就能记录 3,088 条事件，包括 12 个 API 请求伴随 5 次遥测批量上报、2 次 Datadog 日志和 2 次心跳。一个 token 如果只有 API 调用但零遥测，是最强的检测信号。

---

## 3. P0 — Go net/http Header 大小写暴露

### 问题描述

Go 标准库 `net/http` 在发送 HTTP 请求时，会强制将所有 Header 名转为 `Title-Case`（首字母大写）。真实 Claude Code（Node.js / Bun 运行时）默认发送**全小写** Header。

### 代码示例

```go
// backend/internal/service/gateway_service.go
req.Header.Set("authorization", "Bearer "+token)   // Go 实际发送: Authorization
req.Header.Set("content-type", "application/json")  // Go 实际发送: Content-Type
req.Header.Set("x-api-key", token)                  // Go 实际发送: X-Api-Key
```

Go 的 `http.Header.Set()` 内部调用 `textproto.CanonicalMIMEHeaderKey()`，无论传入什么大小写，输出都是 `Title-Case`。

### 对比

| Header | 本项目实际发送（Go） | 真实 Claude Code（Node.js/Bun） |
|--------|---------------------|-------------------------------|
| `authorization` | `Authorization` | `authorization` |
| `content-type` | `Content-Type` | `content-type` |
| `user-agent` | `User-Agent` | `user-agent` |
| `x-stainless-lang` | `X-Stainless-Lang` | `x-stainless-lang` |
| `anthropic-beta` | `Anthropic-Beta` | `anthropic-beta` |

### 影响

Anthropic 服务端只需检查收到的 HTTP 请求中 Header 名是否为 `Title-Case` 即可判断请求来自 Go 客户端。这是一个**零误报**的检测方法。

### 根因

项目的网关主路径使用标准 `net/http`（`backend/internal/repository/http_upstream.go`）：

```go
func buildUpstreamTransportWithTLSFingerprint(...) (*http.Transport, error) {
    transport := &http.Transport{
        // ...标准 net/http Transport
        ForceAttemptHTTP2: false,
    }
    // ...
}
```

虽然 `go.mod` 中已经间接依赖了 `bogdanfinn/fhttp`（支持自定义 Header 大小写和顺序），但**网关主路径完全没有使用它**。`fhttp` 仅作为 `imroc/req/v3` 的传递依赖存在。

---

## 4. P1 — TLS 指纹已过时（OpenSSL vs BoringSSL）

### 问题描述

项目的 TLS 指纹基于 **Node.js 20.x + OpenSSL 3.x** 的 ClientHello 抓包：

```go
// backend/internal/pkg/tlsfingerprint/dialer.go
// Default TLS fingerprint values captured from Claude CLI 2.x (Node.js 20.x + OpenSSL 3.x)
// JA3 Hash: 1a28e69016765d92e3b381168d68922c
```

但新版 Claude Code（v2.1.39+）已迁移至 **Bun 运行时 + 静态链接 BoringSSL**：

> Binary: Bun v1.3.9-canary.51+d5628db23 (Linux x64 baseline)
> SSL library: BoringSSL (statically linked, fully stripped)

### 差异

BoringSSL 和 OpenSSL 的 ClientHello 存在显著差异：
- Cipher suite 列表和顺序不同
- 支持的 TLS 扩展不同
- 支持的曲线/签名算法不同
- JA3/JA4 指纹哈希完全不同

当前代码使用 59 个 cipher suite（OpenSSL 3.x 风格），而 BoringSSL 通常只声明少量精选的 cipher suite。

### 当前 ALPN 配置

```go
// backend/internal/pkg/tlsfingerprint/dialer.go:543
&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
```

eBPF 分析确认真实 Claude Code 也使用 HTTP/1.1（`Protocol: HTTP/1.1 over TLS (not HTTP/2)`），这一点匹配。但 BoringSSL 版本的 ClientHello 整体指纹仍然不同。

---

## 5. P1 — 静态指纹聚集

### 问题描述

所有通过本反代的用户共享**完全相同**的 User-Agent 和 SDK 版本号：

```go
// backend/internal/pkg/claude/constants.go
var DefaultHeaders = map[string]string{
    "User-Agent":                  "claude-cli/2.1.22 (external, cli)",
    "X-Stainless-Lang":            "js",
    "X-Stainless-Package-Version": "0.70.0",
    "X-Stainless-OS":              "Linux",
    "X-Stainless-Arch":            "arm64",
    "X-Stainless-Runtime":         "node",
    "X-Stainless-Runtime-Version": "v24.13.0",
    "X-Stainless-Retry-Count":     "0",
    "X-Stainless-Timeout":         "600",
    "X-App":                       "cli",
}
```

### 影响

当数千个不同的 OAuth token 全部报告：
- 相同 CLI 版本 `2.1.22`
- 相同 Node.js 版本 `v24.13.0`
- 相同 SDK 版本 `0.70.0`
- 相同 OS `Linux/arm64`

这在统计上极其异常。真实世界中，不同用户有不同的操作系统（Linux/macOS/Windows）、不同的架构（x64/arm64）、不同的 Node.js 版本、不同的 CLI 版本。Anthropic 只需简单聚类分析即可识别这批异常流量。

---

## 6. P1 — HTTP Header 顺序不可控

### 问题描述

Go 的 `net/http` 在序列化 HTTP 请求时：
1. `Host` 头永远排第一
2. 其余 Header 按 Go map 迭代顺序（**伪随机**，每次运行不同）

而真实 Claude Code（Bun/Node.js）严格按照**代码中的插入顺序**发送 Header。eBPF 捕获的真实请求头顺序：

```
POST /v1/messages?beta=true HTTP/1.1
Accept: application/json
Authorization: Bearer sk-ant-oat01-...
Content-Type: application/json
User-Agent: claude-cli/2.1.39 (external, cli)
X-Stainless-Arch: x64
X-Stainless-Lang: js
X-Stainless-OS: Linux
X-Stainless-Package-Version: 0.73.0
X-Stainless-Runtime: node
X-Stainless-Runtime-Version: v24.3.0
X-Stainless-Timeout: 600
anthropic-beta: oauth-2025-04-20,interleaved-thinking-2025-05-14,...
anthropic-version: 2023-06-01
x-app: cli
Host: api.anthropic.com
Accept-Encoding: gzip, deflate, br, zstd
```

Go 发送的请求中，这些 Header 的顺序每次都不确定，且 `Host` 会被强制提到最前面，与真实行为不一致。

---

## 7. P2 — 版本号滞后

### 问题描述

项目硬编码的版本号与最新 Claude Code 版本存在差距：

| Header | sub2api 硬编码值 | 真实 Claude Code v2.1.39 |
|--------|-----------------|--------------------------|
| `User-Agent` | `claude-cli/2.1.22` | `claude-cli/2.1.39` |
| `X-Stainless-Package-Version` | `0.70.0` | `0.73.0` |
| `X-Stainless-Runtime-Version` | `v24.13.0` | `v24.3.0` |

如果 Anthropic 维护了一个"当前活跃 CLI 版本分布"，使用已过时版本的 token 会被标记为可疑。

---

## 8. 附录：真实 Claude Code 网络行为

以下数据来源于 eBPF 逆向分析（[Reverse Engineering Claude Code's SSL Traffic with eBPF](https://medium.com/@yunwei356/reverse-engineering-claude-codes-ssl-traffic-with-ebpf-1dde03bcc7ef)）。

### 进程架构

```
PID 959023 (claude)
├── TID 959023  claude          ← 主线程：JS 执行、终端 I/O、epoll
├── TID 959024  claude          ← 辅助事件循环
├── TID 959025-959031  HeapHelper (7) ← GC 线程
├── TID 959035  HTTP Client     ← 所有 SSL/TLS 流量（axios + native fetch）
├── TID 959036-959625  Bun Pool 0-11 (12) ← Worker 线程
├── TID 959061  File Watcher    ← 文件系统监控
└── TID 994361+ JITWorker (3)   ← JIT 编译
```

### 运行时信息

| 属性 | 值 |
|------|-----|
| 运行时 | Bun v1.3.9-canary.51+d5628db23 |
| 二进制大小 | ~213 MB（单文件） |
| SSL 库 | BoringSSL（静态链接，符号全部 strip） |
| HTTP 协议 | HTTP/1.1 over TLS |
| 导出符号 | 556 个 BUN_1.2 函数 |

### 30 秒捕获的流量统计

```
Total events: 3,088
├── READ/RECV: 3,043 events (SSE streaming responses)
└── WRITE/SEND: 45 events (requests + telemetry)

HTTP Endpoints:
├── POST /v1/messages?beta=true: 12 requests   ← API 对话
├── POST /api/event_logging/batch: 5 requests   ← 遥测
├── POST /api/v2/logs: 2 requests               ← Datadog
└── GET /api/hello: 2 requests                   ← 心跳
```

### 真实请求头（eBPF 捕获）

```http
POST /v1/messages?beta=true HTTP/1.1
Accept: application/json
Authorization: Bearer sk-ant-oat01-...
Content-Type: application/json
User-Agent: claude-cli/2.1.39 (external, cli)
X-Stainless-Arch: x64
X-Stainless-Lang: js
X-Stainless-OS: Linux
X-Stainless-Package-Version: 0.73.0
X-Stainless-Runtime: node
X-Stainless-Runtime-Version: v24.3.0
X-Stainless-Timeout: 600
anthropic-beta: oauth-2025-04-20,interleaved-thinking-2025-05-14,...
anthropic-version: 2023-06-01
x-app: cli
Host: api.anthropic.com
Accept-Encoding: gzip, deflate, br, zstd
```

---

## 9. 修复建议

### P0 — 遥测模拟（最高优先级）

1. **实现心跳模拟**：在使用 OAuth token 期间，定期向 `api.anthropic.com/api/hello` 发送 GET 心跳
2. **实现遥测事件模拟**：构造合理的遥测 payload（包含 session_id、device_id、事件类型等），定期通过 `POST /api/event_logging/batch` 发送
3. **实现 Statsig 请求模拟**：向 `statsig.anthropic.com` 发送必要的分析请求
4. 遥测频率和 payload 格式需要与真实 Claude Code 匹配

### P0 — HTTP 层指纹修复（最高优先级）

1. **替换 `net/http` 为 `bogdanfinn/fhttp`**：项目已经间接依赖了它，需将网关主路径的 HTTP 客户端切换到 `fhttp`，后者支持：
   - 自定义 Header 大小写（保持全小写）
   - 自定义 Header 顺序（固定插入顺序）
2. 或者使用 `bogdanfinn/tls-client` 整合方案，同时解决 TLS 和 HTTP 层指纹

### P1 — TLS 指纹更新

1. **抓取最新 Claude Code 的 TLS ClientHello**：基于 Bun + BoringSSL 重新生成指纹
2. 更新 `dialer.go` 中的 cipher suite、扩展列表、曲线等参数
3. 考虑维护多个版本的指纹 profile，支持 Node.js 和 Bun 两种运行时

### P1 — 指纹多样化

1. **随机化版本号**：为不同账号生成不同的 CLI 版本、SDK 版本、Node/Bun 版本
2. **随机化平台信息**：OS（Linux/macOS/Windows）、Arch（x64/arm64）需要多样化
3. 版本号应在合理范围内随机选取，而非所有用户共享同一固定值

### P1 — Header 顺序固定化

1. 使用 `fhttp` 或自定义 HTTP writer 确保 Header 按固定顺序发送
2. 参考 eBPF 捕获的真实 Header 顺序作为模板

---

## 参考资料

- [Issue #1238 — 遥测](https://github.com/Wei-Shaw/sub2api/issues/1238)
- [Reverse Engineering Claude Code's SSL Traffic with eBPF](https://medium.com/@yunwei356/reverse-engineering-claude-codes-ssl-traffic-with-ebpf-1dde03bcc7ef)
- [Statsig Analytics Endpoint Causing Excessive Network Requests (Issue #8243)](https://github.com/anthropics/claude-code/issues/8243)
- [Telemetry Configuration Ambiguity (Issue #19117)](https://github.com/anthropics/claude-code/issues/19117)
- [Interactive mode ignores ANTHROPIC_BASE_URL (Issue #36998)](https://github.com/anthropics/claude-code/issues/36998)
