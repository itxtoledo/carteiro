const TOKEN_KEY = "carteiro.token";
const THEME_KEY = "carteiro.theme";

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? "";
}

export function setToken(token: string) {
  if (token) localStorage.setItem(TOKEN_KEY, token);
  else localStorage.removeItem(TOKEN_KEY);
}

export function clearSession() {
  localStorage.removeItem(TOKEN_KEY);
  window.dispatchEvent(new Event("carteiro:unauthorized"));
}

export function getTheme(): "dark" | "light" {
  const saved = localStorage.getItem(THEME_KEY);
  if (saved === "dark" || saved === "light") return saved;
  return window.matchMedia?.("(prefers-color-scheme: dark)").matches
    ? "dark"
    : "light";
}

export function applyTheme(theme: "dark" | "light") {
  document.documentElement.classList.toggle("dark", theme === "dark");
}

export function saveTheme(theme: "dark" | "light") {
  localStorage.setItem(THEME_KEY, theme);
  applyTheme(theme);
}

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  const token = getToken();
  if (token) headers.Authorization = `Bearer ${token}`;
  if (init?.body) headers["Content-Type"] = "application/json";

  let res: Response;
  try {
    res = await fetch(path, { ...init, headers });
  } catch {
    throw new ApiError(0, "cannot reach the server");
  }
  if (res.status === 401) {
    clearSession();
    throw new ApiError(401, "invalid or expired API token");
  }
  if (!res.ok) {
    let message = `request failed (${res.status})`;
    try {
      const body = (await res.json()) as { error?: string };
      if (body.error) message = body.error;
    } catch {
      /* keep the fallback message */
    }
    throw new ApiError(res.status, message);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

/**
 * Validates a candidate token before it is stored: performs a real authed
 * round-trip with an explicit header (the generic helpers read the token from
 * storage, which is still empty at login time).
 */
export async function checkToken(token: string): Promise<void> {
  let res: Response;
  try {
    res = await fetch("/api/stats", {
      headers: { Accept: "application/json", Authorization: `Bearer ${token.trim()}` },
    });
  } catch {
    throw new ApiError(0, "cannot reach the server");
  }
  if (res.status === 401) throw new ApiError(401, "invalid token");
  if (!res.ok) throw new ApiError(res.status, `server error (${res.status})`);
}

export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: "POST", body: body === undefined ? undefined : JSON.stringify(body) }),
  patch: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: "PATCH", body: body === undefined ? undefined : JSON.stringify(body) }),
  del: <T>(path: string) => request<T>(path, { method: "DELETE" }),
};
