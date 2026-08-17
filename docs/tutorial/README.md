# AI Agent 开发实战教程（从零到手写三个项目）

> 本教程面向**会 Go/TypeScript 基础语法、懂 HTTP，但零 Agent 开发经验**的工程师。
> 读完全部章节并动手完成练习后，你将能独立开发三类 Agent 系统（单 Agent 框架、RAG 知识库、多 Agent 编排系统），并掌握求职面试所需的全部核心知识。
> 教程以本仓库的三个真实项目为教材，所有代码精讲均引用仓库内可运行的代码。

---

## 本教程与其他文档的关系

本仓库的文档体系分工如下，建议按下面的方式组合使用：

| 文档 | 作用 | 什么时候用 |
| --- | --- | --- |
| **本教程（`docs/tutorial/`）** | 学习主线：从零讲清每个概念，代码逐段精讲，进阶拓展带代码，面试深挖 | 第一遍系统学习时按章阅读 |
| `docs/stages/stage-XX-*.md` | 阶段复习资料：考点清单、易混淆对比表、时序图 | 学完对应章节后自测、面试前冲刺复习 |
| `docs/solutions/stage-XX/` | 练习参考答案 | 完成练习**之后**对照（提前看会失去练习意义） |
| `docs/ROADMAP.md` | 总目标与进度 | 任何时候找回"我在哪、下一步做什么" |

## 学习路径与章节地图

教程按三个项目递进，章节与练习一一对应。**读一章 → 做对应练习 → 对照参考答案 → 回到阶段文档自测**，是最有效的节奏。

### 第一部分：地基——手写迷你 Agent 框架（项目 1 `mini-agent/`）

| 章 | 内容 | 对应代码 | 对应练习（阶段一） |
| --- | --- | --- | --- |
| [第 1 章](01-llm-api-and-messages.md) | LLM API 与 messages 协议：四角色、无状态 API、token 与成本 | `mini-agent/internal/llm/` | 跑通 CLI |
| [第 2 章](02-function-calling-and-tools.md) | Function Calling 与工具层设计：协议、安全边界、说明书工程 | `mini-agent/internal/tools/` | 练习 4（文件工具） |
| [第 3 章](03-react-loop-and-agent-kernel.md) | ReAct 循环与 Agent 内核：流式聚合、重试退避、上下文压缩 | `mini-agent/internal/agent/` | 练习 1-3（SSE / 重试 / 压缩） |

### 第二部分：进阶——RAG、Memory 与评估（项目 1 扩展 + 项目 2 `stage-02-kb-agent/`）

| 章 | 内容 | 对应代码 | 对应练习（阶段二） |
| --- | --- | --- | --- |
| [第 4 章](04-embedding-and-vector-search.md) | Embedding 与向量检索：余弦相似度、暴力检索 vs HNSW、向量库选型 | `mini-agent/internal/embed/`、`internal/vectorstore/` | 练习 1-2 |
| [第 5 章](05-rag-pipeline.md) | RAG 全链路：chunking、入库、检索工具、带引用生成、bad case 三板斧 | `mini-agent/internal/rag/` | 练习 3-4 |
| [第 6 章](06-memory-and-evals.md) | 长期 Memory 与 Evals：记忆即检索、recall@k / MRR、LLM-as-judge | `mini-agent/internal/memory/`、`stage-02-kb-agent/scripts/` | 练习 5、8-9 |
| [第 7 章](07-fullstack-kb-agent.md) | 全栈知识库 Agent：Next.js + Vercel AI SDK、流式 UI、引用卡片 | `stage-02-kb-agent/` | 练习 6-7 |

### 第三部分：深入——多 Agent 编排与生产化（项目 3 `stage-03-multi-agent/`）

