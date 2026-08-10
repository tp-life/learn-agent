# 练习 8 参考答案：eval 脚本指标函数（recall@k 与 MRR）

> 对应 TODO：`stage-02-kb-agent/lib/eval-metrics.ts` 的 `TODO(练习8)`
> （recallAtK、mrr 两个纯函数；脚本骨架 `scripts/eval.ts` 与样例数据
> `eval/sample/`、`eval/dataset.jsonl` 已由 AI 写全）。
> **完成练习并自评后再看本文档。**
>
> 本文档代码已于 2026-08-06 实际粘贴进项目验证（同时临时套用了练习 6
> 参考答案，因为建库依赖 chunk/embedTexts；验证后项目代码已全部恢复为骨架版）：
> - `pnpm build` 通过（Next.js 16.3.0，含 scripts/ 的类型检查）；
> - `EMBEDDING_MOCK=1 pnpm eval --sample` 实测输出：
>   - 8 个问题逐题打印期望源命中名次与 top-3 明细；
>   - `recall@3 = 0.875（7/8 个问题命中）`、`MRR = 0.708`；
>   - Bad case 清单 1 条（#4 overlap 问题，期望 chunking.md 未进 top-3）；
>   - 连跑两次数字完全一致（mock 向量确定性 → 指标可复现）；
> - `EMBEDDING_MOCK=1 pnpm eval`（默认模式，data/kb.json 里只有 1 篇无关文档）
>   → `recall@3 = 0.000`、`MRR = 0.000`、8 条 bad case，行为符合预期；
> - 注意：以上数字来自 mock 假向量（无语义），只证明管线正确，
>   不代表真实检索质量——脚本输出里也有这行警告。

---

## 一、参考实现

### `lib/eval-metrics.ts`（完整替换骨架，此处给出全文）

```ts
/**
 * lib/eval-metrics.ts —— 检索质量指标：recall@k 与 MRR。
 *
 * 在 Evals 链路中的位置：scripts/eval.ts 跑完"问题 → 检索 top-k"后，
 * 把每个问题的"排序结果"和"期望来源"交给本模块算出聚合指标。
 * 指标函数是纯函数（输入数组 → 输出数字），不碰网络/文件/向量库——
 * 这样它们可以脱离整个 RAG 管线单独测试和讨论。
 */

/**
 * checkInputs 校验两个指标共用的输入不变式。
 * 指标函数被喂脏数据应该报错（调用方 bug），而不是返回一个
 * 看起来合理的数字——eval 的意义就是"数字可信"。
 */
function checkInputs(ranked: string[][], expected: string[][]): void {
  if (ranked.length === 0) {
    throw new Error("metrics: empty input");
  }
  if (ranked.length !== expected.length) {
    throw new Error(`metrics: ranked (${ranked.length}) vs expected (${expected.length}) length mismatch`);
  }
}

/**
 * recallAtK —— 期望来源是否进入检索结果前 k 名。
 *
 * ranked[i]：第 i 个问题的检索结果，按得分降序的来源文件名列表（已去重）；
 * expected[i]：第 i 个问题期望命中的来源文件名列表。
 * 返回 [0, 1]：期望来源至少有一个出现在前 k 名的问题占比。
 */
export function recallAtK(ranked: string[][], expected: string[][], k: number): number {
  checkInputs(ranked, expected);
  if (!Number.isInteger(k) || k <= 0) {
    throw new Error(`recallAtK: k must be a positive integer, got ${k}`);
  }
  let hit = 0;
  for (let i = 0; i < ranked.length; i++) {
    const topK = new Set(ranked[i].slice(0, k));
    if (expected[i].some((s) => topK.has(s))) {
      hit++;
    }
  }
  return hit / ranked.length;
}

/**
 * mrr（Mean Reciprocal Rank）—— 第一个正确结果排第几。
 *
 * 每个问题：找到 ranked[i] 中第一个属于 expected[i] 的来源的名次 rank
 * （从 1 开始），贡献 1/rank；未命中贡献 0。返回所有问题的平均值。
 *
 * 与 recall@k 的分工（面试高频考点）：recall@k 只看"进没进前 k"，
 * MRR 看"排得多靠前"。recall 高而 MRR 低 → 找到了但排名靠后 →
 * 该上 rerank（精排）或调相似度阈值，而不是动召回侧。
 */
export function mrr(ranked: string[][], expected: string[][]): number {
  checkInputs(ranked, expected);
  let sum = 0;
  for (let i = 0; i < ranked.length; i++) {
    // findIndex 找到的第一个匹配即"第一个正确结果"——
    // MRR 对后续结果不关心（它衡量"用户只看最前面几条"的场景）。
    const rank = ranked[i].findIndex((s) => expected[i].includes(s));
    if (rank >= 0) {
      sum += 1 / (rank + 1);
    }
  }
  return sum / ranked.length;
}
```

