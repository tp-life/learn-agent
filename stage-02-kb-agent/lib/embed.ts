/**
 * lib/embed.ts —— embedding 客户端：把文本变成向量（RAG 的"翻译器"）。
 *
 * 在 RAG 链路中的位置：chunking 之后、向量库之前；
 * 写入路径（embed 每个 chunk）和查询路径（embed 用户问题）共用本模块，
 * 两侧必须用同一个 embedding 模型，向量空间才对齐。
 *
 * 本模块与 Go 侧 mini-agent/internal/embed（练习 1）对应：
 * DeepSeek 官方没有 embedding API，embedding 必须走另一家服务商——
 * 这里直接 fetch 硅基流动的 OpenAI 兼容接口（POST /v1/embeddings，
 * 模型 BAAI/bge-m3），不引 SDK：批量接口就是一个 POST + JSON，
 * 手写几十行就能说清楚，引 SDK 反而遮住"按 index 归位"这个核心细节。
 */

/**
 * bge-m3 的输出维度。写死它的意义：入库前校验向量长度，
 * 维度错了说明模型/服务商配错了，越早报错越好——
 * 否则错误向量悄悄入库，检索结果全错还很难排查。
 */
export const BGE_M3_DIMENSIONS = 1024;

// TODO(练习6): 批量 embedding 调用 embedTexts
//
// 【任务】实现：
//
//   export async function embedTexts(texts: string[]): Promise<number[][]>
//
// 输入一批文本，返回与之一一对应的向量数组（result[i] 是 texts[i] 的向量）。
// 要求两条路径：
//
//  1. 真实路径：fetch https://api.siliconflow.cn/v1/embeddings
//     - 请求体 { model: "BAAI/bge-m3", input: texts }，
//       请求头 Authorization: `Bearer ${process.env.SILICONFLOW_API_KEY}`；
//     - 入参校验：texts 为空、任一段为空白、apiKey 缺失都要尽早报错；
//     - 响应非 200 时把状态码和响应体带进错误信息（排查限流/余额问题全靠它）；
//     - 核心坑（与 Go 侧相同）：响应 data 数组的顺序不能假设与输入一致！
//       每个元素带 index 字段标明对应 input 的第几段，归位必须按 index 放；
//       同时校验 index 不越界、每段向量维度 === BGE_M3_DIMENSIONS、
//       归位后无空洞（防止服务端漏返回）。
//
//  2. mock 路径：process.env.EMBEDDING_MOCK === "1" 时，不调用真实 API，
//     返回"确定性假向量"——同一段文本永远得到同一个向量
//     （例如用文本 hash 做种子驱动一个简单伪随机发生器，如 xorshift，
//     生成 BGE_M3_DIMENSIONS 维的向量）。
//     为什么要有这个开关（可测试性教学点）：embedding 是外部依赖，
//     没有 key / 没网 / 怕花钱时全链路就没法验证。用确定性的 fake 替换
//     外部依赖，ingest 流水线（chunk → embed → 入库 → 落盘）可以离线
//     端到端跑通；假向量没有语义，检索质量无意义，但"管线通不通"能验证。
//     这正是单测里 mock/stub 思想在手工脚本场景的应用。
//
// 【提示】
//   - Next.js Route Handler 运行在 Node.js 环境，全局 fetch 直接用，
//     不需要 node-fetch 之类的库。
//   - 批量一次请求全部 chunk，比逐段调用省掉大量 HTTP 往返；
//     真实项目里文档很大时要分批（硅基流动单批有上限），本练习可先不分批，
//     在注释里说明这个局限即可。
//
// 【验收】EMBEDDING_MOCK=1 时无需任何 key，curl 上传样例文档（见
// app/api/ingest/route.ts 的 TODO 验收）全链路跑通；有真实
// SILICONFLOW_API_KEY 时关掉 mock 再跑一次，返回的向量应为 1024 维。
//
// 参考答案：docs/solutions/stage-02/exercise-6-ingest-pipeline.md（完成后再看）
export async function embedTexts(texts: string[]): Promise<number[][]> {
  throw new Error("embedTexts: TODO(练习6) 未实现，见本函数上方注释");
}
