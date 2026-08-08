// HTTP client for the bulk reads.
//
// Source text and directory listings go over HTTP rather than the WebSocket so
// that fetching a 2 MB file does not sit in front of latency-sensitive stepping
// traffic and inferior output on the one connection. Credentials are the
// session cookie, which the browser attaches automatically; no header is set,
// because a rebound page would not have the cookie.

export class ApiError extends Error {
  constructor(code, message, status) {
    super(message || code);
    this.code = code;
    this.status = status;
  }
}

async function request(url) {
  let res;
  try {
    res = await fetch(url, { credentials: "same-origin" });
  } catch (err) {
    throw new ApiError("network", String(err), 0);
  }
  if (!res.ok) {
    let code = "internal";
    let message = `${res.status} ${res.statusText}`;
    const type = res.headers.get("content-type") || "";
    if (type.includes("application/json")) {
      try {
        const body = await res.json();
        if (body?.error) {
          code = body.error.code || code;
          message = body.error.message || message;
        }
      } catch {
        // Fall through to the status-line message.
      }
    }
    throw new ApiError(code, message, res.status);
  }
  return res;
}

export async function fetchTree(path = "") {
  const res = await request(`/api/tree?path=${encodeURIComponent(path)}`);
  return res.json();
}

export async function fetchFile(path) {
  const res = await request(`/api/file?path=${encodeURIComponent(path)}`);
  return {
    text: await res.text(),
    etag: res.headers.get("etag") || "",
    size: Number(res.headers.get("content-length") || 0),
  };
}