## 二、关键设计点

1. **recall@k 用"命中即 1"的二值定义**：一个问题只要有一个期望源进前 k 就
   算命中。多期望源场景有更严格的变体（|期望 ∩ 前k| / |期望| 按题平均），
   本练习数据集每题只有 1 个期望源，两种定义等价——知道变体存在即可，
   写报告时要说明用的是哪种定义，否则数字没法和别人对齐。

2. **MRR 用 findIndex 取"第一个"正确结果**：`1/(rank+1)` 的衰减很快
   （第 1 名 1.0 → 第 2 名 0.5 → 第 3 名 0.33），这正是它衡量的场景：
   用户只看最前面几条。**易错处**：rank 从 0 开始忘了 +1（第 1 名算出
   Infinity 或 1/0）；把找到的所有正确结果都计入（那是别的指标的思路）。

3. **输入校验是指标函数的一部分**：ranked/expected 长度不符、空输入、
   k<=0 直接 throw。eval 的全部价值在于"数字可信"——喂了脏数据还返回一个
   0.75，比程序崩溃糟糕得多：前者会让调优结论全盘皆错还难以察觉。

4. **按"来源文档"去重再算指标**（在 scripts/eval.ts 里完成，不是本模块）：
   同一文档的多个块可能同时进 top-k，按块算会让 recall 虚高、bad case
   判断失真。指标定义的第一步永远是"单位是什么"——本练习按文档计。

5. **mock 模式下指标可复现但无语义**：EMBEDDING_MOCK 的假向量由文本 hash
   驱动（确定性），所以连跑两次数字一致，管线变更可对比；但假向量没有
   语义，0.875 不是"检索质量好"。换真实 embedding 后数字才有意义——
   这也是为什么脚本在 mock 模式下打印警告。**这本身就是 eval 的一课：
   指标的可信度取决于输入数据的真实性。**

6. **指标之外的 bad case 清单同样重要**：单个数字告诉你"好不好"，
   bad case 清单告诉你"为什么"——每条 bad case 都是一次调优线索
   （见练习 9：是 query 太短？chunk 切坏了？还是该用混合检索？）。
   所以脚本把"哪些题没命中、实际 top 是什么"完整打印出来。

## 三、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [ ] recall@k：前 k 名判定用 `slice(0, k)`；命中语义是"期望源至少一个
      在前 k"；返回命中问题数 / 总问题数
- [ ] MRR：取第一个正确结果的名次，贡献 `1/(rank+1)`（rank 从 0 开始要 +1），
      未命中贡献 0，返回平均值
- [ ] 两个函数对空输入、长度不符、k<=0 都显式报错，而不是返回貌似合理的数字
- [ ] 理解 ranked 列表已按"来源文档"去重（同一文档多块只保留最高名次）
- [ ] `pnpm build` 通过；`EMBEDDING_MOCK=1 pnpm eval --sample` 输出
      recall@3 / MRR 数字与 bad case 清单，连跑两次数字一致
- [ ] （可选但推荐）把 dataset.jsonl 扩充到 20+ 条：同义改写、字面查询
      （如"bge-m3 多少维"）、库外问题（期望应该是"不命中"——想想这种题
      怎么进测试集、指标要不要改）
- [ ] 能口头回答：recall 低和"recall 高 MRR 低"各自指向什么修法？
      为什么 MRR 对后续结果不敏感是特性而不是缺陷？mock 下的指标数字
      能说明什么、不能说明什么？
