export async function api(path, opts = {}) {
  const res = await fetch(path, {
    ...opts,
    headers: { "Content-Type": "application/json", "X-Requested-With": "clusterdeck", ...(opts.headers || {}) },
  });
  const type = res.headers.get("Content-Type") || "";
  const body = type.includes("json") ? await res.json() : await res.text();
  const login = loginTarget(res.status, body);
  if (login) {
    location.assign(login);
    return new Promise(() => {});
  }
  if (!res.ok) throw new Error((body && body.error) || `HTTP ${res.status}`);
  return body;
}

export function post(path, body) {
  return api(path, { method: "POST", body: JSON.stringify(body || {}) });
}

export function loginTarget(status, body) {
  if (status !== 401 || !body || typeof body.login !== "string") return null;
  return body.login.startsWith("/auth/login") ? body.login : null;
}
