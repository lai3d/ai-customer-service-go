# Codex Repository Review

- 日期：2026-09-05
- 基准提交：`7d31fc87fc74fdb26df48f3d6147be7b1cee7651`
- 状态：**六条已全部复核并修复**（2026-09-05，见文末「处理结果」）。
- 范围：聊天流程、会话记忆、HTTP/SSE、演示页面、模型客户端、检索、工具、配置及部署脚本。
- 方法：源码检查、现有测试、临时诊断测试。没有调用真实模型 API，也没有修改业务代码。

本文供后续 reviewer 独立复核。P1 表示应优先处理的正确性问题；P2 表示有明确触发条件的功能或可靠性问题。行号对应上述提交。

## 验证结果与边界

以下检查通过：

```bash
make lint
go test -count=1 ./...
```

测试使用 Go 1.26.1、macOS arm64 和 Docker；数据库集成测试通过 Testcontainers 启动独立 pgvector。强制禁用测试结果缓存后，现有测试仍全部通过。

另使用 Go `-overlay` 加载仓库外的临时测试，复现了 R1、R2、R4、R5、R6。R2 运行真实页面脚本，但 DOM 使用 Node 测试替身，没有启动浏览器。R3 来自 Compose 配置对照，未实际部署一套使用自定义凭据的环境。

本次没有执行完整 race suite、负载基准或真实供应商端到端验证。下面的结果不等同于穷尽性审计。项目明确采用的无认证演示范围、模拟业务工具和进程内 LRU 状态，未单独列为缺陷。

## R1 · P1：同一会话的并发请求会混用历史，遗漏当前检索材料

**位置：** [internal/chat/service.go](internal/chat/service.go)，第 142–170 行；`withPassages`。

`Turn` 分别执行预算检查、用户消息写入、检索和历史读取，没有覆盖整个回合的会话级串行化。历史读取可能看到另一请求刚写入的用户消息或助手回答。

**复现：** 使用现有 `newFixture` 和模拟模型客户端，按以下顺序控制两个请求：

1. A 使用会话 `shared`，发送 `question A`；在发出 retrieval 事件时暂停，尚未读取历史。
2. B 使用相同会话发送 `question B`，完成模型调用并持久化 `answer B`。
3. 恢复 A，检查 A 发给模型的消息。

诊断测试观察到：A 的模型请求最后一条消息是 `role=assistant, text="answer B"`。由于 `withPassages` 只向最后一条 user 消息附加材料，A 的检索结果也被丢弃。

**影响：** 两个标签页、重叠请求等正常并发场景可能导致模型回答错误的问题，或基于错误上下文选择工具。同一入口的预算检查也未与后续消费原子化；本次测试没有单独量化并发超支。

**建议：** 按 conversation ID 串行化整个回合，覆盖历史读取、模型调用、预算记录及最终助手回复持久化；不同会话仍可并发。若要支持多实例，需要相应的跨实例协调或版本冲突机制。

## R2 · P2：演示页面静默忽略 SSE 错误

**位置：** [internal/httpapi/web/index.html](internal/httpapi/web/index.html)，第 262–274 行；[internal/httpapi/sse.go](internal/httpapi/sse.go)，第 57 行。

页面仅解析 `data:`，然后用 JSON 的 `type` 判断事件。服务端错误使用 SSE 的 `event: error`，其 JSON 则是 problem 对象；这里的 `type` 不是聊天事件类型。

**复现：** 让真实 HTTP handler 的 `Turner` 返回 `cost.ErrExceeded`，读取响应，再交给页面的实际 submit handler。响应为：

```text
event: error
data: {"type":"","title":"Conversation budget reached","status":429,"detail":"This conversation has reached its token budget. A human agent can take it from here."}
```

Node DOM 测试替身没有收到任何错误提示节点。HTTP 状态已经是 200，页面的 `!res.ok` 分支也不会处理它。

**影响：** 预算耗尽、供应商错误和检索失败可能表现为空白回复，或没有失败提示的部分答案。

