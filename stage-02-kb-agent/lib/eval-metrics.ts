/**
 * lib/eval-metrics.ts —— 检索质量指标：recall@k 与 MRR。
 *
 * 在 Evals 链路中的位置：scripts/eval.ts 跑完"问题 → 检索 top-k"后，
 * 把每个问题的"排序结果"和"期望来源"交给本模块算出聚合指标。
 * 指标函数是纯函数（输入数组 → 输出数字），不碰网络/文件/向量库——
 * 这样它们可以脱离整个 RAG 管线单独测试和讨论。
 *
 * 练习：本模块的两个函数都是 TODO(练习8)，由用户实现。
 */

function checkInputs(ranked: string[][], expected: string[][]): void {
  if (ranked.length === 0) {
    throw new Error("metrics: empty");
  }

  if (ranked.length !== expected.length) {
    throw new Error(
      `metrics: ranked (${ranked.length}) vs expected (${expected.length}) length mismatch`,
    );
  }
}

/**
 * TODO(练习8): recall@k —— 期望来源是否进入检索结果前 k 名
 *
 * 【任务】实现：
 *
 *   recallAtK(ranked: string[][], expected: string[][], k: number): number
 *
 *   - ranked[i]：第 i 个问题的检索结果，按得分降序的来源文件名列表
 *     （已由调用方去重：同一文档的多个块只保留最高名次）；
 *   - expected[i]：第 i 个问题期望命中的来源文件名列表（通常 1 个，可多个）；
 *   - 返回 [0, 1]：所有问题中，"期望来源至少有一个出现在 ranked[i] 前 k 名"
 *     的比例。
 *
 *   定义（单期望源的简化版，学习项目够用）：
 *     recall@k = 命中问题数 / 总问题数
 *   变体（多期望源时更严格的定义）：|期望 ∩ 前k| / |期望| 按问题取平均——
 *   知道有这回事即可，本练习用简化版。
 *
 * 【提示】
 *   - 边界：ranked/expected 长度不一致、空数组、k <= 0 时怎么处理？
 *     指标函数被喂了脏数据应该报错（调用方 bug），而不是返回一个
 *     看起来合理的数字——eval 的意义就是"数字可信"。
 *   - "前 k 名"是 ranked[i].slice(0, k)。
 *
 * 参考答案：docs/solutions/stage-02/exercise-8-eval-script.md（完成后再看）
 */
export function recallAtK(
  ranked: string[][],
  expected: string[][],
  k: number,
): number {
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
 * TODO(练习8): MRR（Mean Reciprocal Rank）—— 第一个正确结果排第几
 *
 * 【任务】实现：
 *
 *   mrr(ranked: string[][], expected: string[][]): number
 *
 *   每个问题：找到 ranked[i] 中第一个属于 expected[i] 的来源的名次 rank
 *   （从 1 开始），贡献 1/rank；一个都没命中贡献 0。
 *   MRR = 所有问题贡献的平均值，范围 [0, 1]。
 *
 *   例：期望源排第 1 → 1；排第 2 → 1/2；排第 3 → 1/3；未命中 → 0。
 *
 * 【提示】
 *   - 与 recall@k 的分工（面试高频考点）：recall@k 只看"进没进前 k"，
 *     MRR 看"排得多靠前"。两个数字一起读才有意义：
 *       recall 低        → 检索根本没找到（查 chunk/embedding/混合检索）；
 *       recall 高、MRR 低 → 找到了但排名靠后 → 该上 rerank（精排）
 *                          或调相似度阈值，而不是动召回侧。
 *   - MRR 对"第一个正确结果"敏感、对后续结果不关心——它衡量的场景是
 *     "用户只看最前面几条"，适合问答；如果要评估整个列表的排序质量
 *     应该用 nDCG（了解即可）。
 *
 * 参考答案：docs/solutions/stage-02/exercise-8-eval-script.md（完成后再看）
 */
export function mrr(ranked: string[][], expected: string[][]): number {
  checkInputs(ranked, expected);
  let sum = 0;
  for (let i = 0; i < ranked.length; i++) {
    const rank = ranked[i].findIndex((s) => expected[i].includes(s));
    if (rank >= 0) {
      sum += 1 / (rank + 1);
    }
  }

  return sum / ranked.length;
}
