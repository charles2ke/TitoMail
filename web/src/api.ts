import type {
  ActionResult,
  Address,
  Alias,
  AttachmentRef,
  ComposeState,
  Contact,
  DNSRecord,
  Domain,
  Filter,
  Label,
  ListResult,
  ListView,
  Mailbox,
  MessageAction,
  MessageView,
  Overview,
  QueueEntry,
  SaveDraftResult,
  SenderPref,
  SendResult,
  SessionInfo,
  Settings,
  Thread,
  Upload,
  User,
} from "./types";

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

let csrfToken = "";
let unauthorizedHandler: (() => void) | null = null;

export function setCSRFToken(token: string) {
  csrfToken = token;
}

/** Registers a callback for when the session has expired. */
export function onUnauthorized(fn: () => void) {
  unauthorizedHandler = fn;
}

async function request<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  const headers: Record<string, string> = {};
  let payload: BodyInit | undefined;
  if (body instanceof FormData) {
    payload = body;
  } else if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    payload = JSON.stringify(body);
  }
  if (method !== "GET" && csrfToken) headers["X-CSRF-Token"] = csrfToken;
  let res: Response;
  try {
    res = await fetch(path, { method, headers, body: payload, credentials: "same-origin", signal });
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    throw new ApiError(0, "Can't reach the server. Check your connection and try again.");
  }
  const text = await res.text();
  let data: unknown;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = undefined;
    }
  }
  if (!res.ok) {
    const msg = (data as { error?: string } | undefined)?.error || `${res.status} ${res.statusText}`;
    if (res.status === 401 && path !== "/api/login" && unauthorizedHandler) unauthorizedHandler();
    throw new ApiError(res.status, msg);
  }
  return data as T;
}

const get = <T>(path: string, signal?: AbortSignal) => request<T>("GET", path, undefined, signal);
const post = <T>(path: string, body?: unknown) => request<T>("POST", path, body ?? {});
const put = <T>(path: string, body: unknown) => request<T>("PUT", path, body);
const patch = <T>(path: string, body: unknown) => request<T>("PATCH", path, body);
const del = <T>(path: string) => request<T>("DELETE", path);

export interface ActionBody {
  action: MessageAction;
  mailbox_id?: number;
  label_id?: number;
}

