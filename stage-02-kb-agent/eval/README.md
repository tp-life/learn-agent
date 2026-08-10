# eval/ —— 练习 8-9 的评估资产

- `dataset.jsonl`：测试集，每行 `{"question": "...", "expect_sources": ["xx.md"]}`。
  当前 8 条样例；练习 8 建议扩充到 20+ 条（覆盖同义改写、字面查询、库外问题等）。
- `sample/`：样例文档，`pnpm eval --sample` 会用它们现场建库，不依赖 ingest 产物。
- 运行方式与输出说明见 `scripts/eval.ts` 头部注释。
- 练习 9 的调优报告（基线指标 → 改动 → 前后对比 → bad case 分析）也放本目录，
  模板见 `docs/solutions/stage-02/exercise-9-tuning-report.md`。
