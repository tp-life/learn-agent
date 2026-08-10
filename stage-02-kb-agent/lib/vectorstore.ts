/**
 * lib/vectorstore.ts —— 内存向量库：余弦相似度 + 暴力 top-k 检索 + JSON 持久化。
 *
 * 在 RAG 链路中的位置（与 Go 侧 mini-agent/internal/vectorstore 完全同构）：
 *
 *   文档 --(lib/chunk.ts)--> 文本块 --(lib/embed.ts)--> 向量 --(本模块 add)--> 向量库
 *   用户问题 --(lib/embed.ts)--> 查询向量 --(本模块 search)--> 相关块 --(练习 7 聊天)--> 拼进 prompt
 *
 * 本模块是 Go 侧练习 2 参考答案的 TS 移植版
 * （见 docs/solutions/stage-02/exercise-2-vector-store.md），
 * 用户在 Go 侧已完成该练习，这里直接给出完整实现，方便对照两种语言的差异：
 *   - Go 的 []float32 在 TS 里没有对应类型，统一用 number（即 float64）；
 *     JSON 序列化因此天然"最短可往返"，比 Go 侧更省心。
 *   - Go 排序要用 sort.SliceStable 才保证稳定；JS 的 Array.prototype.sort
 *     自 ES2019 起规范保证稳定排序，普通 .sort() 即可。
 *   - Go 用 error 返回值，TS 用 throw——本模块对"调用方 bug"（维度不符、
 *     topK<=0、零向量）一律 throw，因为静默继续会把错误伪装成"检索质量差"。
 *
 * 为什么手写暴力检索而不是直接用向量数据库（面试反直觉考点）：
 * 暴力检索是 O(N) 全表扫描，但 1024 维向量一次点积就是 1024 次乘加，
 * 10 万条记录约 1 亿次浮点运算，现代 CPU 毫秒级跑完。
 * "必须上 ANN 索引（HNSW）"只在百万级以上才成立；学习项目和个人知识库
 * 场景，暴力检索更简单、结果还精确（ANN 是近似，会丢召回）。
 *
 * 练习：本模块无需用户完成的部分（移植自 Go 侧已完成的练习 2）。
 */

import { readFileSync, renameSync, writeFileSync, unlinkSync } from "node:fs";

/** Document 是向量库里的一条记录：一段文本 + 它的向量 + 溯源元数据。 */
export interface Document {
  /**
   * 文档唯一标识，由调用方生成（如 "guide.md#3" 表示 guide.md 的第 3 块）。
   * 入库时校验非空——没有 ID 的记录无法更新、无法在去重时定位。
   */
  id: string;
  /**
   * 原始文本。向量库存它的原因：search 返回的 Hit 要直接能拼进 prompt，
   * 如果只存向量，拿到检索结果还得回查一次原文，多一跳依赖。
   */
  text: string;
  /** text 的 embedding（由 lib/embed.ts 生成，bge-m3 为 1024 维）。 */
  vector: number[];
  /**
   * 溯源信息：来源文档名、chunk 序号等。
   * 引用溯源（"这个答案来自《XX 文档》第 3 段"）全靠它——
   * 没有 metadata，RAG 的答案就无法给出出处，用户无法验证，可信度大打折扣。
   */
  metadata: Record<string, string>;
}

/** Hit 是一次检索命中：文档 + 相似度得分。 */
export interface Hit {
  doc: Document;
  /** 余弦相似度，范围 [-1, 1]，越大越相似。 */
  score: number;
}

/**
 * cosineSimilarity 计算两个向量的余弦相似度：cos(θ) = a·b / (|a|·|b|)。
 *
 * 为什么用余弦而不是欧氏距离：embedding 的"语义"编码在方向上而非长度上，
 * 余弦相似度只比方向、忽略模长，所以两段长短不同但语义相近的文本得分依然高。
 *
 * 三个必须堵住的坑（与 Go 侧相同）：
 *  1. 维度不等必须报错——按短向量截断算出的分数在数学上无意义；
 *  2. 空向量报错；
 *  3. 零向量（模长为 0）报错——零向量没有"方向"，相似度无定义；
 *     若静默产出 NaN，sort 的比较函数对 NaN 恒返回 false，
 *     排序结果不可预期且不报任何错，极难排查。
 */
export function cosineSimilarity(a: number[], b: number[]): number {
  if (a.length !== b.length) {
    throw new Error(`vectorstore: dim mismatch: ${a.length} vs ${b.length}`);
  }
  if (a.length === 0) {
    throw new Error("vectorstore: empty vectors");
  }
  let dot = 0;
  let normA = 0;
  let normB = 0;
  for (let i = 0; i < a.length; i++) {
    dot += a[i] * b[i];
    normA += a[i] * a[i];
    normB += b[i] * b[i];
  }
  if (normA === 0 || normB === 0) {
    throw new Error("vectorstore: zero vector has no direction, cosine similarity undefined");
  }
  return dot / (Math.sqrt(normA) * Math.sqrt(normB));
}

/**
 * Store 是内存向量库。用数组平铺存储，检索时全表扫描（暴力检索）。
 *
 * dim 记录全库统一的向量维度：第一条 add 的记录定下维度，
 * 之后所有记录的维度必须与它一致（见 add 的校验注释）。
 */
export class Store {
  private docs: Document[] = [];
  private dim = 0;

