/**
 * app/api/ingest/route.ts —— RAG 写入路径的 HTTP 入口。
 *
 * 全链路：上传文档 → 解析 → chunking（lib/chunk.ts）→ embedding（lib/embed.ts）
 *        → 向量入库（lib/vectorstore.ts）→ 落盘 data/kb.json。
 *
 * 安全边界（Route Handler 是公网入口，必须假设输入是恶意的）：
 *  - 只接受 multipart 表单里名为 file 的文件字段；
 *  - 只放行 .md / .txt 扩展名（纯文本，无需解析库；PDF 支持是进阶要求，见下方 TODO）；
 *  - 限制文件大小，防止超大文件打爆内存 / 刷 embedding 费用；
 *  - 生产环境还需要：鉴权、限流、上传内容审计——学习项目从简，但要知道缺了什么。
 */

import { NextResponse } from "next/server";
import { getKbStore, KB_PATH } from "@/lib/kb";

/** 上传文件大小上限：5MB。个人知识库的 md/txt 文档远超够用。 */
const MAX_FILE_BYTES = 5 * 1024 * 1024;

export async function POST(req: Request) {
  // —— 以下为骨架已实现部分：解析上传 + 输入校验 ——

  // multipart/form-data 解析：Web 标准的 Request.formData()，
  // Next.js Route Handler 直接可用，不需要 multer/busboy 这类库。
  // 注意：非 multipart 请求（如裸 POST）会让 formData() 直接抛异常，
  // 不兜住的话客户端只会看到一个没有任何错误信息的裸 500。
  let form: FormData;
  try {
    form = await req.formData();
  } catch {
    return NextResponse.json(
      { error: "请求体不是合法的 multipart/form-data" },
      { status: 400 }
    );
  }
  const file = form.get("file");
  if (!(file instanceof File)) {
    return NextResponse.json(
      { error: '缺少文件字段：请用 multipart 表单上传，字段名 "file"' },
      { status: 400 }
    );
  }

  // 扩展名白名单：md/txt 是纯文本，file.text() 直接读；
  // PDF 支持是进阶要求（见下方 TODO；参考答案"进阶实现"一节有
  // 经过验证的完整实现，依赖 unpdf）。
  if (!/\.(md|txt)$/i.test(file.name)) {
    return NextResponse.json(
      { error: `暂不支持的文件类型：${file.name}（目前只支持 .md / .txt）` },
      { status: 400 }
    );
  }
  if (file.size > MAX_FILE_BYTES) {
    return NextResponse.json(
      { error: `文件过大：${file.size} 字节，上限 ${MAX_FILE_BYTES} 字节` },
      { status: 413 }
    );
  }

  const text = await file.text();
  if (text.trim() === "") {
    return NextResponse.json({ error: "文件内容为空" }, { status: 400 });
  }

  // TODO(练习6): ingest 组装逻辑（chunk → embed → 入库 → 落盘 → 返回统计）
  //
  // 【任务】把上面解析出的 text 走完写入路径，建议步骤：
  //   1. const chunks = chunk(text)（lib/chunk.ts，TODO 练习6）；
  //      chunks 为空时返回 400。
  //   2. const vectors = await embedTexts(chunks)（lib/embed.ts，TODO 练习6）。
  //   3. 组装 Document 数组入库：getKbStore().add(...docs)。
  //      - id 建议 `${file.name}#${i}`（可读、可定位回来源块）；
  //      - metadata 至少带 { source: file.name, chunk: String(i) }——
  //        练习 7 的"可点击引用"全靠这些溯源信息；
  //      - 注意 add 是 all-or-nothing，维度不符会整批 throw，
  //        用 try/catch 兜住并返回 500 + 错误信息。
  //   4. store.save(KB_PATH) 落盘。
  //   5. 返回 NextResponse.json({ ok: true, file: file.name,
  //      chunks: chunks.length, total: store.size })。
  //
  // 【提示】
  //   - 需要 import：chunk（@/lib/chunk）、embedTexts（@/lib/embed）。
  //   - 重复上传同一文件会入库重复块。可以先不管（检索顶多结果重复），
  //     想处理的话：入库前按 metadata.source 过滤掉旧块——这是个好的
  //     设计讨论点，做完基础版再来想。
  //   - 进阶要求：PDF 支持。扩展名白名单加 .pdf，用 unpdf 把上传的
  //     ArrayBuffer 解析成文本后走同一 chunk/embed/入库管线。
  //     参考答案"进阶实现：PDF 支持"一节有完整实现与验证记录
  //     （含 pdf-parse 在 Next.js 里的已知踩坑说明），做完基础版再做。
  //     注意：任何纯文本提取方案对扫描件（图片型 PDF）都无能为力，
  //     需要 OCR，那是另一条技术路线。
  //
  // 【验收】
  //   EMBEDDING_MOCK=1 pnpm dev 启动后：
  //     echo 一段多段落的中文 markdown 到 /tmp/sample.md
  //     curl -F "file=@/tmp/sample.md" http://localhost:3000/api/ingest
  //   返回 { ok: true, chunks: N, ... } 且 N > 0，
  //   项目目录下生成 data/kb.json，内含 N 条 1024 维向量记录。
  //
  // 参考答案：docs/solutions/stage-02/exercise-6-ingest-pipeline.md（完成后再看）
  void getKbStore; // 骨架期避免未使用告警；实现组装逻辑后删除本行
  return NextResponse.json(
    { error: "ingest 组装逻辑尚未实现（练习 6，见本文件 TODO）" },
    { status: 501 }
  );
}
