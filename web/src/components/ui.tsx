import type { ReactNode } from "react";
import type { SendStatus } from "../lib/types";

export function Card({ children, className = "" }: { children: ReactNode; className?: string }) {
  return (
    <div
      className={`rounded border border-slate-300 bg-white shadow-sm dark:border-slate-700 dark:bg-[#1f242b] ${className}`}
    >
      {children}
    </div>
  );
}

export function CardHeader({ title, subtitle, right }: { title: string; subtitle?: string; right?: ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-4 border-b border-slate-200 px-4 py-3 dark:border-slate-700">
      <div>
        <h2 className="text-sm font-semibold text-slate-800 dark:text-slate-100">{title}</h2>
        {subtitle ? <p className="mt-0.5 text-xs text-slate-500 dark:text-slate-400">{subtitle}</p> : null}
      </div>
      {right}
    </div>
  );
}

export function StatCard({
  label,
  value,
  hint,
  tone = "default",
}: {
  label: string;
  value: string | number;
  hint?: string;
  tone?: "default" | "ok" | "warn" | "bad" | "accent";
}) {
  const tones: Record<string, string> = {
    default: "text-slate-800 dark:text-slate-100",
    ok: "text-emerald-700 dark:text-emerald-400",
    warn: "text-amber-700 dark:text-amber-400",
    bad: "text-red-700 dark:text-red-400",
    accent: "text-[#0073bb] dark:text-[#44b9d6]",
  };
  return (
    <Card className="px-4 py-3">
      <div className="text-xs font-medium uppercase tracking-wide text-slate-500 dark:text-slate-400">{label}</div>
      <div className={`mt-1 truncate font-mono text-2xl font-semibold tabular-nums ${tones[tone]}`}>{value}</div>
      {hint ? <div className="mt-0.5 text-xs text-slate-500 dark:text-slate-400">{hint}</div> : null}
    </Card>
  );
}

const badgeTones: Record<string, string> = {
  queued: "bg-sky-100 text-sky-800 dark:bg-sky-900/60 dark:text-sky-300",
  delivered: "bg-emerald-100 text-emerald-800 dark:bg-emerald-900/60 dark:text-emerald-300",
  dead: "bg-red-100 text-red-800 dark:bg-red-900/60 dark:text-red-300",
  neutral: "bg-slate-200 text-slate-700 dark:bg-slate-700 dark:text-slate-200",
};

export function StatusBadge({ status, label }: { status: SendStatus | string; label?: string }) {
  const tone = badgeTones[status] ?? badgeTones.neutral;
  return (
    <span
      className={`inline-flex items-center rounded-full px-2 py-0.5 text-[11px] font-semibold capitalize ${tone}`}
    >
      {label ?? status}
    </span>
  );
}

export function ErrorBox({ message }: { message: string }) {
  return (
    <div className="rounded border border-red-300 bg-red-50 px-3 py-2 text-sm text-red-800 dark:border-red-800 dark:bg-red-950/50 dark:text-red-300">
      {message}
    </div>
  );
}

export function Spinner({ label = "Loading…" }: { label?: string }) {
  return (
    <div className="flex items-center gap-2 px-1 py-6 text-sm text-slate-500 dark:text-slate-400">
      <span className="size-3.5 animate-spin rounded-full border-2 border-slate-300 border-t-[#0073bb] dark:border-slate-600 dark:border-t-[#44b9d6]" />
      {label}
    </div>
  );
}

export function EmptyState({ children }: { children: ReactNode }) {
  return (
    <div className="px-4 py-10 text-center text-sm text-slate-500 dark:text-slate-400">{children}</div>
  );
}

// Shared page container: every routed page uses the same content width so
// navigation does not "jump" between screens.
export const pageCls = "mx-auto w-full max-w-6xl space-y-4";

export const inputCls =
  "w-full rounded border border-slate-300 bg-white px-2.5 py-1.5 text-sm text-slate-800 placeholder:text-slate-400 focus:border-[#0073bb] focus:outline-none focus:ring-1 focus:ring-[#0073bb] dark:border-slate-600 dark:bg-[#14171c] dark:text-slate-100";

export const btnPrimary =
  "inline-flex items-center gap-1.5 rounded border border-transparent bg-[#0073bb] px-3 py-1.5 text-sm font-medium text-white hover:bg-[#096f93] focus:outline-none focus:ring-1 focus:ring-[#0073bb] disabled:cursor-not-allowed disabled:opacity-50 dark:hover:bg-[#0b7fae]";

export const btnGhost =
  "inline-flex items-center gap-1.5 rounded border border-slate-300 bg-white px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-50 focus:outline-none focus:ring-1 focus:ring-[#0073bb] dark:border-slate-600 dark:bg-transparent dark:text-slate-200 dark:hover:bg-slate-800";

export const btnDanger =
  "inline-flex items-center gap-1.5 rounded border border-red-300 bg-white px-3 py-1.5 text-sm font-medium text-red-700 hover:bg-red-50 focus:outline-none focus:ring-1 focus:ring-red-500 dark:border-red-800 dark:bg-transparent dark:text-red-400 dark:hover:bg-red-950/40";
