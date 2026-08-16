/**
 * components/status-badge.tsx —— 状态徽章：任务/子任务状态的可视化。
 *
 * waiting_human 用醒目的橙底——它是唯一"需要人做事"的状态，
 * 看板的第一职责就是让审批项一眼跳出来。
 *
 * 练习：本模块无需用户完成的部分。
 */

import type { TaskStatus } from "@/lib/api";

const COLORS: Record<TaskStatus, { bg: string; fg: string }> = {
  pending: { bg: "#eee", fg: "#555" },
  planning: { bg: "#e3f2fd", fg: "#1565c0" },
  running: { bg: "#e8f5e9", fg: "#2e7d32" },
  waiting_human: { bg: "#fff3e0", fg: "#e65100" },
  done: { bg: "#e0e0e0", fg: "#1b5e20" },
  failed: { bg: "#ffebee", fg: "#c62828" },
};

export function StatusBadge({ status }: { status: TaskStatus }) {
  const c = COLORS[status] ?? COLORS.pending;
  return (
    <span
      style={{
        background: c.bg,
        color: c.fg,
        borderRadius: 4,
        padding: "2px 8px",
        fontSize: 12,
        fontWeight: 600,
        whiteSpace: "nowrap",
      }}
    >
      {status}
    </span>
  );
}