export const api = {
  session: () => get<SessionInfo>("/api/session"),
  login: (email: string, password: string) => post<SessionInfo>("/api/login", { email, password }),
  setup: (body: { token: string; email: string; password: string; display_name: string }) =>
    post<SessionInfo>("/api/setup", body),
  logout: () => post<unknown>("/api/logout"),

  mailboxes: () => get<Mailbox[]>("/api/mailboxes"),
  createMailbox: (name: string) => post<Mailbox>("/api/mailboxes", { name }),
  updateMailbox: (id: number, body: { name?: string; subscribed?: boolean }) => patch<Mailbox>(`/api/mailboxes/${id}`, body),
  deleteMailbox: (id: number) => del<unknown>(`/api/mailboxes/${id}`),
  emptyMailbox: (id: number) => post<{ deleted: number }>(`/api/mailboxes/${id}/empty`),

  labels: () => get<Label[]>("/api/labels"),
  createLabel: (name: string, color: string) => post<Label>("/api/labels", { name, color }),
  updateLabel: (id: number, body: { name?: string; color?: string }) => patch<Label>(`/api/labels/${id}`, body),
  deleteLabel: (id: number) => del<unknown>(`/api/labels/${id}`),

  conversations: (params: URLSearchParams, signal?: AbortSignal) => get<ListResult>(`/api/conversations?${params}`, signal),
  thread: (id: number, trash: boolean) => get<Thread>(`/api/threads/${id}${trash ? "?trash=1" : ""}`),
  message: (id: number, remote: boolean) => get<MessageView>(`/api/messages/${id}${remote ? "?remote=1" : ""}`),
  composeFrom: (id: number, mode: "reply" | "reply_all" | "forward") =>
    get<ComposeState>(`/api/messages/${id}/compose?mode=${mode}`),
  messageAction: (ids: number[], body: ActionBody) => post<ActionResult>("/api/messages/actions", { ids, ...body }),
  threadAction: (threadIds: number[], view: ListView, body: ActionBody) =>
    post<ActionResult>("/api/threads/actions", { thread_ids: threadIds, view, ...body }),

  draft: (id: number) => get<ComposeState>(`/api/drafts/${id}`),
  saveDraft: (state: ComposeState) => post<SaveDraftResult>("/api/drafts", state),
  send: (state: ComposeState) => post<SendResult>("/api/send", state),
  outbox: () => get<QueueEntry[]>("/api/outbox"),
  cancelSend: (queueId: number) => post<{ draft_id: number }>(`/api/outbox/${queueId}/cancel`),

  contacts: (q = "", saved = false) =>
    get<Contact[]>(`/api/contacts?${new URLSearchParams({ q, ...(saved ? { saved: "1" } : {}) })}`),
  saveContact: (c: Partial<Contact>) =>
    c.id ? put<Contact>(`/api/contacts/${c.id}`, c) : post<Contact>("/api/contacts", c),
  deleteContact: (id: number) => del<unknown>(`/api/contacts/${id}`),
  autocomplete: (q: string, signal?: AbortSignal) =>
    get<Address[]>(`/api/autocomplete?q=${encodeURIComponent(q)}`, signal),

  settings: () => get<Settings>("/api/settings"),
  saveSettings: (s: Settings) => put<Settings>("/api/settings", s),
  updateAccount: (displayName: string) => patch<User>("/api/account", { display_name: displayName }),
  changePassword: (current: string, next: string) => post<unknown>("/api/account/password", { current, new: next }),
  myAliases: () => get<Alias[]>("/api/account/aliases"),

  filters: () => get<Filter[]>("/api/filters"),
  saveFilter: (f: Filter) => (f.id ? put<Filter>(`/api/filters/${f.id}`, f) : post<Filter>("/api/filters", f)),
  deleteFilter: (id: number) => del<unknown>(`/api/filters/${id}`),

  senders: () => get<SenderPref[]>("/api/senders"),
  setSender: (address: string, kind: "trusted" | "blocked") => post<unknown>("/api/senders", { address, kind }),
  deleteSender: (address: string) => del<unknown>(`/api/senders?address=${encodeURIComponent(address)}`),

  admin: {
    overview: () => get<Overview>("/api/admin/overview"),
    domains: () => get<Domain[]>("/api/admin/domains"),
    createDomain: (name: string) => post<Domain>("/api/admin/domains", { name }),
    deleteDomain: (id: number) => del<unknown>(`/api/admin/domains/${id}`),
    dns: (id: number, check: boolean) =>
      get<{ domain: Domain; hostname: string; records: DNSRecord[] }>(`/api/admin/domains/${id}/dns${check ? "?check=1" : ""}`),
    users: () => get<User[]>("/api/admin/users"),
    createUser: (body: { email: string; password: string; display_name: string; is_admin: boolean; quota_bytes: number }) =>
      post<User>("/api/admin/users", body),
    updateUser: (
      id: number,
      body: { display_name?: string; is_admin?: boolean; disabled?: boolean; quota_bytes?: number; password?: string },
    ) => patch<User>(`/api/admin/users/${id}`, body),
    deleteUser: (id: number) => del<unknown>(`/api/admin/users/${id}`),
    aliases: () => get<Alias[]>("/api/admin/aliases"),
    createAlias: (address: string, userId: number) => post<Alias>("/api/admin/aliases", { address, user_id: userId }),
    deleteAlias: (id: number) => del<unknown>(`/api/admin/aliases/${id}`),
    queue: () => get<QueueEntry[]>("/api/admin/queue"),
    retryQueue: (id: number) => post<unknown>(`/api/admin/queue/${id}/retry`),
    deleteQueue: (id: number) => del<unknown>(`/api/admin/queue/${id}`),
  },
};

/**
 * Uploads a file for use as an attachment, reporting progress (0..1).
 * fetch() cannot report upload progress, so this uses XMLHttpRequest.
 */
export function uploadFile(file: File, onProgress: (p: number) => void, signal?: AbortSignal): Promise<Upload> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", "/api/uploads");
    if (csrfToken) xhr.setRequestHeader("X-CSRF-Token", csrfToken);
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) onProgress(e.loaded / e.total);
    };
    xhr.onload = () => {
      let data: unknown;
      try {
        data = JSON.parse(xhr.responseText);
      } catch {
        data = undefined;
      }
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve(data as Upload);
      } else {
        if (xhr.status === 401 && unauthorizedHandler) unauthorizedHandler();
        const msg = (data as { error?: string } | undefined)?.error;
        reject(new ApiError(xhr.status, msg || (xhr.status === 413 ? "The file is too large" : `Upload failed (${xhr.status})`)));
      }
    };
    xhr.onerror = () => reject(new ApiError(0, "Upload failed: the server could not be reached"));
    xhr.onabort = () => reject(new DOMException("Upload cancelled", "AbortError"));
    signal?.addEventListener("abort", () => xhr.abort());
    const form = new FormData();
    form.append("file", file, file.name);
    xhr.send(form);
  });
}

export function attachmentURL(messageId: number, part: string, download = false) {
  return `/api/messages/${messageId}/attachments/${encodeURIComponent(part)}${download ? "?download=1" : ""}`;
}

export function refURL(ref: AttachmentRef): string | null {
  return ref.message_id && ref.part ? attachmentURL(ref.message_id, ref.part) : null;
}

export function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
