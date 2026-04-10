import { loadConfig } from "./config.js";

async function request<T>(path: string, init: RequestInit): Promise<T> {
  const config = loadConfig();
  if (!config?.session?.accessToken || !config.baseUrl) {
    throw new Error("mesh-cli is not logged in. Run: mesh auth login <meshApiBaseUrl>");
  }

  const res = await fetch(`${config.baseUrl.replace(/\/$/, "")}${path}`, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${config.session.accessToken}`,
      ...(init.headers ?? {}),
    },
  });

  const text = await res.text();
  const body = text ? JSON.parse(text) : {};

  if (!res.ok) {
    throw new Error(body?.error ?? `Request failed (${res.status})`);
  }

  return body as T;
}

export function post<T>(path: string, body: unknown) {
  return request<T>(path, {
    method: "POST",
    body: JSON.stringify(body),
  });
}
