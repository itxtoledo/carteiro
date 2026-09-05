import { useState } from "react";
import { NavLink, Outlet, useLocation } from "react-router-dom";

import { clearSession, saveTheme } from "../lib/api";

type Theme = "dark" | "light";

const nav = [
  { to: "/", label: "Dashboard", icon: "M3 13h8V3H3v10zm0 8h8v-6H3v6zm10 0h8V11h-8v10zm0-18v6h8V3h-8z" },
  { to: "/compose", label: "Compose", icon: "M20 4H4c-1.1 0-1.99.9-1.99 2L2 18c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2V6c0-1.1-.9-2-2-2zm0 4l-8 5-8-5V6l8 5 8-5v2z" },
  { to: "/sends", label: "Sends", icon: "M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm-2 15l-5-5 1.41-1.41L10 14.17l7.59-7.59L19 8l-9 9z" },
  { to: "/accounts", label: "Accounts", icon: "M16 11c1.66 0 2.99-1.34 2.99-3S17.66 5 16 5s-3 1.34-3 3 1.34 3 3 3zm-8 0c1.66 0 2.99-1.34 2.99-3S9.66 5 8 5 5 6.34 5 8s1.34 3 3 3zm0 2c-2.33 0-7 1.17-7 3.5V19h14v-2.5c0-2.33-4.67-3.5-7-3.5zm8 0c-.29 0-.62.02-.97.05 1.16.84 1.97 1.97 1.97 3.45V19h6v-2.5c0-2.33-4.67-3.5-7-3.5z" },
];

const titles: Record<string, string> = {
  "/": "Dashboard",
  "/compose": "Compose message",
  "/sends": "Sends",
  "/accounts": "Accounts",
};

function ThemeToggle({ theme, onChange }: { theme: Theme; onChange: (t: Theme) => void }) {
  return (
    <button
      type="button"
      onClick={() => {
        const next = theme === "dark" ? "light" : "dark";
        saveTheme(next);
        onChange(next);
      }}
      title={theme === "dark" ? "Switch to light mode" : "Switch to dark mode"}
      className="rounded border border-slate-300 bg-white p-1.5 text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:bg-transparent dark:text-slate-300 dark:hover:bg-slate-800"
    >
      {theme === "dark" ? (
        <svg className="size-4" viewBox="0 0 24 24" fill="currentColor">
          <path d="M12 7c-2.76 0-5 2.24-5 5s2.24 5 5 5 5-2.24 5-5-2.24-5-5-5zM2 13h2c.55 0 1-.45 1-1s-.45-1-1-1H2c-.55 0-1 .45-1 1s.45 1 1 1zm18 0h2c.55 0 1-.45 1-1s-.45-1-1-1h-2c-.55 0-1 .45-1 1s.45 1 1 1zM11 2v2c0 .55.45 1 1 1s1-.45 1-1V2c0-.55-.45-1-1-1s-1 .45-1 1zm0 18v2c0 .55.45 1 1 1s1-.45 1-1v-2c0-.55-.45-1-1-1s-1 .45-1 1zM5.99 4.58c-.39-.39-1.03-.39-1.41 0-.39.39-.39 1.03 0 1.41l1.06 1.06c.39.39 1.03.39 1.41 0s.39-1.03 0-1.41L5.99 4.58zm12.37 12.37c-.39-.39-1.03-.39-1.41 0-.39.39-.39 1.03 0 1.41l1.06 1.06c.39.39 1.03.39 1.41 0 .39-.39.39-1.03 0-1.41l-1.06-1.06zm1.06-10.96c.39-.39.39-1.03 0-1.41-.39-.39-1.03-.39-1.41 0l-1.06 1.06c-.39.39-.39 1.03 0 1.41s1.03.39 1.41 0l1.06-1.06zM7.05 18.36c.39-.39.39-1.03 0-1.41-.39-.39-1.03-.39-1.41 0l-1.06 1.06c-.39.39-.39 1.03 0 1.41s1.03.39 1.41 0l1.06-1.06z" />
        </svg>
      ) : (
        <svg className="size-4" viewBox="0 0 24 24" fill="currentColor">
          <path d="M12 3c-4.97 0-9 4.03-9 9s4.03 9 9 9 9-4.03 9-9c0-.46-.04-.92-.1-1.36-.98 1.37-2.58 2.26-4.4 2.26-2.98 0-5.4-2.42-5.4-5.4 0-1.81.89-3.42 2.26-4.4-.44-.06-.9-.1-1.36-.1z" />
        </svg>
      )}
    </button>
  );
}