  /**
   * add 批量入库文档。任一文档校验失败则整批拒绝（all-or-nothing），
   * 避免"一半入库一半没入"的中间状态——调用方重试时不用先清理。
   *
   * 校验规则：
   *  1. id 非空、vector 非空（见字段注释）；
   *  2. 全库维度一致：第一条记录定维度，后续维度不符直接拒绝。
   *     为什么这条是硬约束：余弦相似度要求两个向量等长，维度不同的向量
   *     根本无法计算相似度；混进库里会让错误潜伏到检索时才暴露，极难排查。
   *     维度混杂的真实来源通常是"换了 embedding 模型忘了重建索引"。
   */
  add(...docs: Document[]): void {
    // 先整批校验，再统一追加，保证 all-or-nothing。
    for (let i = 0; i < docs.length; i++) {
      const d = docs[i];
      if (d.id === "") {
        throw new Error(`vectorstore: docs[${i}] has empty ID`);
      }
      if (d.vector.length === 0) {
        throw new Error(`vectorstore: docs[${i}] (${d.id}) has empty vector`);
      }
      // 期望维度：库里已有记录用 this.dim；空库时以本批第一条的维度为准
      // （i === 0 时 want 就是自身长度，必然通过——第一条记录定维度）。
      const want = this.dim === 0 ? docs[0].vector.length : this.dim;
      if (d.vector.length !== want) {
        throw new Error(
          `vectorstore: docs[${i}] (${d.id}) dim = ${d.vector.length}, want ${want}`
        );
      }
    }

    if (this.dim === 0 && docs.length > 0) {
      this.dim = docs[0].vector.length; // 第一条记录定维度
    }
    this.docs.push(...docs);
  }

  /** size 返回库中文档数量。 */
  get size(): number {
    return this.docs.length;
  }

  /**
   * search 暴力 top-k 检索：对全库每条记录算余弦相似度，返回得分最高的
   * topK 条，按 score 降序。空库返回空数组（不是错误）。
   *
   * topK <= 0 报错——调用方传 0 几乎一定是 bug（比如忘了设默认值），
   * 静默返回空结果会让上游误以为"没检索到相关内容"。
   * topK 超过库存量时不报错，返回全部即可（调用方想要的是"尽量多"）。
   */
  search(query: number[], topK: number): Hit[] {
    if (topK <= 0) {
      throw new Error(`vectorstore: topK must be positive, got ${topK}`);
    }
    if (this.docs.length === 0) {
      return [];
    }
    if (query.length !== this.dim) {
      throw new Error(`vectorstore: query dim = ${query.length}, want ${this.dim}`);
    }

    const hits: Hit[] = this.docs.map((doc) => ({
      doc,
      score: cosineSimilarity(query, doc.vector),
    }));

    // Array.prototype.sort 自 ES2019 起是稳定排序（Go 侧需要显式用
    // sort.SliceStable）：得分相同（比如两条完全相同的文档）时保持入库顺序，
    // 检索结果可复现——测试和调试都依赖确定性输出。
    hits.sort((x, y) => y.score - x.score);

    return hits.slice(0, Math.min(topK, hits.length));
  }

  /**
   * save 把整个库序列化为 JSON 写入 path（同步实现）。
   *
   * 持久化的动机：内存库进程退出即丢，而重建索引要重新调 embedding API——
   * 既花钱又慢，所以入库一次、落盘复用。
   *
   * 原子写入：先写同目录临时文件，成功后 rename 覆盖目标。
   * 直接写目标文件的话，写到一半进程崩溃会留下半个损坏的 JSON，
   * 下次启动 load 直接挂掉且原数据已丢。rename 在同文件系统内是原子操作，
   * 任意时刻目标路径要么是完整的旧版本、要么是完整的新版本。
   *
   * 同步 vs 异步：save 发生在 ingest 请求末尾、且知识库场景写入频率极低，
   * 同步实现更简单；高并发服务才需要换成异步 + 写队列。
   */
  save(path: string): void {
    const data = JSON.stringify({ dim: this.dim, documents: this.docs }, null, 2);
    const tmp = `${path}.${process.pid}.tmp`;
    try {
      writeFileSync(tmp, data, "utf8");
      renameSync(tmp, path);
    } catch (err) {
      // 错误路径上清理临时文件，否则目录里会堆积 *.tmp 垃圾。
      try {
        unlinkSync(tmp);
      } catch {
        // 临时文件可能根本没创建成功，忽略。
      }
      throw err;
    }
  }

  /**
   * load 从 JSON 文件恢复向量库，重建 dim 并逐条校验。
   *
   * 校验失败时抛错且不改动现有数据（先 load 到局部变量再整体替换），
   * 这样"重载一个坏文件"不会把正在运行的库冲掉。
   * JSON 文件是外部输入（可能被手改、被旧版本程序写出），不能信任，必须校验：
   * 校验规则与 add 完全一致——id 非空、vector 非空、全库维度一致。
   */
  load(path: string): void {
    const raw = readFileSync(path, "utf8");
    const parsed = JSON.parse(raw) as { dim?: number; documents?: Document[] };
    const docs = parsed.documents ?? [];

    // 重建 dim：优先用文件里存的，没有（旧格式/手删）则用第一条记录的维度。
    let dim = parsed.dim ?? 0;
    if (dim === 0 && docs.length > 0) {
      dim = docs[0].vector.length;
    }
    for (let i = 0; i < docs.length; i++) {
      const d = docs[i];
      if (!d.id) {
        throw new Error(`vectorstore: file documents[${i}] has empty ID`);
      }
      if (!Array.isArray(d.vector) || d.vector.length === 0) {
        throw new Error(`vectorstore: file documents[${i}] (${d.id}) has empty vector`);
      }
      if (d.vector.length !== dim) {
        throw new Error(
          `vectorstore: file documents[${i}] (${d.id}) dim = ${d.vector.length}, want ${dim}`
        );
      }
    }

    this.docs = docs;
    this.dim = dim;
  }
}
