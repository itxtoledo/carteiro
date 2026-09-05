export interface Account {
  email: string;
  allowed_from: string[];
}

export interface QueueStats {
  queued: number;
  due: number;
  dead: number;
}

export interface Stats {
  version: string;
  uptime_seconds: number;
  counters: Record<string, number>;
  queue: QueueStats;
}

export type SendStatus = "queued" | "delivered" | "dead";

export interface SendSummary {
  id: string;
  from: string;
  to: string[];
  subject: string;
  status: SendStatus;
  attempts: number;
  last_error?: string;
  queued_at: string;
  updated_at: string;
  size: number;
  truncated?: boolean;
}

export interface SendDetail extends SendSummary {
  html?: string;
  text?: string;
  raw?: string;
}

export interface Health {
  status: string;
}

export interface SendResult {
  id: string;
  status: SendStatus;
}

export function formatTime(iso: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

export function timeAgo(iso: string): string {
  if (!iso) return "";
  const ms = Date.now() - new Date(iso).getTime();
  if (ms < 60_000) return "just now";
  const min = Math.floor(ms / 60_000);
  if (min < 60) return `${min}m ago`;
  const h = Math.floor(min / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

export function uptimeHuman(seconds: number): string {
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.floor(seconds % 60);
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}
