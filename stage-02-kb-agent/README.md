# stage-02-kb-agent —— 全栈知识库 Agent（阶段二 · 项目 2）

> 本项目的定位、练习清单与设计决策见 `docs/stages/stage-02-rag-memory-evals.md`（工作区根目录下）。
> 代码即教材：注释密度偏高是有意的，请连注释一起读。

## 这是什么

阶段二（RAG / Memory / Evals）的项目 2：一个可以**上传文档、基于文档问答、引用可溯源**的知识库 Agent。
与 Go 侧的 `mini-agent/` 零依赖、互为对照——同一条 RAG 链路的两种语言实现：

| 环节 | Go 侧（mini-agent） | 本项目（TS） |
| --- | --- | --- |
| embedding 客户端 | `internal/embed`（练习 1 ✅） | `lib/embed.ts`（练习 6） |
| 向量库 | `internal/vectorstore`（练习 2） | `lib/vectorstore.ts`（已移植，无需练习） |
| chunking | `internal/rag/chunk.go`（练习 3） | `lib/chunk.ts`（练习 6） |
| RAG 写入路径 | CLI 灌文档（练习 4） | `app/api/ingest/route.ts`（练习 6） |
| 问答界面 | CLI 交互 | Next.js 页面 + 流式（练习 7） |
| Evals | — | `scripts/` + `eval/`（练习 8-9） |

技术栈：Next.js（App Router）+ TypeScript + Vercel AI SDK；聊天模型 DeepSeek（OpenAI 兼容），
embedding 用硅基流动 bge-m3（直接 fetch，不引 SDK）。

## 目录结构

```
stage-02-kb-agent/
├── app/
│   ├── page.tsx              # 练习 7：问答界面（useChat + 流式渲染）
│   ├── layout.tsx            # 根布局
│   └── api/
│       ├── ingest/route.ts   # 练习 6：文档上传 → chunk → embed → 入库 → 落盘
│       └── chat/route.ts     # 练习 7：流式问答（检索 → 防幻觉 prompt → SSE）
├── components/
│   └── sources-card.tsx      # 练习 7：引用卡片（渲染 data part 里的检索来源）
├── lib/
│   ├── vectorstore.ts        # 内存向量库（余弦相似度 + 暴力 top-k + JSON 持久化）
│   │                         #   移植自 Go 侧练习 2 参考答案，已写全
│   ├── chunk.ts              # 练习 6：文档切分（结构优先 + overlap 硬切兜底）
│   ├── embed.ts              # 练习 6：硅基流动 bge-m3 批量 embedding（含 mock 开关）
│   ├── kb.ts                 # 向量库进程级单例（ingest/chat 共享，已写全）
│   ├── chat-types.ts         # 聊天链路前后端共享类型（SourceItem / KbUIMessage，已写全）
│   └── eval-metrics.ts       # 练习 8：recall@k 与 MRR（纯函数 TODO）
├── scripts/
│   └── eval.ts               # 练习 8：检索评估运行器（pnpm eval，骨架已就绪）
├── eval/
│   ├── dataset.jsonl         # 练习 8：测试集（8 条样例，建议扩充到 20+）
│   └── sample/               # 练习 8：样例文档（--sample 模式现场建库）
├── data/                     # 运行期生成：kb.json（向量库落盘，已 gitignore）
├── .env.example              # 环境变量模板（复制为 .env 并填 key）
└── package.json
```

## 跑起来

```bash
pnpm install

# 没有任何 key 也能跑通练习 6-7 全链路（embedding 走确定性假向量，LLM 走罐装假模型）：
EMBEDDING_MOCK=1 CHAT_MOCK=1 pnpm dev

# 有 key 时：cp .env.example .env，填入 SILICONFLOW_API_KEY / DEEPSEEK_API_KEY，然后
pnpm dev
```

上传文档（练习 6 完成 TODO 后生效）：

```bash
curl -F "file=@sample.md" http://localhost:3000/api/ingest
# => {"ok":true,"file":"sample.md","chunks":N,"total":M}
# 向量库落盘到 data/kb.json
```

问答（练习 7 完成两处 TODO 后生效）：浏览器打开 http://localhost:3000 ，
提问后流式回答 + 助手消息下方引用卡片（点击展开原文）。

检索评估（练习 8 完成 TODO 后生效）：

```bash
EMBEDDING_MOCK=1 pnpm eval --sample   # 用 eval/sample/ 现场建库，无需先 ingest
pnpm eval                             # 用 data/kb.json（ingest 的产物）
# 输出：逐题命中名次 + recall@k / MRR + bad case 清单
```

其他命令：`pnpm build`（生产构建，含类型检查）、`pnpm typecheck`（仅 tsc --noEmit）。

## 练习入口

按 AGENTS.md 约定：AI 只写骨架，练习部分在代码里以 `TODO(练习N)` 标注（含【任务】【提示】【验收】），
参考答案在 `docs/solutions/stage-02/`，**完成并自评后再看**。

| # | 练习 | 入口 | 状态 |
| --- | --- | --- | --- |
| 6 | 文档上传 → chunking → embedding → 入库 | `lib/chunk.ts`、`lib/embed.ts`、`app/api/ingest/route.ts` 的三处 `TODO(练习6)` | 📖 骨架就绪（[参考答案](../../docs/solutions/stage-02/exercise-6-ingest-pipeline.md)） |
| 7 | 问答界面：流式回答 + 可查看引用 | `app/api/chat/route.ts` 的 `TODO(练习7)`（检索 → 防幻觉 prompt）、`components/sources-card.tsx` 的 `TODO(练习7)`（引用卡片）；`app/page.tsx` 聊天 UI 已由 AI 写全 | 📖 骨架就绪（[参考答案](../../docs/solutions/stage-02/exercise-7-chat-ui.md)） |
| 8 | eval 脚本：测试集 + 召回率/MRR + bad case | `lib/eval-metrics.ts` 的 `TODO(练习8)`（recallAtK / mrr）；`scripts/eval.ts`、`eval/` 已由 AI 写全 | 📖 骨架就绪（[参考答案](../../docs/solutions/stage-02/exercise-8-eval-script.md)） |
| 9 | 基于 eval 的调优闭环 | 无代码 TODO，产出调优报告 `eval/tuning-report.md` | 📖 模板就绪（[参考答案/模板](../../docs/solutions/stage-02/exercise-9-tuning-report.md)） |

## 环境变量

| 变量 | 用途 | 何时需要 |
| --- | --- | --- |
| `SILICONFLOW_API_KEY` | 硅基流动 embedding（bge-m3） | 练习 6 真实路径；`EMBEDDING_MOCK=1` 时不需要 |
| `DEEPSEEK_API_KEY` | DeepSeek 聊天模型 | 练习 7 真实路径；`CHAT_MOCK=1` 时不需要 |
| `EMBEDDING_MOCK` | 设为 `1` 时 embedding 走确定性假向量 | 无 key 时跑通 ingest 全链路 / 离线调试 |
| `CHAT_MOCK` | 设为 `1` 时聊天走罐装假模型（MockLanguageModelV3） | 无 key 时跑通问答全链路 / 离线调试 |