**建议：** 使用 SSE 的 `event:` 字段分派事件，再按对应的数据结构解析 payload。不要将 problem 的 `type` 字段当作聊天事件类型。增加服务端错误响应到页面显示的契约测试。

## R3 · P2：Compose 的数据库配置只在应用侧接受覆盖

**位置：** [docker-compose.yml](docker-compose.yml)，第 11–13、19、57–59 行。

Postgres 服务把数据库、用户名和密码硬编码为 `csagent`，应用服务却从 `POSTGRES_DB`、`POSTGRES_USER`、`POSTGRES_PASSWORD` 读取可配置值。

**触发条件：** 在全新数据库卷上部署时，将 `.env` 中任一上述值改为非默认值。数据库仍按固定值初始化，应用却使用覆盖后的连接参数。

**影响：** 修改文档公开的数据库配置会导致应用启动失败。现有 deployment 测试只确认环境变量名称存在于应用服务，没有检查两端的值是否一致。

**建议：** 为两项服务使用一致的数据库配置，并同步 healthcheck 中的用户名和数据库名。已有卷不会因环境变量变化自动更新数据库账号；修复和验证时应区分首次初始化与既有数据迁移，不应删除用户已有卷。

**证据强度：** 源码配置直接矛盾；本次未进行自定义凭据的实际 Compose 部署。

## R4 · P2：数据库 URL 未转义凭据

**位置：** [internal/config/config.go](internal/config/config.go)，第 39–41 行。

`Postgres.URL()` 将用户名、密码和数据库名直接插入 URL，未进行对应的 URL 编码。

**复现：** 构造仅用于测试的配置，密码设为 `test/a#b%`，将 `p.URL()` 传给 `pgx.ParseConfig`。解析在连接数据库之前失败，报告 `invalid port ":test" after host`。

**影响：** 含 URL 特殊字符的有效数据库密码可能使服务无法启动。这与 R3 独立：即使连接到已正确配置的外部数据库，仍然存在。

**建议：** 使用 `net/url` 的结构化 URL 和 `url.UserPassword`，或直接设置 pgx 连接配置字段。用合成凭据验证编码后的解析往返，不在日志或测试输出中暴露真实密码。

## R5 · P2：源码启动端口与快速开始文档不一致

**位置：** [internal/config/config.go](internal/config/config.go)，第 136 行；[.env.example](.env.example)；`cmd/server/main.go` 的 `healthcheck`。

配置默认监听 `:8080`，而 README、AGENTS.md 和 healthcheck 的默认目标均为 8081。`.env.example` 没有设置 `HTTP_ADDR`。Dockerfile 单独设置了 `HTTP_ADDR=:8081`，所以容器启动掩盖了这一差异。

**复现：** 不提供有效的 `HTTP_ADDR` 覆盖，仅设置测试所需的供应商名称和合成 API key，执行 `config.Load()`；得到 `HTTPAddr == ":8080"`。

**影响：** 按源码运行指南执行 `make run` 后，文档中的 8081 地址无法访问该进程，也可能与使用 8080 的 Java 实现冲突。

**建议：** 将源码默认监听端口、healthcheck、环境示例及文档统一为 8081，并对默认端口的一致性增加轻量检查。

## R6 · P2：模型客户端返回流错误时没有主动关闭上游响应

**位置：** [internal/llm/anthropic.go](internal/llm/anthropic.go)，第 75 行；[internal/llm/openai_protocol.go](internal/llm/openai_protocol.go)，第 81 行。

两个客户端创建 SDK stream 后都没有调用 `Close()`。所使用版本的 SDK 在读到流内错误并让 `Next()` 返回 false 时，不会自行关闭响应体。

**复现：** 使用本地 `httptest` provider，发送 SSE 错误帧、flush，然后保持响应打开。以 `context.Background()` 和 5 秒请求超时调用客户端 `Stream`。两个客户端均返回错误，但 200 毫秒后 mock server 的请求上下文仍未取消。

