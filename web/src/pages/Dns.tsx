import { useEffect, useState } from "react";

import {
  pageCls,
  Card,
  CardHeader,
  EmptyState,
  ErrorBox,
  Spinner,
  btnPrimary,
  inputCls,
} from "../components/ui";
import { api } from "../lib/api";
import type { DKIMKey, DNSCheck, DNSReport } from "../lib/types";
import { usePolling } from "../lib/usePolling";

const pillTones: Record<string, string> = {
  pass: "bg-emerald-100 text-emerald-800 dark:bg-emerald-900/60 dark:text-emerald-300",
  warn: "bg-amber-100 text-amber-800 dark:bg-amber-900/60 dark:text-amber-300",
  fail: "bg-red-100 text-red-800 dark:bg-red-900/60 dark:text-red-300",
  info: "bg-slate-200 text-slate-700 dark:bg-slate-700 dark:text-slate-200",
};

const statusLabel: Record<string, string> = {
  pass: "Pass",
  warn: "Warning",
  fail: "Fail",
  info: "Info",
};

function StatusPill({ status }: { status: string }) {
  const tone = pillTones[status] ?? pillTones.info;
  return (
    <span className={`inline-flex items-center rounded-full px-2 py-0.5 text-[11px] font-semibold ${tone}`}>
      {statusLabel[status] ?? status}
    </span>
  );
}

function Copyable({ value }: { value: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* clipboard unavailable; the value is still selectable */
    }
  };
  return (
    <div className="flex items-start gap-2">
      <code className="min-w-0 flex-1 break-all rounded bg-slate-100 px-2 py-1 font-mono text-[11px] text-slate-700 dark:bg-[#14171c] dark:text-slate-200">
        {value}
      </code>
      <button
        type="button"
        onClick={copy}
        className="shrink-0 rounded border border-slate-300 px-2 py-1 text-[11px] text-slate-600 hover:bg-slate-50 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800"
      >
        {copied ? "Copied" : "Copy"}
      </button>
    </div>
  );
}

function CheckRow({ check }: { check: DNSCheck }) {
  return (
    <div className="border-b border-slate-100 px-4 py-3 last:border-0 dark:border-slate-800">
      <div className="flex items-center justify-between gap-3">
        <span className="text-sm font-medium text-slate-800 dark:text-slate-100">{check.label}</span>
        <StatusPill status={check.status} />
      </div>
      <p className="mt-1 text-xs text-slate-600 dark:text-slate-300">{check.message}</p>
      {check.found && check.found.length > 0 ? (
        <div className="mt-2 space-y-1">
          <div className="text-[11px] font-semibold uppercase tracking-wide text-slate-400">Found</div>
          {check.found.map((rec) => (
            <Copyable key={rec} value={rec} />
          ))}
        </div>
      ) : null}
      {check.expected ? (
        <div className="mt-2 space-y-1">
          <div className="text-[11px] font-semibold uppercase tracking-wide text-slate-400">Publish this record</div>
          <Copyable value={check.expected} />
        </div>
      ) : null}
    </div>
  );
}

export function DnsPage() {
  const keys = usePolling<DKIMKey[]>("dkim", () => api.get<DKIMKey[]>("/api/dkim"), 60_000);
  const [domain, setDomain] = useState("");
  const [selector, setSelector] = useState("");
  const [report, setReport] = useState<DNSReport | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Pre-fill with the first configured sending domain once the keys load.
  useEffect(() => {
    if (domain || !keys.data || keys.data.length === 0) return;
    setDomain(keys.data[0].domain);
    setSelector(keys.data[0].selector);
  }, [keys.data, domain]);

  const pickDomain = (value: string) => {
    setDomain(value);
    const match = keys.data?.find((k) => k.domain.toLowerCase() === value.trim().toLowerCase());
    if (match) setSelector(match.selector);
  };

  const run = async (e: React.FormEvent) => {
    e.preventDefault();
    const d = domain.trim().toLowerCase();
    if (!d) return;
    setBusy(true);
    setError(null);
    try {
      const q = new URLSearchParams({ domain: d });
      if (selector.trim()) q.set("selector", selector.trim());
      setReport(await api.get<DNSReport>(`/api/dns?${q.toString()}`));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setReport(null);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className={pageCls}>
      <div>
        <h2 className="text-base font-semibold text-slate-800 dark:text-slate-100">Sender / DNS check</h2>
        <p className="text-xs text-slate-500 dark:text-slate-400">
          Inspect the public DNS records (SPF, DKIM, DMARC, MX) of a sending domain and see whether they are set up so
          your mail is trusted instead of flagged as spam.
        </p>
      </div>

      <Card>
        <CardHeader title="Domain to analyse" subtitle="Pick a configured domain or type any sender domain" />
        <form onSubmit={run} className="flex flex-col gap-3 p-4 sm:flex-row sm:items-end">
          <div className="flex-1">
            <label className="mb-1 block text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400" htmlFor="dns-domain">
              Domain
            </label>
            <input
              id="dns-domain"
              className={inputCls}
              list="dns-domains"
              placeholder="example.com"
              value={domain}
              onChange={(e) => pickDomain(e.target.value)}
            />
            <datalist id="dns-domains">
              {keys.data?.map((k) => (
                <option key={k.domain} value={k.domain} />
              ))}
            </datalist>
          </div>
          <div className="sm:w-48">
            <label className="mb-1 block text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400" htmlFor="dns-selector">
              Selector <span className="font-normal normal-case text-slate-400">(optional)</span>
            </label>
            <input
              id="dns-selector"
              className={inputCls}
              placeholder="auto"
              value={selector}
              onChange={(e) => setSelector(e.target.value)}
            />
          </div>
          <button type="submit" className={btnPrimary} disabled={busy || !domain.trim()}>
            {busy ? "Checking…" : "Run check"}
          </button>
        </form>
      </Card>

      {error ? <ErrorBox message={error} /> : null}
      {busy && !report ? <Spinner label="Querying DNS…" /> : null}

      {report ? (
        <>
          <div
            className={`rounded border px-4 py-3 text-sm ${
              report.ok
                ? "border-emerald-300 bg-emerald-50 text-emerald-800 dark:border-emerald-800 dark:bg-emerald-950/40 dark:text-emerald-300"
                : "border-red-300 bg-red-50 text-red-800 dark:border-red-800 dark:bg-red-950/40 dark:text-red-300"
            }`}
          >
            <div className="font-medium">
              {report.ok ? "This domain looks ready to send." : "Some records need attention before sending."}
            </div>
            <div className="mt-1 text-xs">
              {report.domain}
              {report.selector ? ` · selector ${report.selector}` : ""}
              {report.hostname ? ` · hostname ${report.hostname}` : ""} · {report.passed} passed ·{" "}
              {report.warnings} warnings · {report.failed} failed
            </div>
          </div>

          <Card>
            <CardHeader title="Records" subtitle="Fix every failure and warning to maximize deliverability" />
            {report.checks.length === 0 ? (
              <EmptyState>No checks were run.</EmptyState>
            ) : (
              <div>
                {report.checks.map((c) => (
                  <CheckRow key={c.name} check={c} />
                ))}
              </div>
            )}
          </Card>
        </>
      ) : null}
    </div>
  );
}
