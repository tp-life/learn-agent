/**
 * scripts/eval.ts —— RAG 检索评估运行器（练习 8）。
 *
 * 干什么：对测试集里的每个问题跑一次"embed → 向量检索 top-k"，
 * 汇总 recall@k / MRR 两个指标（lib/eval-metrics.ts，TODO 练习8），
 * 并列出 bad case（期望来源没进前 k 的问题）——调优闭环的输入。
 *
 * 用法（项目根目录下）：
 *   pnpm eval                        # 用 data/kb.json（练习 6 ingest 的产物）
 *   pnpm eval --sample               # 用 eval/sample/*.md 现场建库（无需先 ingest）
 *   pnpm eval --k 5                  # 改 k
 *   pnpm eval --dataset <path> --kb <path>
 *
 * 无 key 运行：EMBEDDING_MOCK=1 pnpm eval --sample
 * 注意：mock 向量没有语义（相似文本向量并不相近），此时指标数字
 * 只证明"管线通了"，不代表真实检索质量——这本身就是 eval 的一课：
 * 指标的可信度取决于输入数据的真实性。
 *
 * 练习：本脚本骨架已就绪（CLI、数据加载、检索循环、输出格式），
 * 指标计算见 lib/eval-metrics.ts 的 TODO(练习8)。
 */

import { existsSync, readFileSync, readdirSync } from "node:fs";
import path from "node:path";
import { chunk } from "../lib/chunk";
import { embedTexts } from "../lib/embed";
import { Store, type Document } from "../lib/vectorstore";
import { recallAtK, mrr } from "../lib/eval-metrics";

/** 与 lib/kb.ts 的 KB_PATH 保持一致（脚本独立进程，不用 globalThis 单例）。 */
const DEFAULT_KB_PATH = path.join(process.cwd(), "data", "kb.json");
const DEFAULT_DATASET_PATH = path.join(process.cwd(), "eval", "dataset.jsonl");
const SAMPLE_DIR = path.join(process.cwd(), "eval", "sample");

/** dataset.jsonl 每行的形状：一个问题 + 期望命中的来源文件名列表。 */
interface EvalEntry {
  question: string;
  expect_sources: string[];
}

interface CliOptions {
  kbPath: string;
  datasetPath: string;
  k: number;
  sample: boolean;
}

/**
 * parseArgs 手写极简 CLI 解析（不引 commander/yargs——4 个开关的脚本，
 * 引库反而掩盖"参数解析没什么神秘的"这个事实）。
 */
function parseArgs(argv: string[]): CliOptions {
  const opts: CliOptions = {
    kbPath: DEFAULT_KB_PATH,
    datasetPath: DEFAULT_DATASET_PATH,
    k: 3,
    sample: false,
  };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    const takeValue = (name: string): string => {
      const v = argv[++i];
      if (v === undefined) throw new Error(`参数 ${name} 缺少值`);
      return v;
    };
    if (arg === "--sample") {
      opts.sample = true;
    } else if (arg === "--kb") {
      opts.kbPath = path.resolve(takeValue("--kb"));
    } else if (arg === "--dataset") {
      opts.datasetPath = path.resolve(takeValue("--dataset"));
    } else if (arg === "--k") {
      opts.k = Number(takeValue("--k"));
      if (!Number.isInteger(opts.k) || opts.k <= 0) {
        throw new Error(`--k 必须是正整数，got ${opts.k}`);
      }
    } else if (arg === "--help" || arg === "-h") {
      console.log("用法: pnpm eval [--sample] [--kb <path>] [--dataset <path>] [--k <n>]");
      process.exit(0);
    } else {
      throw new Error(`未知参数: ${arg}（--help 查看用法）`);
    }
  }
  return opts;
}

/**
 * loadDataset 读取 jsonl 测试集并逐行校验。
 * 测试集是 eval 的地基：脏数据会让指标"看起来能跑"但结论全错，
 * 所以每行都严格校验形状，带行号报错。
 */
function loadDataset(datasetPath: string): EvalEntry[] {
  if (!existsSync(datasetPath)) {
    throw new Error(`测试集不存在: ${datasetPath}`);
  }
  const lines = readFileSync(datasetPath, "utf8").split("\n");
  const entries: EvalEntry[] = [];
  lines.forEach((line, idx) => {
    const trimmed = line.trim();
    if (trimmed === "") return; // 允许空行
    const parsed = JSON.parse(trimmed) as Partial<EvalEntry>; // 形状在下一行校验
    if (
      typeof parsed.question !== "string" ||
      parsed.question.trim() === "" ||
      !Array.isArray(parsed.expect_sources) ||
      parsed.expect_sources.length === 0 ||
      !parsed.expect_sources.every((s) => typeof s === "string" && s !== "")
    ) {
      throw new Error(
        `测试集第 ${idx + 1} 行形状非法：需要 {"question": string, "expect_sources": [string, ...]}`
      );
    }
    entries.push({ question: parsed.question, expect_sources: parsed.expect_sources });
  });
  if (entries.length === 0) {
    throw new Error(`测试集为空: ${datasetPath}`);
  }
  return entries;
}

/**
 * buildSampleStore 用 eval/sample/*.md 现场建一个内存向量库：
 * 与 ingest 管线同源（chunk → embedTexts → add），只是不落盘。
 * 这样 eval 可以在不动 data/kb.json 的情况下独立运行。
 */
