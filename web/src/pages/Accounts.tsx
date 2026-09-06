import { useState } from "react";

import { pageCls, Card, CardHeader, EmptyState, ErrorBox, Spinner, btnDanger, btnGhost, btnPrimary, inputCls } from "../components/ui";
import { useRedact } from "../components/Redact";
import { api } from "../lib/api";
import type { Account } from "../lib/types";
import { maskEmail } from "../lib/sensitive";
import { usePolling } from "../lib/usePolling";

interface PatchResult {
  email: string;
  updated: boolean;
  allowed_from: string[];
  password_changed: boolean;
}

/** Small modal used by the account editor. */
function Modal({ title, onClose, children }: { title: string; onClose: () => void; children: React.ReactNode }) {
  return (
    <div
      className="fixed inset-0 z-40 grid place-items-center bg-black/40 p-4"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div className="w-full max-w-lg rounded border border-slate-300 bg-white shadow-xl dark:border-slate-700 dark:bg-[#1f242b]">
        <div className="flex items-center justify-between border-b border-slate-200 px-4 py-3 dark:border-slate-700">
          <h3 className="text-sm font-semibold text-slate-800 dark:text-slate-100">{title}</h3>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close"
            className="rounded p-1 text-slate-400 hover:bg-slate-100 hover:text-slate-700 dark:hover:bg-slate-800 dark:hover:text-slate-200"
          >
            <svg className="size-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" aria-hidden="true">
              <path d="M18 6 6 18M6 6l12 12" strokeLinecap="round" />
            </svg>
          </button>
        </div>
        <div className="p-4">{children}</div>
      </div>
    </div>
  );
}

function Chip({ value, onRemove }: { value: string; onRemove?: () => void }) {
  return (
    <span className="inline-flex items-center gap-1 rounded bg-slate-100 px-1.5 py-0.5 font-mono text-[11px] text-slate-600 dark:bg-slate-800 dark:text-slate-300">
      {value}
      {onRemove ? (
        <button
          type="button"
          onClick={onRemove}
          aria-label={`Remove ${value}`}
          className="rounded-full text-slate-400 hover:text-red-600 dark:hover:text-red-400"
        >
          <svg className="size-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" aria-hidden="true">
            <path d="M18 6 6 18M6 6l12 12" strokeLinecap="round" />
          </svg>
        </button>
      ) : (
        <svg className="size-3 text-slate-400" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
          <path d="M9 16.2 4.8 12l-1.4 1.4L9 19 21 7l-1.4-1.4z" />
        </svg>
      )}
    </span>
  );
}

function EditAccountModal({
  account,
  onClose,
  onSaved,
  onError,
}: {
  account: Account;
  onClose: () => void;
  onSaved: (msg: string) => void;
  onError: (msg: string) => void;
}) {
  // The account email itself is always allowed; only the extras are editable.
  const [extras, setExtras] = useState<string[]>(account.allowed_from.filter((f) => f !== account.email));
  const [draft, setDraft] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);

  const add = (raw?: string) => {
    const v = (raw ?? draft).trim().toLowerCase();
    if (!v || !v.includes("@") || extras.includes(v)) {
      setDraft("");
      return;
    }
    setExtras((xs) => [...xs, v]);
    setDraft("");
  };

  const save = async () => {
    setBusy(true);
    try {
      const body: { allowed_from: string[]; password?: string } = { allowed_from: extras };
      if (password.trim()) body.password = password;
      const res = await api.patch<PatchResult>(`/api/accounts/${encodeURIComponent(account.email)}`, body);
      onSaved(
        `${res.email} updated: allowed senders ${res.allowed_from.length > 1 ? "updated" : "reset to the account email only"}${
          res.password_changed ? " and password changed" : ""
        }.`,
      );
      onClose();
    } catch (err) {
      onError(err instanceof Error ? err.message : String(err));
      setBusy(false);
    }
  };

  return (
    <Modal title={`Edit account — ${account.email}`} onClose={onClose}>
      <div className="space-y-4">
        <div>
          <p className="mb-1.5 text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">
            Allowed senders (MAIL FROM the account may use)
          </p>
          <div className="flex flex-wrap items-center gap-1.5 rounded border border-slate-200 bg-slate-50 p-2 dark:border-slate-600 dark:bg-[#14171c]">
            <Chip value={account.email} />
            {extras.map((f) => (
              <Chip key={f} value={f} onRemove={() => setExtras((xs) => xs.filter((x) => x !== f))} />
            ))}
          </div>
          <div className="mt-2 flex gap-2">
            <input
              type="email"
              className={inputCls}
              placeholder="Add another sender, e.g. news@example.com"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  add();
                }
              }}
            />
            <button type="button" className={btnGhost} onClick={() => add()} disabled={!draft.trim()}>
              Add
            </button>
          </div>
          <p className="mt-1.5 text-[11px] text-slate-400">
            The account e-mail is always allowed. Empty list = only the login address can send.
          </p>
        </div>

        <div>
          <label className="mb-1 block text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400" htmlFor="new-pass">
            New password <span className="font-normal normal-case text-slate-400">(optional — leave blank to keep)</span>
          </label>
          <input
            id="new-pass"
            type="password"
            autoComplete="new-password"
            className={inputCls}
            placeholder="••••••••"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </div>

        <div className="flex justify-end gap-2 border-t border-slate-200 pt-3 dark:border-slate-700">
          <button type="button" className={btnGhost} onClick={onClose}>
            Cancel
          </button>
          <button type="button" className={btnPrimary} onClick={save} disabled={busy}>
            {busy ? "Saving…" : "Save changes"}
          </button>
        </div>
      </div>
    </Modal>
  );
}

