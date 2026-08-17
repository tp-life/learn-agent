# 附录 B：结课综合项目——个人知识助理（mini-agent 全链路整合）

> 定位：教程 14 章学完之后、投简历之前的最后一站。这不是新知识的章节，而是一次"把三个阶段串成一个系统"的集成实战——面试官最想听的就是这种"我把它真正组合过"的项目。

## 为什么值得做

三个阶段的练习各自为战：阶段一给了你 ReAct 内核，阶段二给了你 RAG 与记忆，阶段三给了你审批、持久化、状态机的思想。但有一个问题只有集成项目能回答：**这些零件装进同一个进程后，接口对不对得上、概念还成不成立？** 面试里"你做过最有挑战的集成是什么"这类问题，靠单点练习答不出深度。

本项目把 mini-agent 升级为一个**个人知识助理**：会话可恢复、高风险操作有人工审批、内核有离线测试、模型能自主收录文档。四个部分各自对应教程的一组章节，做完你会得到一个可以在面试现场 demo 的东西。

## 前置要求

- 读完教程第 1-13 章（重点：第 2、3、5、9、11 章）；
- 完成阶段一练习 1-4、阶段二练习 1-4（参考答案基于已实现的 `compressIfNeeded` 与 `kb.Ingest`）；
- `cd mini-agent && go test ./...` 全绿。

## 项目总览

```
用户 ──> TUI 主循环（cmd/agent）
          ├── 启动时：session.json → RestoreMessages（P1）
          ├── 每轮结束：SaveMessages 原子落盘（P1）
          └── ReAct 循环（ChatClient 接口，可注入假模型，P3）
                ├── 只读工具：calculator / http_fetch / read_file / kb_search —— 直接执行
                └── 写操作：write_file / kb_ingest（P4）—— ApprovalGate 人工审批（P2）
```

## P1：会话持久化与恢复

**要做什么**：给 mini-agent 加会话快照——每轮对话成功结束后把完整 messages 落盘到 `workspace/session.json`；启动时若快照存在则恢复，接着上次聊。

**涉及概念**：

- checkpoint"每完成一步存一步"的纪律 → [第 9 章](09-task-persistence-and-recovery.md) §1.3
- 原子写（临时文件 + rename）、权限位 → [第 4 章](04-embedding-and-vector-search.md) `Save` 的同款手法
- "历史即状态"——所以保存历史就是保存全部状态 → [第 1 章](01-llm-api-and-messages.md) §1.4

**提示**：落盘文件也是不可信输入，读回来要校验；想清楚"为什么只在 Run 成功后才保存"（快照永远落在 user 边界，不会留半个 tool 调用组）；`Agent` 目前只有 `Messages()` 读接口，需要一个恢复入口。

**验收**：

```bash
go run ./cmd/agent
> 我叫小王，记住这个名字。   # 回答后 exit
go run ./cmd/agent            # 重启
# 期望：启动提示"已恢复上次会话（N 条消息）"
> 我叫什么名字？              # 期望答出"小王"
ls -l workspace/session.json  # 期望权限 -rw-------
```

## P2：工具执行审批闸门

**要做什么**：给 `write_file` 包一层审批——模型每次请求写文件时，终端先打印工具名和参数摘要，等你输入 `y` 才真正执行；其他任何输入都视为拒绝。

**涉及概念**：

- HITL 的三类介入点、工具调用粒度闸门 → [第 11 章](11-human-in-the-loop.md) §1.2、§3.3
- 被拒绝后怎么办：错误回喂，模型自我恢复 → [第 2 章](02-function-calling-and-tools.md) §3.1
- fail-closed：读不到输入时默认拒绝 → 第 11 章 §3.1

**提示**：做成"包装器"而不是改 `write_file` 源码（装饰器模式——闸门与被包装工具实现同一个接口，模型看到的 schema 不变）；拒绝时返回 error 而不是特殊文本，让 Run 里现成的错误回喂路径把它包成 tool 消息；全程序的 stdin 只能有一个 buffered reader（两个会互相抢输入，阶段三 hitl-demo 的坑）。

**验收**：

```bash
> 把"今天天气不错"写入 workspace/diary.txt
# 期望：终端弹 [人工审批] 提示；输 n → agent 回复大意"我取消了写入"且不崩溃；
# 再发一次，输 y → 文件真的写入。
```

## P3：给 Agent 内核补单元测试

**要做什么**：让 `Agent.Run` 可以被离线确定性测试，并补上三个核心用例。

**涉及概念**：脚本化假 LLM、接口定义在使用方 → [第 3 章](03-react-loop-and-agent-kernel.md) §3.4（教学模式）；本章是它的实战版。

**提示**：`Agent` 目前依赖具体类型 `*llm.Client`，假模型插不进去——把字段与 `New` 的参数换成一个只含 `Chat`/`ChatStream` 的接口（注意：签名必须和真实 client 的方法集**完全一致**，否则 `*llm.Client` 不满足接口）；假模型按脚本逐轮播放响应，终稿要模拟走 `onDelta` 回调；三个用例 = 工具调用后终止（断言消息演化序列 + tool_call_id 回挂）、工具错误被回喂、达到 MaxSteps 报错。

**验收**：`go test ./internal/agent/` 全绿；测试全程零网络调用（拔掉网线也能跑）。

## P4（加分项）：kb_ingest 工具——把"收录文档"也交给模型

**要做什么**：新增 `kb_ingest` 工具，让模型能把 workspace 内的文本文件收录进知识库，并把它也挂到 P2 的审批闸门上。

**涉及概念**：RAG 写入路径 → [第 5 章](05-rag-pipeline.md)；工具 Description 工程 → 第 2 章 §3.3；"知识写入是有副作用操作"（花 embedding 额度、改变后续检索结果）所以要审批 → 第 11 章 §3.2。

**提示**：`kb.Ingest` 自带幂等（内容相同跳过）与 all-or-nothing 语义，直接复用；路径防护与 file 工具同构；入库成功后立刻落盘。

**验收**：

```bash
> 把 workspace/notes.md 收录进知识库
# 期望：弹审批 → y → "已收录 … 新增 N 个块"
> notes.md 里讲了什么？   # 期望 agent 调 kb_search 并答出内容
```

## 建议工时

4-8 小时：P1 ≈ 1.5h，P2 ≈ 1.5h，P3 ≈ 2h，P4 ≈ 1h，联调与自评 1-2h。

## 自评清单

做完后逐条自评（这也是面试追问题库）：

- [ ] 会话恢复后模型能正确引用上一轮的事实？
- [ ] 崩溃（直接 kill）后重启，会话恢复到最近一轮、没有半个 JSON？
- [ ] 拒绝审批后 agent 不崩溃、不换工具名硬闯，而是向用户解释？
- [ ] 审批提示不走 stdout（为什么？——MCP 章节）？
- [ ] 三个内核测试拔掉网线仍全绿？
- [ ] 能一句话说清"接口为什么定义在 agent 包而不是 llm 包"？
- [ ] kb_ingest 重复收录同一文件，块数不翻倍？

## 参考答案

[docs/solutions/capstone/exercise-capstone-personal-assistant.md](../solutions/capstone/exercise-capstone-personal-assistant.md)——完成并自评后再看。差异只要是合理设计选择即可，欢迎带着差异点来讨论。

---

返回 [教程首页](README.md) | 上一篇：[附录 A：术语速查表](appendix-a-glossary.md)