async function buildSampleStore(): Promise<{ store: Store; files: number }> {
  if (!existsSync(SAMPLE_DIR)) {
    throw new Error(`样例目录不存在: ${SAMPLE_DIR}`);
  }
  const files = readdirSync(SAMPLE_DIR).filter((f) => /\.(md|txt)$/i.test(f));
  if (files.length === 0) {
    throw new Error(`样例目录里没有 .md/.txt 文档: ${SAMPLE_DIR}`);
  }

  // 先切完所有文档的块，再一次批量 embed（一次 HTTP 往返，比逐文档调用快）。
  const docs: { source: string; chunkIndex: number; text: string }[] = [];
  for (const f of files.sort()) {
    const text = readFileSync(path.join(SAMPLE_DIR, f), "utf8");
    chunk(text).forEach((t, i) => docs.push({ source: f, chunkIndex: i, text: t }));
  }
  const vectors = await embedTexts(docs.map((d) => d.text));

  const store = new Store();
  const documents: Document[] = docs.map((d, i) => ({
    id: `${d.source}#${d.chunkIndex}`,
    text: d.text,
    vector: vectors[i],
    metadata: { source: d.source, chunk: String(d.chunkIndex) },
  }));
  store.add(...documents);
  return { store, files: files.length };
}

/** loadKbStore 从落盘文件加载向量库（练习 6 ingest 的产物）。 */
function loadKbStore(kbPath: string): Store {
  if (!existsSync(kbPath)) {
    throw new Error(
      `知识库不存在: ${kbPath}\n先跑 ingest（curl -F "file=@doc.md" localhost:3000/api/ingest），或用 --sample 模式`
    );
  }
  const store = new Store();
  store.load(kbPath);
  return store;
}

async function main() {
  const opts = parseArgs(process.argv.slice(2));
  const entries = loadDataset(opts.datasetPath);
  const { store, kbDesc } = opts.sample
    ? await buildSampleStore().then(({ store, files }) => ({
        store,
        kbDesc: `${SAMPLE_DIR}（${files} 篇文档，${store.size} 块，--sample 现场建库）`,
      }))
    : { store: loadKbStore(opts.kbPath), kbDesc: `${opts.kbPath}（落盘知识库）` };

  console.log("=== RAG 检索评估 ===");
  console.log(`知识库: ${kbDesc}`);
  console.log(`数据集: ${opts.datasetPath}（${entries.length} 个问题）`);
  console.log(`k = ${opts.k}`);
  if (process.env.EMBEDDING_MOCK === "1") {
    console.log("⚠ EMBEDDING_MOCK=1：假向量没有语义，指标只验证管线通畅，不代表真实检索质量");
  }
  console.log("");

  // 批量 embed 所有问题（一次请求），再逐题检索。
  const queryVectors = await embedTexts(entries.map((e) => e.question));

  const rankedAll: string[][] = []; // 每题的排序来源列表（去重后）
  const expectedAll: string[][] = [];
  const badCases: { index: number; entry: EvalEntry; top: string[] }[] = [];

  entries.forEach((entry, i) => {
    const hits = store.search(queryVectors[i], opts.k);
    // 同一文档可能多个块进 top-k：按名次去重，指标按"来源文档"计。
    const ranked: string[] = [];
    for (const h of hits) {
      const src = h.doc.metadata.source ?? h.doc.id;
      if (!ranked.includes(src)) ranked.push(src);
    }
    rankedAll.push(ranked);
    expectedAll.push(entry.expect_sources);

    const firstHitRank = ranked.findIndex((s) => entry.expect_sources.includes(s));
    const topDesc = hits
      .map((h) => `${h.doc.metadata.source ?? h.doc.id}(${h.score.toFixed(3)})`)
      .join(" | ");
    console.log(
      `[${i + 1}/${entries.length}] ${entry.question}\n` +
        `      期望: ${entry.expect_sources.join(", ")} → ` +
        (firstHitRank >= 0 ? `命中第 ${firstHitRank + 1} 名` : "未命中（bad case）") +
        `\n      top-${opts.k}: ${topDesc || "(空库，无结果）"}`
    );
    if (firstHitRank < 0) {
      badCases.push({ index: i + 1, entry, top: ranked });
    }
  });

  // —— 聚合指标（TODO 练习8 实现的两个函数）——
  let metricsFailed = false;
  console.log("\n=== 汇总 ===");
  try {
    const recall = recallAtK(rankedAll, expectedAll, opts.k);
    const mrrValue = mrr(rankedAll, expectedAll);
    console.log(
      `recall@${opts.k} = ${recall.toFixed(3)}（${Math.round(recall * entries.length)}/${entries.length} 个问题命中）`
    );
    console.log(`MRR      = ${mrrValue.toFixed(3)}`);
  } catch (err) {
    // 骨架期指标函数是 TODO：不让异常中断整个报告，bad case 清单照样输出。
    metricsFailed = true;
    console.log(`指标计算失败：${err instanceof Error ? err.message : String(err)}`);
  }

  console.log(`\n=== Bad cases（${badCases.length} 个：期望来源未进 top-${opts.k}）===`);
  for (const bc of badCases) {
    console.log(
      `#${bc.index} ${bc.entry.question}\n` +
        `  期望: ${bc.entry.expect_sources.join(", ")}；实际 top: ${bc.top.join(", ") || "(无)"}`
    );
  }
  if (badCases.length === 0) {
    console.log("（无）");
  }
  if (metricsFailed) {
    process.exitCode = 1;
  }
}

main().catch((err) => {
  console.error(`eval 失败：${err instanceof Error ? err.message : String(err)}`);
  process.exitCode = 1;
});
