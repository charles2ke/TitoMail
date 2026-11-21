// Types mirroring the JSON API of the TitoMail server.

export interface Address {
  name: string;
  address: string;
}

export interface User {
  id: number;
  domain_id: number;
  email: string;
  display_name: string;
  is_admin: boolean;
  disabled: boolean;
  quota_bytes: number;
  created_at: string;
  used_bytes: number;
}

export interface Vacation {
  enabled: boolean;
  subject: string;
  body: string;
  start: string;
  end: string;
  contacts_only: boolean;
}

export type ReadingPane = "right" | "bottom" | "none";
export type Theme = "system" | "light" | "dark";

export interface Settings {
  signature: string;
  conversation_view: boolean;
  reading_pane: ReadingPane;
  theme: Theme;
  density: "comfortable" | "compact";
  page_size: number;
  external_images: "ask" | "always";
  undo_send_seconds: number;
  desktop_notifications: boolean;
  vacation: Vacation;
}

export interface SessionInfo {
  authenticated: boolean;
  setup_required: boolean;
  user?: User;
  settings?: Settings;
  csrf_token?: string;
  addresses?: string[];
  hostname: string;
  version: string;
  max_upload_bytes: number;
}

export type SpecialUse = "inbox" | "drafts" | "sent" | "archive" | "junk" | "trash";

export interface Mailbox {
  id: number;
  name: string;
  special_use?: SpecialUse;
  subscribed: boolean;
  total: number;
  unread: number;
  size: number;
}

export interface Label {
  id: number;
  name: string;
  color: string;
  total: number;
  unread: number;
}

export interface Participant {
  name: string;
  address: string;
  unread: boolean;
}

export interface Conversation {
  thread_id: number;
  message_ids: number[];
  latest_id: number;
  subject: string;
  snippet: string;
  participants: Participant[] | null;
  to: Address[] | null;
  count: number;
  unread: boolean;
  starred: boolean;
  has_attachments: boolean;
  has_draft: boolean;
  labels: number[] | null;
  mailboxes: number[] | null;
  date: string;
}

export interface ListResult {
  conversations: Conversation[] | null;
  total: number;
  offset: number;
  limit: number;
}

export interface Attachment {
  part: string;
  filename: string;
  content_type: string;
  size: number;
  content_id?: string;
  inline: boolean;
}

export interface Message {
  id: number;
  mailbox_id: number;
  thread_id: number;
  message_id: string;
  in_reply_to: string;
  references: string[] | null;
  subject: string;
  from: Address;
  to: Address[] | null;
  cc: Address[] | null;
  bcc: Address[] | null;
  reply_to: Address[] | null;
  date: string;
  received_at: string;
  size: number;
  snippet: string;
  has_attachments: boolean;
  attachments: Attachment[] | null;
  seen: boolean;
  flagged: boolean;
  answered: boolean;
  draft: boolean;
  deleted: boolean;
  keywords: string[] | null;
  labels: number[] | null;
}

export interface Rendered {
  document: string;
  remote_blocked: boolean;
  has_quote: boolean;
}

export interface AuthSummary {
  spf?: string;
  dkim?: string;
  dmarc?: string;
  mailed_by?: string;
  signed_by?: string;
}

export interface MessageView extends Message {
  mailbox_special: string;
  body: Rendered;
  remote_allowed: boolean;
  sender_pref: string;
  unsubscribe?: string;
  auth?: AuthSummary;
  sender?: Address;
}

export interface Thread {
  thread_id: number;
  subject: string;
  messages: Message[] | null;
}

export interface AttachmentRef {
  upload_id?: string;
  message_id?: number;
  part?: string;
  filename: string;
  content_type: string;
  size: number;
}

export interface ComposeState {
  draft_id?: number;
  from: string;
  to: Address[];
  cc: Address[];
  bcc: Address[];
  subject: string;
  html: string;
  attachments: AttachmentRef[];
  in_reply_to_id?: number;
  forward_of_id?: number;
  schedule_at?: string;
}

export interface Upload {
  id: string;
  filename: string;
  content_type: string;
  size: number;
  created_at: string;
}

export interface SaveDraftResult {
  draft_id: number;
  thread_id: number;
  attachments: AttachmentRef[] | null;
}

export interface SendResult {
  queue_id: number;
  send_at: string;
  scheduled: boolean;
  message_id: string;
  thread_id: number;
}

export interface QueueRcpt {
  id: number;
  rcpt: string;
  domain: string;
  status: "pending" | "sent" | "failed";
  attempts: number;
  next_attempt_at: string;
  last_attempt_at: string;
  last_error: string;
}

export interface QueueEntry {
  id: number;
  user_id: number;
  mail_from: string;
  message_id: string;
  subject: string;
  sent_copy_id: number;
  scheduled: boolean;
  size: number;
  created_at: string;
  recipients: QueueRcpt[] | null;
}

export interface Contact {
  id: number;
  email: string;
  name: string;
  phone: string;
  notes: string;
  saved: boolean;
  use_count: number;
  last_used_at: string;
}

export interface FilterCriteria {
  from: string;
  to: string;
  subject: string;
  has_words: string;
  has_attachment: boolean;
}

export interface FilterActions {
  skip_inbox: boolean;
  mark_read: boolean;
  star: boolean;
  label_id: number;
  move_to: number;
  trash: boolean;
  never_spam: boolean;
}

export interface Filter {
  id: number;
  name: string;
  criteria: FilterCriteria;
  actions: FilterActions;
  enabled: boolean;
}

export interface SenderPref {
  address: string;
  kind: "trusted" | "blocked";
  created_at: string;
}

export interface Domain {
  id: number;
  name: string;
  dkim_selector: string;
  created_at: string;
  users: number;
}

export interface Alias {
  id: number;
  address: string;
  user_id: number;
  user_email: string;
  created_at: string;
}

export interface DNSRecord {
  type: string;
  name: string;
  value: string;
  purpose: string;
  required: boolean;
  status: "ok" | "missing" | "mismatch" | "error" | "unchecked";
  found: string[] | null;
}

export interface Overview {
  hostname: string;
  version: string;
  users: number;
  domains: number;
  storage_bytes: number;
  queue_pending: number;
  queue_deferred: number;
  ports: Record<string, number>;
}

export interface UndoMove {
  mailbox_id: number;
  ids: number[];
}

export interface ActionResult {
  affected: number;
  undo?: UndoMove[];
}

export type MessageAction =
  | "read"
  | "unread"
  | "star"
  | "unstar"
  | "archive"
  | "trash"
  | "spam"
  | "notspam"
  | "inbox"
  | "move"
  | "delete"
  | "label"
  | "unlabel";

export interface ListView {
  kind: "mailbox" | "label" | "starred" | "all";
  mailbox_id?: number;
  label_id?: number;
}

export interface ChangeEvent {
  mailboxes: boolean;
  messages: boolean;
  added: number[] | null;
}