export function AccountsPage() {
  const { data, error, loading, reload } = usePolling<Account[]>("accounts", () => api.get<Account[]>("/api/accounts"), 10_000);
  const { redact } = useRedact();
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [editing, setEditing] = useState<Account | null>(null);

  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [allowed, setAllowed] = useState("");

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setFormError(null);
    setNotice(null);
    const allowedFrom = allowed
      .split(/[\n,;]+/)
      .map((s) => s.trim().toLowerCase())
      .filter(Boolean);
    try {
      const res = await api.post<{ email: string; created: boolean }>("/api/accounts", {
        email,
        password,
        allowed_from: allowedFrom,
      });
      setNotice(
        res.created
          ? `${res.email} created (password stored as a bcrypt hash).`
          : `${res.email} updated (password/allowed senders replaced).`,
      );
      setEmail("");
      setPassword("");
      setAllowed("");
      reload();
    } catch (err) {
      setFormError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (a: string) => {
    if (!window.confirm(`Delete the account ${a}?\nSMTP logins with it stop working immediately.`)) return;
    try {
      await api.del(`/api/accounts/${encodeURIComponent(a)}`);
      setNotice(`Account ${a} deleted.`);
      reload();
    } catch (err) {
      setNotice(null);
      setFormError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div className={pageCls}>
      <Card>
        <CardHeader
          title="Add account"
          subtitle="SMTP login = account e-mail + password (bcrypt, never stored or returned in plain text)"
        />
        <form onSubmit={submit} className="grid gap-3 p-4 sm:grid-cols-[1fr_1fr_1.4fr_auto] sm:items-end">
          <div>
            <label className="mb-1 block text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400" htmlFor="email">
              E-mail (login)
            </label>
            <input id="email" type="email" required className={inputCls} placeholder="project@example.com" value={email} onChange={(e) => setEmail(e.target.value)} />
          </div>
          <div>
            <label className="mb-1 block text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400" htmlFor="password">
              Password
            </label>
            <input id="password" type="password" required className={inputCls} placeholder="min 1 char" value={password} onChange={(e) => setPassword(e.target.value)} />
          </div>
          <div>
            <label className="mb-1 block text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400" htmlFor="allowed">
              Allowed senders (optional, comma separated)
            </label>
            <input id="allowed" className={inputCls} placeholder="news@example.com, team@example.com" value={allowed} onChange={(e) => setAllowed(e.target.value)} />
          </div>
          <button type="submit" className={btnPrimary} disabled={busy || !email || !password}>
            {busy ? "Saving…" : "Add account"}
          </button>
        </form>
        {formError ? (
          <div className="px-4 pb-4">
            <ErrorBox message={formError} />
          </div>
        ) : null}
        {notice ? <div className="px-4 pb-4 text-sm text-emerald-700 dark:text-emerald-400">{notice}</div> : null}
      </Card>

      <Card>
        <CardHeader title={`Accounts (${data?.length ?? 0})`} subtitle="Edit the allowed senders or reset a password without knowing the old one" />
        {error ? (
          <div className="p-4">
            <ErrorBox message={error} />
          </div>
        ) : loading && !data ? (
          <Spinner label="Loading accounts…" />
        ) : !data || data.length === 0 ? (
          <EmptyState>
            No accounts yet. Create the first one above or seed accounts via <code className="font-mono">CARTEIRO_ACCOUNTS</code>.
          </EmptyState>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead>
                <tr className="border-b border-slate-200 text-xs uppercase tracking-wide text-slate-500 dark:border-slate-700 dark:text-slate-400">
                  <th className="px-4 py-2 font-semibold">Login (e-mail)</th>
                  <th className="px-4 py-2 font-semibold">Allowed senders</th>
                  <th className="w-40 px-4 py-2 text-right font-semibold">Actions</th>
                </tr>
              </thead>
              <tbody>
                {data.map((a) => (
                  <tr key={redact ? maskEmail(a.email) : a.email} className="border-b border-slate-100 last:border-0 hover:bg-slate-50 dark:border-slate-800 dark:hover:bg-slate-800/50">
                    <td className="px-4 py-2.5 font-mono text-xs text-slate-800 dark:text-slate-100">{redact ? maskEmail(a.email) : a.email}</td>
                    <td className="px-4 py-2.5">
                      <div className="flex flex-wrap gap-1">
                        {a.allowed_from.map((f) => (
                          <Chip key={f} value={redact ? maskEmail(f) : f} />
                        ))}
                      </div>
                    </td>
                    <td className="px-4 py-2.5 text-right">
                      <div className="flex justify-end gap-2">
                        <button type="button" className={btnGhost} onClick={() => setEditing(a)}>
                          Edit
                        </button>
                        <button type="button" className={btnDanger} onClick={() => remove(a.email)}>
                          Delete
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {editing ? (
        <EditAccountModal
          account={editing}
          onClose={() => setEditing(null)}
          onSaved={(msg) => {
            setNotice(msg);
            setFormError(null);
            reload();
          }}
          onError={(msg) => {
            setNotice(null);
            setFormError(msg);
          }}
        />
      ) : null}
    </div>
  );
}