**影响与边界：** 错误返回后，上游响应和相关连接资源仍依赖父上下文取消、供应商关闭连接或超时释放。实际 HTTP handler 返回时会取消请求上下文，因此不能据此断言每次线上错误都会永久泄漏；诊断证明的是客户端缺少确定性的资源关闭。回调错误或解析错误提前退出也需要覆盖。

**建议：** 在每次创建 stream 后立即 `defer stream.Close()`，同时保留现有的部分 usage 汇总逻辑。测试应检查提前返回后的上游关闭行为，而不仅检查错误值和 token 数。

## 后续复核建议

建议其他 reviewer 先独立检查关键路径，再逐项判断本文结论：

- R1：是否允许同一会话并发；若允许，明确历史和回复的顺序保证。
- R2：使用预算错误和供应商错误，在真实浏览器中确认显示行为。
- R3–R5：分别验证默认源码启动、全新 Compose 部署、自定义凭据连接。
- R6：结合实际请求上下文生命周期判断资源滞留的严重程度，避免将有限滞留误报为永久泄漏。

现有测试通过与这些问题并不矛盾：诊断补充了并发交错、跨组件事件契约和非默认配置等原有测试未验证的路径。


---

## 处理结果（2026-09-05，由后续 reviewer 复核后修复）

六条全部独立复核为真，均已修复。每条修复都先确认对应测试在未修复的代码上会失败，而不是只确认修复后变绿。

| | 复核方式 | 处理 |
| --- | --- | --- |
| R1 | 按本文描述的顺序复现：在 A 的 retrieval 事件处暂停（此时尚未读历史），B 跑完，恢复 A。A 的第二次模型调用最后一条消息确为 `role=assistant "answer 1"`，检索材料同时被丢弃 | 按 conversation ID 串行化整个回合。用 channel 而非 `sync.Mutex`，使已断开的调用方能被 ctx 取消而不占队列；锁表按引用计数回收，只受**在途**请求数约束 |
| R2 | 实机复现：错误帧的 JSON `type` 为 `""`，页面 `event.type === 'error'` 永不匹配 | 页面改为按 SSE `event:` 字段分派。补两个测试：页面结构（不得再 switch payload 字段）+ 服务端契约（错误帧必须具名，且 payload 的 `type` 不得恰好等于 "error" 而让错误写法看似能用） |
| R3 | 用临时 project + override 起了一套全新卷：`POSTGRES_USER=reviewuser`、`POSTGRES_PASSWORD=p@ss/w#rd`、`POSTGRES_DB=reviewdb`。healthcheck 转 healthy 并成功连接 | postgres 服务改用与应用侧相同的变量，healthcheck 同步 |
| R4 | `test/a#b%25`、`p@ssw:rd`、`a?b=c&d` 等密码经 `pgx.ParseConfig` 往返 | 改用 `net/url` 构造，`url.UserPassword` 转义 userinfo |
| R5 | `config.Load()` 在无覆盖时确为 `:8080` | 源码默认改为 `:8081`；`.env.example` 补上 `HTTP_ADDR`；新增测试同时检查默认值与文档中不再出现 8080 |
| R6 | **两次尝试的测试都无法区分修与不修** —— 释放来自请求超时（5s、60s 两版各自吻合），不来自 `Close()` | `defer stream.Close()` 已加，但**没有测试**。删掉了那个通不过否定检验的测试，并在源码里写明这条修复基于论证而非证据 |

关于 R6 的说明：本文的复现同样只证明了**未修复时**资源滞留，没有证明加上 `Close()` 会改变该现象。要真正区分，需要把连接池限制为 1 并用第二个请求证明第一个已释放，这需要向 `llm.Options` 注入 http client。在此之前，保留一个两种情况都通过的测试比没有测试更糟。

一点值得记录的：R2 之所以长期存在，是因为 `TestAFailureAfterTheFirstTokenArrivesAsAnErrorEvent` 断言的是**服务端**发出了 `event: error`，它一直是绿的；页面从未进入验证回路。这与本仓库此前记录的模式同形——测试与代码出自同一理解，只能自我确认。第四次了。
