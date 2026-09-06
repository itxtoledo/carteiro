import { createContext, useCallback, useContext, useState } from "react";

const KEY = "carteiro.redact";

interface RedactState {
  redact: boolean;
  toggle: () => void;
}

const RedactContext = createContext<RedactState>({ redact: false, toggle: () => {} });

/** Persists the "hide sensitive data" flag across sessions. */
export function RedactProvider({ children }: { children: React.ReactNode }) {
  const [redact, setRedact] = useState<boolean>(() => localStorage.getItem(KEY) === "true");
  const toggle = useCallback(() => {
    setRedact((r) => {
      const next = !r;
      localStorage.setItem(KEY, String(next));
      return next;
    });
  }, []);
  return <RedactContext.Provider value={{ redact, toggle }}>{children}</RedactContext.Provider>;
}

export function useRedact(): RedactState {
  return useContext(RedactContext);
}

/** Eye toggle used in the top bar to enable screenshot mode. */
export function RedactToggle() {
  const { redact, toggle } = useRedact();
  return (
    <button
      type="button"
      onClick={toggle}
      aria-pressed={redact}
      title={
        redact
          ? "Screenshot mode on: sensitive data is hidden. Click to show it."
          : "Screenshot mode off. Click to hide sensitive data."
      }
      className={`rounded border p-1.5 ${
        redact
          ? "border-amber-400 bg-amber-50 text-amber-700 dark:border-amber-500 dark:bg-amber-500/10 dark:text-amber-300"
          : "border-slate-300 bg-white text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:bg-transparent dark:text-slate-300 dark:hover:bg-slate-800"
      }`}
    >
      {redact ? (
        <svg className="size-4" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
          <path d="M12 7c2.76 0 5 2.24 5 5 0 .65-.13 1.26-.36 1.83l2.92 2.92c1.51-1.26 2.7-2.89 3.43-4.75-1.73-4.39-6-7.5-11-7.5-1.4 0-2.74.25-3.98.7l2.16 2.16C10.74 7.13 11.35 7 12 7zM2 4.27l2.28 2.28.46.46C3.08 8.3 1.78 10.02 1 12c1.73 4.39 6 7.5 11 7.5 1.55 0 3.03-.3 4.38-.84l.42.42L19.73 22 21 20.73 3.27 3 2 4.27zM7.53 9.8l1.55 1.55c-.05.21-.08.43-.08.65 0 1.66 1.34 3 3 3 .22 0 .44-.03.65-.08l1.55 1.55c-.67.33-1.41.53-2.2.53-2.76 0-5-2.24-5-5 0-.79.2-1.53.53-2.2zm4.31-.78 3.15 3.15.02-.16c0-1.66-1.34-3-3-3l-.17.01z" />
        </svg>
      ) : (
        <svg className="size-4" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
          <path d="M12 4.5C7 4.5 2.73 7.61 1 12c1.73 4.39 6 7.5 11 7.5s9.27-3.11 11-7.5c-1.73-4.39-6-7.5-11-7.5zM12 17c-2.76 0-5-2.24-5-5s2.24-5 5-5 5 2.24 5 5-2.24 5-5 5zm0-8c-1.66 0-3 1.34-3 3s1.34 3 3 3 3-1.34 3-3-1.34-3-3-3z" />
        </svg>
      )}
    </button>
  );
}
