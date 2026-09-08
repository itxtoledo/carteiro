import { useState } from "react";
import { Link, useParams } from "react-router-dom";

import { pageCls, Card, ErrorBox, Spinner, StatusBadge, btnGhost, btnPrimary } from "../components/ui";
import { useRedact } from "../components/Redact";
import { api } from "../lib/api";
import type { SendDetail } from "../lib/types";
import { maskEmail, maskList, maskTextEmails, redactedToken } from "../lib/sensitive";
import { formatBytes, formatTime } from "../lib/types";
import { usePolling } from "../lib/usePolling";

type Tab = "preview" | "text" | "raw";

function MetaRow({ k, v }: { k: string; v: string }) {
  return (
    <div className="grid grid-cols-[140px_1fr] gap-2 py-1">
      <div className="text-xs font-semibold uppercase tracking-wide text-slate-400">{k}</div>
      <div className="break-all font-mono text-xs text-slate-700 dark:text-slate-200">{v}</div>
    </div>
  );
}

export function SendDetailPage() {
  const { id = "" } = useParams();
  const { data, error, loading, reload } = usePolling<SendDetail>(
    `send-${id}`,
    () => api.get<SendDetail>(`/api/sends/${encodeURIComponent(id)}`),
    3000,
  );
  const [tab, setTab] = useState<Tab>("preview");
  const [busy, setBusy] = useState(false);
  const { redact } = useRedact();

  const retry = async () => {
    setBusy(true);
    try {
      await api.post(`/api/queue/${id}/retry`);
      reload();
    } catch (err) {
      alert(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  if (error) {
    return (
      <div className={pageCls}>
        <ErrorBox message={error} />
      </div>
    );
  }
  if (!data || loading) {
    return (
      <div className={pageCls}>
        <Spinner label="Loading send…" />
      </div>
    );
  }

  const terminal = data.status === "delivered" || data.status === "dead";
  const viewFrom = redact ? maskEmail(data.from) : data.from;
  const viewTo = redact ? maskList(data.to).join(", ") : data.to.join(", ");
  const viewSubject = redact ? redactedToken : data.subject || "(no subject)";
  const viewLastError = data.last_error ? (redact ? maskTextEmails(data.last_error) : data.last_error) : undefined;
  const tabCls = (t: Tab) =>
    `rounded px-3 py-1.5 text-sm font-medium ${
      tab === t
        ? "bg-[#e3f0f7] text-[#0a4f74] dark:bg-[#1e3a4a] dark:text-[#8fd0f0]"
        : "text-slate-600 hover:bg-slate-100 dark:text-slate-300 dark:hover:bg-slate-800"
    }`;

  return (
    <div className={pageCls}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <Link to="/sends" className="text-xs font-medium text-[#0073bb] hover:underline dark:text-[#44b9d6]">
            ← Back to sends
          </Link>
          <h2 className="mt-1 text-base font-semibold text-slate-800 dark:text-slate-100">
            {viewSubject}
          </h2>
        </div>
        <div className="flex items-center gap-2">
          <StatusBadge status={data.status} />
          {data.status === "dead" ? (
            <button type="button" className={btnPrimary} onClick={retry} disabled={busy}>
              {busy ? "Retrying…" : "Retry delivery"}
            </button>
          ) : null}
          <button type="button" className={btnGhost} onClick={reload} disabled={!terminal}>
            Refresh
          </button>
        </div>
      </div>

      <Card>
        <div className="grid gap-1 px-4 py-3 sm:grid-cols-2">
          <MetaRow k="Message-ID" v={data.id} />
          <MetaRow k="From" v={viewFrom} />
          <MetaRow k="To" v={viewTo} />
          <MetaRow k="Size" v={`${formatBytes(data.size)}${data.truncated ? " (body truncated for the feed)" : ""}`} />
          <MetaRow k="Queued at" v={formatTime(data.queued_at)} />
          <MetaRow k="Updated at" v={formatTime(data.updated_at)} />
          <MetaRow k="Attempts" v={String(data.attempts)} />
          {viewLastError ? <MetaRow k="Last error" v={viewLastError} /> : null}
        </div>
      </Card>

      <Card>
        <div className="flex items-center justify-between gap-2 border-b border-slate-200 px-4 py-2 dark:border-slate-700">
          <div className="flex gap-1 rounded border border-slate-200 p-0.5 dark:border-slate-700">
            <button type="button" className={tabCls("preview")} onClick={() => setTab("preview")} disabled={!data.html}>
              Rendered
            </button>
            <button type="button" className={tabCls("text")} onClick={() => setTab("text")}>
              Plain text
            </button>
            <button type="button" className={tabCls("raw")} onClick={() => setTab("raw")}>
              Source
            </button>
          </div>
          <span className="text-xs text-slate-400">
            {data.truncated ? "truncated preview" : `${formatBytes(data.size)}`}
          </span>
        </div>

        {redact ? (
          <div className="p-6 text-center text-sm text-slate-500 dark:text-slate-400">
            Sensitive content hidden (screenshot mode). Disable the eye icon to view this message.
          </div>
        ) : tab === "preview" && data.html ? (
          <iframe
            title="E-mail preview"
            sandbox=""
            srcDoc={`<!doctype html><html><head><meta charset="utf-8"></head><body style="font-family:system-ui,sans-serif;padding:16px">${data.html}</body></html>`}
            className="h-[520px] w-full bg-white dark:bg-white"
          />
        ) : tab === "preview" ? (
          <div className="p-4 text-sm text-slate-500 dark:text-slate-400">
            This message has no HTML body. Use the Plain text tab.
          </div>
        ) : tab === "text" ? (
          <pre className="max-h-[520px] overflow-auto whitespace-pre-wrap break-words px-4 py-3 font-mono text-xs text-slate-700 dark:bg-[#14171c] dark:text-slate-200">
            {data.text || data.html || "(no readable body)"}
          </pre>
        ) : (
          <pre className="max-h-[520px] overflow-auto whitespace-pre-wrap break-words px-4 py-3 font-mono text-xs text-slate-700 dark:bg-[#14171c] dark:text-slate-200">
            {data.raw || "(no raw content recorded)"}
          </pre>
        )}
      </Card>
    </div>
  );
}
