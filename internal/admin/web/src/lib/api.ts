// SaaS API client. JWT-based; token persisted in localStorage.

import type { User } from "@/lib/types";

const TOKEN_KEY = "hypi.jwt";
const USER_KEY = "hypi.user";

export const getJWT = (): string => localStorage.getItem(TOKEN_KEY) || "";
export const setJWT = (t: string): void => {
  if (t) {
    localStorage.setItem(TOKEN_KEY, t);
    // A fresh token means a new session — re-arm the expiry guard so a future
    // 401 fires the "session expired" flow again.
    sessionExpiredFired = false;
  } else localStorage.removeItem(TOKEN_KEY);
};

// SESSION_EXPIRED_EVENT is dispatched on window when an authenticated request
// comes back 401 (JWT expired/invalid). AuthProvider listens for it to clear
// the user (→ RequireAuth bounces to /login) and toast a notice. JWTs are
// fixed-lifetime with no refresh flow, so this is how an already-open tab
// learns its session died instead of surfacing a raw "invalid token".
export const SESSION_EXPIRED_EVENT = "hypi:session-expired";

// One-shot guard: a burst of parallel requests can each 401 at once, but we
// only want a single logout + toast. Reset whenever a new JWT is stored.
let sessionExpiredFired = false;

function handleSessionExpired() {
  if (sessionExpiredFired) return;
  sessionExpiredFired = true;
  setJWT("");
  setCachedUser(null);
  window.dispatchEvent(new Event(SESSION_EXPIRED_EVENT));
}

export const getCachedUser = (): User | null => {
  const raw = localStorage.getItem(USER_KEY);
  if (!raw) return null;
  try {
    return JSON.parse(raw) as User;
  } catch {
    return null;
  }
};
export const setCachedUser = (u: User | null): void => {
  if (u) localStorage.setItem(USER_KEY, JSON.stringify(u));
  else localStorage.removeItem(USER_KEY);
};

export const logout = () => {
  setJWT("");
  setCachedUser(null);
};

export class ApiError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.status = status;
  }
}

const BASE = "/api/v2";

export async function api<T = unknown>(path: string, opts: RequestInit = {}): Promise<T> {
  const token = getJWT();
  const headers: Record<string, string> = {
    ...(opts.body instanceof FormData ? {} : { "Content-Type": "application/json" }),
    ...((opts.headers as Record<string, string>) || {}),
  };
  if (token) headers.Authorization = `Bearer ${token}`;
  const res = await fetch(BASE + path, { ...opts, headers });
  const text = await res.text();
  let data: unknown = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = { raw: text };
  }
  if (!res.ok) {
    // A 401 on a request we sent a token with means the session expired/was
    // revoked server-side. Centralise the bounce-to-login here so no caller
    // has to special-case it (and users stop seeing the raw "invalid token").
    if (res.status === 401 && token) {
      handleSessionExpired();
    }
    const msg =
      (data && typeof data === "object" && "error" in data && typeof data.error === "string"
        ? data.error
        : null) || `HTTP ${res.status}`;
    throw new ApiError(msg, res.status);
  }
  return data as T;
}

// apiDownload fetches a file endpoint and saves it to disk.
//
// A plain <a href> can't be used: the JWT lives in localStorage, not a cookie, so
// a browser navigation would arrive at the server unauthenticated. We fetch with
// the Authorization header, then hand the blob to a synthetic anchor.
export async function apiDownload(path: string, filename: string): Promise<void> {
  const token = getJWT();
  const res = await fetch(BASE + path, {
    method: "GET",
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  });
  if (!res.ok) {
    if (res.status === 401 && token) handleSessionExpired();
    throw new ApiError(`HTTP ${res.status}`, res.status);
  }
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  try {
    const a = document.createElement("a");
    a.href = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
  } finally {
    // Object URLs pin their blob in memory until revoked — without this, every
    // export leaks the whole file for the lifetime of the tab.
    URL.revokeObjectURL(url);
  }
}

export const apiGet = <T = unknown>(p: string) => api<T>(p, { method: "GET" });
export const apiPost = <T = unknown>(p: string, body?: unknown) =>
  api<T>(p, { method: "POST", body: body == null ? undefined : JSON.stringify(body) });
export const apiPatch = <T = unknown>(p: string, body?: unknown) =>
  api<T>(p, { method: "PATCH", body: body == null ? undefined : JSON.stringify(body) });
export const apiDelete = <T = unknown>(p: string) => api<T>(p, { method: "DELETE" });