| 章 | 内容 | 对应代码 | 对应练习（阶段三） |
| --- | --- | --- | --- |
| [第 8 章](08-go-concurrency-and-worker-pool.md) | Go 并发编排：errgroup、context 预算、semaphore 限流 | `stage-03-multi-agent/internal/pool/` | 练习 1 |
| [第 9 章](09-task-persistence-and-recovery.md) | 任务状态机与崩溃恢复：checkpoint、幂等键、SQLite | `stage-03-multi-agent/internal/task/` | 练习 2 |
| [第 10 章](10-orchestrator-planner-worker-critic.md) | 多 Agent 编排：planner/worker/critic、计划校验、双重熔断 | `stage-03-multi-agent/internal/orchestrator/` | 练习 3-4 |
| [第 11 章](11-human-in-the-loop.md) | Human-in-the-loop：审批点、暂停-恢复、事件驱动 | `stage-03-multi-agent/internal/hitl/` | 练习 5 |
| [第 12 章](12-observability-and-mcp.md) | 可观测性与 MCP：嵌套 trace、成本归因、MCP 协议 | `stage-03-multi-agent/internal/trace/`、`cmd/mcp-server/` | 练习 6-7 |
| [第 13 章](13-server-sse-dashboard.md) | 产品化集成：HTTP/SSE API、实时看板 | `stage-03-multi-agent/internal/server/`、`web/` | 练习 8-9 |

### 第四部分：求职——把项目兑换成 offer

| 章 | 内容 | 对应文档 |
| --- | --- | --- |
| [第 14 章](14-interview-and-career.md) | 面试作战手册：题库索引、系统设计答题骨架、简历写法、STAR 行为题 | `docs/stages/stage-04-job-hunting.md` |

### 附录

| 篇 | 内容 |
| --- | --- |
| [附录 A：术语速查表](appendix-a-glossary.md) | 14 章全部核心概念的一页纸索引（概念 → 一句话定义 → 回链章节），面试前 20 分钟自测用 |
| [附录 B：结课综合项目](appendix-b-capstone.md) | 把 mini-agent 升级为个人知识助理（会话持久化 + 审批闸门 + 内核单测 + kb_ingest 工具），一次串起三个阶段；[参考答案](../solutions/capstone/exercise-capstone-personal-assistant.md) |

## 环境准备

| 依赖 | 用途 | 备注 |
| --- | --- | --- |
| Go（1.22+） | 项目 1、3 与全部 Go 概念练习 | `go version` 确认 |
| Node.js + pnpm | 项目 2、项目 3 看板 | 本机注意：系统 node v22 与 pnpm 11 不兼容，TS 命令前加 `PATH=/opt/homebrew/opt/node/bin:$PATH` |
| `DEEPSEEK_API_KEY` | 生成模型（全部项目） | [platform.deepseek.com](https://platform.deepseek.com) 注册，几元/月 |
| `SILICONFLOW_API_KEY` | embedding（第 4 章起） | 硅基流动注册，bge-m3 有免费档 |
| Ollama（可选） | 本地跑 bge-m3，零成本零网络 | `ollama pull bge-m3` |

验证环境（项目 1 已完整可跑）：

```bash
cd mini-agent
DEEPSEEK_API_KEY=sk-xxx go run ./cmd/agent
# 输入：357 乘以 482 等于多少？  → 观察 Verbose 输出的 ReAct 循环
```

## 每章的固定结构

- **概念详解**：从零讲起——直觉类比 → 精确定义 → 为什么这样设计；
- **代码精讲**：仓库真实代码逐段讲解，引用格式 `path:line`，可对照源码阅读；
- **进阶拓展**：超出最小可用版本的主题（生产化、原理深挖），**每个拓展点都带代码**；
- **面试视角**：高频题的标准回答 + 追问链 + 加分点，比阶段文档更深；
- **常见坑**：仓库里真实踩过的坑，解释根因；
- **动手练习**：对应 `TODO(练习N)` 的位置与验收方式，参考答案做完再看。

## 使用约定

- 教程中的代码分两类：**项目代码精讲**（逐字来自仓库，标注了 `path:line`）与**教学示例代码**（为讲清概念而写，可独立成立，不依赖仓库）。
- 阶段三（第 8-13 章）的项目代码是**练习骨架**：教程讲清概念与模式，但完整实现要你亲手完成——这是本教程的设计，不是遗漏。参考答案在 `docs/solutions/stage-03/`，做完再看。
- 不确定时效的事实（价格、模型上下文窗口等）以官方文档为准，教程中均有标注。
