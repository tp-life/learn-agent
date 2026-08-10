/**
 * lib/kb.ts —— 知识库向量库的进程级单例。
 *
 * 在 RAG 链路中的位置：写入路径（app/api/ingest/route.ts）和查询路径
 * （app/api/chat/route.ts、scripts/eval.ts）共享同一个 Store 实例——
 * ingest 入库的文档，chat 要能立刻检索到，所以单例必须只有一个。
 *
 * 为什么挂到 globalThis：Next.js dev 模式的热重载会重新执行模块代码，
 * 普通的模块级变量（let store）在热重载后会被重置，库就"丢"了。
 * globalThis 在整个 Node 进程里只有一份，热重载后还在——
 * 这是 Next.js 项目里保存进程级状态的惯用法（Prisma 官方示例同款）。
 * 生产环境（多实例 / serverless）这个模式不成立，得用外部存储——
 * 这正是"内存向量库"架构的边界。
 *
 * 练习：本模块无需用户完成的部分。
 */

import { existsSync } from "node:fs";
import path from "node:path";
import { Store } from "@/lib/vectorstore";

/** 向量库落盘位置。process.cwd() 是项目根目录（next dev / next start 都从这里启动）。 */
export const KB_PATH = path.join(process.cwd(), "data", "kb.json");

const globalForKb = globalThis as unknown as { __kbStore?: Store };

/**
 * getKbStore 返回进程内唯一的向量库实例；首次调用时若磁盘上有
 * 落盘文件（data/kb.json）则恢复——进程重启不丢索引，
 * 避免每次重启都重新跑 embedding（既花钱又慢）。
 */
export function getKbStore(): Store {
  if (!globalForKb.__kbStore) {
    const store = new Store();
    if (existsSync(KB_PATH)) {
      store.load(KB_PATH);
    }
    globalForKb.__kbStore = store;
  }
  return globalForKb.__kbStore;
}
