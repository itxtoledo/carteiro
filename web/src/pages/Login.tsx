import { useState } from "react";
import { Navigate, useNavigate } from "react-router-dom";

import { ErrorBox, btnPrimary, inputCls } from "../components/ui";
import { checkToken, getToken, setToken } from "../lib/api";

export function LoginPage() {
  const navigate = useNavigate();
  const [token, setTokenValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (getToken()) return <Navigate to="/" replace />;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!token.trim()) return;
    setBusy(true);
    setError(null);
    try {
      // A stats round-trip validates the token against a real endpoint.
      await checkToken(token);
      setToken(token.trim());
      navigate("/", { replace: true });
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="grid min-h-screen place-items-center bg-[#f2f3f3] px-4 dark:bg-[#101317]">
      <div className="w-full max-w-sm">
        <div className="mb-6 flex flex-col items-center gap-2 text-center">
          <span className="grid size-12 place-items-center rounded bg-[#0073bb] text-white">
            <svg className="size-6" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
              <path d="M20 4H4c-1.1 0-2 .9-2 2v12c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2V6c0-1.1-.9-2-2-2zm0 4l-8 5-8-5V6l8 5 8-5v2z" />
            </svg>
          </span>
          <div>
            <h1 className="text-lg font-semibold text-slate-800 dark:text-white">Carteiro</h1>
            <p className="text-sm text-slate-500 dark:text-slate-400">SMTP relay console</p>
          </div>
        </div>

        <form
          onSubmit={submit}
          className="rounded border border-slate-300 bg-white p-5 shadow-sm dark:border-slate-700 dark:bg-[#1f242b]"
        >
          <label className="block text-sm font-medium text-slate-700 dark:text-slate-200" htmlFor="token">
            API token
          </label>
          <input
            id="token"
            type="password"
            autoComplete="current-password"
            className={`${inputCls} mt-1.5 font-mono`}
            placeholder="CARTEIRO_API_TOKEN"
            value={token}
            onChange={(e) => setTokenValue(e.target.value)}
            autoFocus
          />
          <p className="mt-2 text-xs text-slate-500 dark:text-slate-400">
            The value of <code className="font-mono">CARTEIRO_API_TOKEN</code> configured on the server.
          </p>
          {error ? (
            <div className="mt-3">
              <ErrorBox message={error} />
            </div>
          ) : null}
          <button type="submit" disabled={busy || !token.trim()} className={`${btnPrimary} mt-4 w-full justify-center`}>
            {busy ? "Signing in…" : "Sign in"}
          </button>
        </form>
      </div>
    </div>
  );
}