function SidebarIcon({ path }: { path: string }) {
  return (
    <svg className="size-4 shrink-0" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
      <path d={path} />
    </svg>
  );
}

export function Layout() {
  const location = useLocation();
  const [theme, setTheme] = useState<Theme>(
    (localStorage.getItem("carteiro.theme") as Theme | null) ?? "dark",
  );
  const title = titles[location.pathname] ?? "Carteiro";
  const inSendDetail = /^\/sends\/.+/.test(location.pathname);

  return (
    <div className="flex h-full">
      {/* Console sidebar */}
      <aside className="hidden w-56 shrink-0 flex-col border-r border-slate-300 bg-white md:flex dark:border-slate-800 dark:bg-[#14171c]">
        <div className="flex items-center gap-2 border-b border-slate-200 px-4 py-3.5 dark:border-slate-800">
          <span className="grid size-8 place-items-center rounded bg-[#0073bb] text-white">
            <svg className="size-4.5" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
              <path d="M20 4H4c-1.1 0-2 .9-2 2v12c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2V6c0-1.1-.9-2-2-2zm0 4l-8 5-8-5V6l8 5 8-5v2z" />
            </svg>
          </span>
          <div className="leading-tight">
            <div className="text-sm font-semibold text-slate-800 dark:text-white">Carteiro</div>
            <div className="text-[11px] text-slate-500 dark:text-slate-400">SMTP relay console</div>
          </div>
        </div>
        <nav className="flex-1 space-y-0.5 px-2 py-3">
          {nav.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.to === "/"}
              className={({ isActive }) =>
                `flex items-center gap-2.5 rounded px-2.5 py-2 text-sm ${
                  isActive
                    ? "bg-[#e3f0f7] font-medium text-[#0a4f74] dark:bg-[#1e3a4a] dark:text-[#8fd0f0]"
                    : "text-slate-600 hover:bg-slate-100 hover:text-slate-900 dark:text-slate-300 dark:hover:bg-slate-800 dark:hover:text-white"
                }`
              }
            >
              <SidebarIcon path={item.icon} />
              {item.label}
            </NavLink>
          ))}
        </nav>
        <div className="border-t border-slate-200 px-4 py-3 text-[11px] text-slate-400 dark:border-slate-800 dark:text-slate-500">
          SMTP :587 · web :8080
        </div>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        {/* Top bar */}
        <header className="flex items-center justify-between gap-3 border-b border-slate-300 bg-white px-4 py-2.5 dark:border-slate-800 dark:bg-[#1f242b]">
          <div className="flex items-center gap-2 min-w-0">
            <span className="text-[#0073bb] md:hidden dark:text-[#44b9d6]">
              <svg className="size-5" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
                <path d="M20 4H4c-1.1 0-2 .9-2 2v12c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2V6c0-1.1-.9-2-2-2zm0 4l-8 5-8-5V6l8 5 8-5v2z" />
              </svg>
            </span>
            <h1 className="truncate text-[15px] font-semibold text-slate-800 dark:text-slate-100">{title}</h1>
            {inSendDetail ? (
              <span className="ml-1 hidden truncate text-xs text-slate-400 sm:inline">{location.pathname.slice(7)}</span>
            ) : null}
          </div>
          <div className="flex items-center gap-2">
            <ThemeToggle theme={theme} onChange={setTheme} />
            <button
              type="button"
              onClick={() => clearSession()}
              className="rounded border border-slate-300 bg-white px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-50 dark:border-slate-600 dark:bg-transparent dark:text-slate-200 dark:hover:bg-slate-800"
            >
              Sign out
            </button>
          </div>
        </header>

        <main className="min-h-0 flex-1 overflow-y-auto p-4 md:p-6">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
