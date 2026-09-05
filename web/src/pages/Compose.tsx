import { useState } from "react";
import { Link } from "react-router-dom";

import { pageCls, Card, CardHeader, ErrorBox, Spinner, btnGhost, btnPrimary, inputCls } from "../components/ui";
import { api } from "../lib/api";
import type { Account, SendResult } from "../lib/types";
import { usePolling } from "../lib/usePolling";

interface ComposeState {
  from: string;
  to: string;
  subject: string;
  text: string;
  html: string;
}

const empty: ComposeState = { from: "", to: "", subject: "", text: "", html: "" };

export function ComposePage() {
  const accounts = usePolling<Account[]>("accounts", () => api.get<Account[]>("/api/accounts"), 30_000);
  const [form, setForm] = useState<ComposeState>(empty);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [sent, setSent] = useState<SendResult | null>(null);

  const senders: string[] = [];
  for (const a of accounts.data ?? []) {
    for (const from of a.allowed_from) {
      if (!senders.includes(from)) senders.push(from);
    }
  }

  const set = (k: keyof ComposeState) => (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement>) =>
    setForm((f) => ({ ...f, [k]: e.target.value }));

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    setSent(null);
    const to = form.to
      .split(/[\n,;]+/)
      .map((s) => s.trim())
      .filter(Boolean);
    try {
      const res = await api.post<SendResult>("/api/send", {
        from: form.from,
        to,
        subject: form.subject,
        text: form.text,
        html: form.html,
      });
      setSent(res);
      setForm((f) => ({ ...f, subject: "", text: "", html: "", to: "" }));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const hasBody = form.text.trim() !== "" || form.html.trim() !== "";

  return (
    <div className={pageCls}>
      <Card>
        <CardHeader title="Compose message" subtitle="Delivered through the same queue as SMTP submissions (DKIM + retries included)" />
        {accounts.error ? (
          <div className="p-4">
            <ErrorBox message={accounts.error} />
          </div>
        ) : !accounts.data ? (
          <div className="p-4">
            <Spinner label="Loading sender accounts…" />
          </div>
        ) : (
          <form onSubmit={submit} className="space-y-4 p-4">
            <div className="grid gap-4 sm:grid-cols-2">
              <div>
                <label className="mb-1 block text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400" htmlFor="from">
                  From (envelope)
                </label>
                {senders.length > 0 ? (
                  <select id="from" className={inputCls} value={form.from} onChange={set("from")} required>
                    <option value="" disabled>
                      — pick a sender —
                    </option>
                    {senders.map((s) => (
                      <option key={s} value={s}>
                        {s}
                      </option>
                    ))}
                  </select>
                ) : (
                  <input
                    id="from"
                    className={inputCls}
                    placeholder="no accounts yet"
                    value={form.from}
                    onChange={set("from")}
                    disabled
                  />
                )}
                <p className="mt-1 text-[11px] text-slate-400">
                  An account e-mail or one of its <span className="font-mono">allowed_from</span> addresses.
                </p>
              </div>
              <div>
                <label className="mb-1 block text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400" htmlFor="to">
                  To (one per line, comma or semicolon)
                </label>
                <textarea
                  id="to"
                  rows={3}
                  className={inputCls}
                  placeholder={"client@example.com\nother@example.com"}
                  value={form.to}
                  onChange={set("to")}
                  required
                />
              </div>
            </div>

            <div>
              <label className="mb-1 block text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400" htmlFor="subject">
                Subject
              </label>
              <input id="subject" className={inputCls} value={form.subject} onChange={set("subject")} />
            </div>

            <div className="grid gap-4 md:grid-cols-2">
              <div>
                <label className="mb-1 block text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400" htmlFor="text">
                  Plain text body
                </label>
                <textarea
                  id="text"
                  rows={12}
                  className={`${inputCls} font-mono text-xs`}
                  value={form.text}
                  onChange={set("text")}
                  placeholder="The plain-text version…"
                />
              </div>
              <div>
                <label className="mb-1 block text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400" htmlFor="html">
                  HTML body
                </label>
                <textarea
                  id="html"
                  rows={12}
                  className={`${inputCls} font-mono text-xs`}
                  value={form.html}
                  onChange={set("html")}
                  placeholder="<html><body>…</body></html>"
                />
              </div>
            </div>

            {error ? <ErrorBox message={error} /> : null}

            {sent ? (
              <div className="rounded border border-emerald-300 bg-emerald-50 px-3 py-2 text-sm text-emerald-800 dark:border-emerald-800 dark:bg-emerald-950/40 dark:text-emerald-300">
                Queued as <code className="font-mono">{sent.id}</code> —{" "}
                <Link className="font-medium underline" to={`/sends/${sent.id}`}>
                  open the send
                </Link>{" "}
                to watch delivery.
              </div>
            ) : null}

            <div className="flex items-center justify-end gap-2 border-t border-slate-200 pt-4 dark:border-slate-700">
              <button
                type="button"
                className={btnGhost}
                onClick={() => {
                  setForm(empty);
                  setError(null);
                  setSent(null);
                }}
              >
                Clear
              </button>
              <button type="submit" className={btnPrimary} disabled={busy || !form.from || !hasBody}>
                {busy ? "Queueing…" : "Send message"}
              </button>
            </div>
          </form>
        )}
      </Card>
    </div>
  );
}
