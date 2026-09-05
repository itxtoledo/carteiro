import { Link } from "react-router-dom";

import { pageCls, Card, CardHeader, EmptyState, ErrorBox, Spinner, StatCard, StatusBadge } from "../components/ui";
import { api } from "../lib/api";
import type { SendSummary, Stats } from "../lib/types";
import { timeAgo, uptimeHuman } from "../lib/types";
import { usePolling } from "../lib/usePolling";

function RecentTable({ rows }: { rows: SendSummary[] }) {
  return (
    <table className="w-full text-left text-sm">
      <thead>
        <tr className="border-b border-slate-200 text-xs uppercase tracking-wide text-slate-500 dark:border-slate-700 dark:text-slate-400">
          <th className="px-4 py-2 font-semibold">Subject</th>
          <th className="px-4 py-2 font-semibold">From</th>
          <th className="hidden px-4 py-2 font-semibold lg:table-cell">To</th>
          <th className="px-4 py-2 font-semibold">Status</th>
          <th className="px-4 py-2 text-right font-semibold">Age</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((s) => (
          <tr key={s.id} className="border-b border-slate-100 last:border-0 hover:bg-slate-50 dark:border-slate-800 dark:hover:bg-slate-800/50">
            <td className="max-w-0 px-4 py-2.5">
              <Link to={`/sends/${s.id}`} className="block truncate font-medium text-slate-800 hover:underline dark:text-slate-100">
                {s.subject || "(no subject)"}
              </Link>
              <div className="truncate font-mono text-[11px] text-slate-400">{s.id}</div>
            </td>
            <td className="truncate px-4 py-2.5 font-mono text-xs text-slate-600 dark:text-slate-300">{s.from}</td>
            <td className="hidden max-w-0 px-4 py-2.5 lg:table-cell">
              <div className="truncate font-mono text-xs text-slate-600 dark:text-slate-300">{s.to.join(", ")}</div>
            </td>
            <td className="px-4 py-2.5">
              <StatusBadge status={s.status} />
            </td>
            <td className="whitespace-nowrap px-4 py-2.5 text-right text-xs text-slate-500 dark:text-slate-400">
              {timeAgo(s.queued_at)}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export function DashboardPage() {
  const stats = usePolling<Stats>("stats", () => api.get<Stats>("/api/stats"), 5000);
  const recent = usePolling<SendSummary[]>("recent", () => api.get<SendSummary[]>("/api/sends?limit=8"), 5000);

  if (stats.error) return <ErrorBox message={stats.error} />;
  if (!stats.data) return <Spinner label="Loading metrics…" />;

  const c = stats.data.counters;
  const q = stats.data.queue;

  return (
    <div className={pageCls}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="text-base font-semibold text-slate-800 dark:text-slate-100">Relay overview</h2>
          <p className="text-xs text-slate-500 dark:text-slate-400">
            version {stats.data.version || "dev"} · up {uptimeHuman(stats.data.uptime_seconds)}
          </p>
        </div>
        <Link
          to="/compose"
          className="inline-flex items-center gap-1.5 rounded bg-[#0073bb] px-3 py-1.5 text-sm font-medium text-white hover:bg-[#096f93] dark:hover:bg-[#0b7fae]"
        >
          <svg className="size-4" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
            <path d="M12 8c-2.21 0-4 1.79-4 4s1.79 4 4 4 4-1.79 4-4-1.79-4-4-4zm8.94 3c.03-.33.06-.66.06-1s-.03-.67-.06-1l2.02-1.58c.18-.14.23-.38.12-.56l-1.89-3.28c-.12-.19-.36-.26-.56-.18l-2.38.96c-.5-.38-1.06-.71-1.66-.94L16.31 3c-.06-.22-.25-.38-.48-.38h-3.8c-.23 0-.42.16-.47.38l-.35 2.42c-.6.23-1.16.56-1.66.94l-2.38-.96c-.2-.08-.44-.01-.56.18l-1.89 3.28c-.12.19-.07.42.12.56L7.06 11c-.03.33-.06.66-.06 1s.03.67.06 1l-2.02 1.58c-.18.14-.23.38-.12.56l1.89 3.28c.12.19.36.26.56.18l2.38-.96c.5.38 1.06.71 1.66.94l.35 2.42c.06.22.25.38.48.38h3.8c.23 0 .42-.16.47-.38l.35-2.42c.6-.23 1.16-.56 1.66-.94l2.38.96c.2.08.44.01.56-.18l1.89-3.28c.12-.19.07-.42-.12-.56L20.94 11z" />
          </svg>
          Send e-mail
        </Link>
      </div>

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <StatCard label="Delivered" value={c.messages_delivered_total ?? 0} tone="ok" hint="total" />
        <StatCard label="Queued" value={q.queued} tone="accent" hint={`${q.due} due now`} />
        <StatCard label="Dead letter" value={q.dead} tone={q.dead > 0 ? "bad" : "default"} hint="awaiting review" />
        <StatCard label="Accepted" value={c.messages_queued_total ?? 0} hint="since start" />
        <StatCard label="Auth ok / fail" value={`${c.auth_success_total ?? 0} / ${c.auth_failure_total ?? 0}`} hint="SMTP logins" />
        <StatCard label="Attempts" value={c.delivery_attempts_total ?? 0} hint="toward MX" />
      </div>

      <Card>
        <CardHeader
          title="Recent activity"
          subtitle="Most recent messages (persistent history)"
          right={
            <Link to="/sends" className="text-xs font-medium text-[#0073bb] hover:underline dark:text-[#44b9d6]">
              View all →
            </Link>
          }
        />
        {recent.error ? (
          <div className="p-4">
            <ErrorBox message={recent.error} />
          </div>
        ) : !recent.data ? (
          <Spinner />
        ) : recent.data.length === 0 ? (
          <EmptyState>No messages yet. Send one from the SMTP port or with <span className="font-mono">Compose</span>.</EmptyState>
        ) : (
          <RecentTable rows={recent.data.slice(0, 8)} />
        )}
      </Card>
    </div>
  );
}
