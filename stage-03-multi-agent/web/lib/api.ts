/**
 * lib/api.ts —— 看板与 Go 编排引擎 API 之间的类型与请求封装。
 *
 * 类型与 internal/server 的 DTO 一一对应（snake_case 是 Go JSON tag 的约定，
 * 前端照吃不误）。API_BASE 默认指向本地 Go 服务（:8080），
 * 可用 NEXT_PUBLIC_API_BASE 环境变量覆盖。
 *
 * 练习：本模块无需用户完成的部分（练习8 的实现区在详情页）。
 */

export const API_BASE =
  process.env.NEXT_PUBLIC_API_BASE ?? "http://localhost:8080";

/** 与 Go task.Status 的六个取值一一对应。 */
export type TaskStatus =
  | "pending"
  | "planning"
  | "running"
  | "waiting_human"
  | "done"
  | "failed";

export interface TaskView {
  id: string;
  goal: string;
  status: TaskStatus;
  total_tokens: number;
  created_at: string;
  updated_at: string;
}

export interface SubtaskView {
  id: string;
  title: string;
  prompt: string;
  output?: string;
  status: TaskStatus;
  tokens_used: number;
  attempts: number;
  requires_approval: boolean;
}

/** GET /api/tasks/{id} 与 SSE 快照共用的载荷。 */
export interface TaskDetail extends TaskView {
  subtasks: SubtaskView[];
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${API_BASE}${path}`, {
    cache: "no-store",
    ...init,
  });
  if (!res.ok) {
    // 服务端错误统一是 {"error": "..."} 形状（internal/server 的约定）。
    let msg = `HTTP ${res.status}`;
    try {
      const body = (await res.json()) as { error?: string };
      if (body.error) msg = body.error;
    } catch {
      /* 保留默认 msg */
    }
    throw new Error(msg);
  }
  return (await res.json()) as T;
}

export function listTasks(): Promise<{ tasks: TaskView[] }> {
  return request("/api/tasks");
}

export function getTask(id: string): Promise<TaskDetail> {
  return request(`/api/tasks/${id}`);
}

export function createTask(goal: string): Promise<{ task_id: string }> {
  return request("/api/tasks", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ goal }),
  });
}

/** 提交人工审批决定（approve/reject）。by 是审批人标识，审计必须留名。 */
export function decide(
  taskId: string,
  subtaskId: string,
  approved: boolean,
  by: string,
): Promise<{ ok: boolean }> {
  return request(`/api/tasks/${taskId}/approve`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ subtask_id: subtaskId, approved, by }),
  });
}
