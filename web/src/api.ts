const TOKEN_KEY = 'forge_gateway_token';

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY);
}

export function setToken(token: string): void {
  localStorage.setItem(TOKEN_KEY, token);
}

export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY);
}

async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const token = getToken();
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...(init?.headers as Record<string, string> || {}),
  };
  if (token) {
    headers['Authorization'] = `Bearer ${token}`;
  }

  const resp = await fetch(path, { ...init, headers });

  if (resp.status === 401) {
    clearToken();
    throw new AuthError();
  }

  if (!resp.ok) {
    throw new Error(`API error: ${resp.status}`);
  }

  return resp.json();
}

export class AuthError extends Error {
  constructor() {
    super('Unauthorized');
    this.name = 'AuthError';
  }
}

// Types
export interface Session {
  sessionId: string;
  name: string;
  status: 'active' | 'closed';
  createdAt: number;
  lastActiveAt: number;
  cwd: string;
}

export interface SessionListResponse {
  sessions: Session[];
  total: number;
  limit: number;
  offset: number;
}

export interface SessionMessage {
  uuid: string;
  parentUuid?: string;
  sessionId: string;
  type: 'user' | 'assistant' | 'system' | 'reflection';
  message: any;
  timestamp: number;
}

export interface SessionCost {
  sessionId: string;
  totalCost: number;
  callCount: number;
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  firstCall: string;
  lastCall: string;
}

export interface CostSummary {
  totalCost: number;
  sessionCount: number;
  callCount: number;
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  daily: DailySummary[];
}

export interface DailySummary {
  date: string;
  totalCost: number;
  sessionCount: number;
  callCount: number;
  inputTokens: number;
  outputTokens: number;
}

// API functions
export async function fetchSessions(params?: {
  status?: string;
  limit?: number;
  offset?: number;
}): Promise<SessionListResponse> {
  const q = new URLSearchParams();
  if (params?.status) q.set('status', params.status);
  if (params?.limit) q.set('limit', String(params.limit));
  if (params?.offset) q.set('offset', String(params.offset));
  const qs = q.toString();
  return apiFetch(`/api/sessions${qs ? '?' + qs : ''}`);
}

export async function fetchSessionHistory(
  sessionId: string
): Promise<{ messages: SessionMessage[] }> {
  return apiFetch(`/api/sessions/${sessionId}/history`);
}

export async function fetchSessionCosts(
  sessionId: string
): Promise<SessionCost> {
  return apiFetch(`/api/sessions/${sessionId}/costs`);
}

export async function fetchCostSummary(
  start?: string,
  end?: string
): Promise<CostSummary> {
  const q = new URLSearchParams();
  if (start) q.set('start', start);
  if (end) q.set('end', end);
  const qs = q.toString();
  return apiFetch(`/api/costs/summary${qs ? '?' + qs : ''}`);
}
