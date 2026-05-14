import { useState, useEffect, useCallback } from 'react';
import './index.css';
import {
  AuthError,
  clearToken,
  fetchSessionCosts,
  fetchSessionHistory,
  fetchSessions,
  getToken,
  setToken,
} from './api';
import type {
  Session,
  SessionCost,
  SessionMessage,
} from './api';

function App() {
  const [route, setRoute] = useState(() => getRoute());
  const [needsAuth, setNeedsAuth] = useState(false);

  useEffect(() => {
    const onPop = () => setRoute(getRoute());
    window.addEventListener('popstate', onPop);
    return () => window.removeEventListener('popstate', onPop);
  }, []);

  const navigate = useCallback((path: string) => {
    window.history.pushState(null, '', '/ui' + path);
    setRoute(getRoute());
  }, []);

  const handleAuthError = useCallback(() => {
    setNeedsAuth(true);
  }, []);

  if (needsAuth) {
    return <TokenPrompt onSubmit={(t) => { setToken(t); setNeedsAuth(false); }} />;
  }

  if (route.sessionId) {
    return (
      <Layout onSignOut={() => { clearToken(); setNeedsAuth(true); }}>
        <SessionDetail
          sessionId={route.sessionId}
          onBack={() => navigate('/')}
          onAuthError={handleAuthError}
        />
      </Layout>
    );
  }

  return (
    <Layout onSignOut={() => { clearToken(); setNeedsAuth(true); }}>
      <SessionList
        onSelect={(id) => navigate(`/sessions/${id}`)}
        onAuthError={handleAuthError}
      />
    </Layout>
  );
}

function getRoute(): { sessionId?: string } {
  const path = window.location.pathname.replace(/^\/ui\/?/, '');
  const match = path.match(/^sessions\/(.+)/);
  return match ? { sessionId: match[1] } : {};
}

function Layout({ children, onSignOut }: { children: React.ReactNode; onSignOut: () => void }) {
  return (
    <div className="min-h-screen bg-gray-950 text-gray-100">
      <header className="border-b border-gray-800 px-6 py-3 flex items-center justify-between">
        <h1 className="text-lg font-semibold text-white">Forge</h1>
        {getToken() && (
          <button
            onClick={onSignOut}
            className="text-sm text-gray-400 hover:text-white"
          >
            Sign out
          </button>
        )}
      </header>
      <main className="max-w-7xl mx-auto px-4 py-6">{children}</main>
    </div>
  );
}

function TokenPrompt({ onSubmit }: { onSubmit: (token: string) => void }) {
  const [value, setValue] = useState('');
  return (
    <div className="min-h-screen bg-gray-950 flex items-center justify-center">
      <div className="bg-gray-900 border border-gray-700 rounded-lg p-6 w-96">
        <h2 className="text-white text-lg font-semibold mb-4">Gateway Token Required</h2>
        <input
          type="password"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          placeholder="Enter gateway token"
          className="w-full px-3 py-2 bg-gray-800 border border-gray-600 rounded text-white mb-4"
          onKeyDown={(e) => e.key === 'Enter' && value && onSubmit(value)}
          autoFocus
        />
        <button
          onClick={() => value && onSubmit(value)}
          className="w-full py-2 bg-blue-600 hover:bg-blue-500 rounded text-white font-medium"
        >
          Connect
        </button>
      </div>
    </div>
  );
}

function SessionList({
  onSelect,
  onAuthError,
}: {
  onSelect: (id: string) => void;
  onAuthError: () => void;
}) {
  const [sessions, setSessions] = useState<Session[]>([]);
  const [total, setTotal] = useState(0);
  const [filter, setFilter] = useState<string>('');
  const [search, setSearch] = useState('');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');

  const load = useCallback(async () => {
    try {
      const data = await fetchSessions({
        status: filter || undefined,
        limit: 50,
      });
      setSessions(data.sessions);
      setTotal(data.total);
      setError('');
    } catch (e) {
      if (e instanceof AuthError) { onAuthError(); return; }
      setError('Failed to load sessions');
    } finally {
      setLoading(false);
    }
  }, [filter, onAuthError]);

  useEffect(() => {
    load();
    const interval = setInterval(load, 10000);
    return () => clearInterval(interval);
  }, [load]);

  const filtered = sessions.filter(
    (s) =>
      !search ||
      s.name.toLowerCase().includes(search.toLowerCase()) ||
      s.sessionId.toLowerCase().includes(search.toLowerCase())
  );

  return (
    <div>
      <div className="flex flex-col sm:flex-row gap-3 mb-4">
        <input
          type="text"
          placeholder="Search sessions..."
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="flex-1 px-3 py-2 bg-gray-900 border border-gray-700 rounded text-white text-sm"
        />
        <div className="flex gap-2">
          {['', 'active', 'closed'].map((s) => (
            <button
              key={s}
              onClick={() => setFilter(s)}
              className={`px-3 py-1 rounded text-sm ${
                filter === s
                  ? 'bg-blue-600 text-white'
                  : 'bg-gray-800 text-gray-400 hover:text-white'
              }`}
            >
              {s || 'All'}
            </button>
          ))}
        </div>
      </div>

      {loading && <p className="text-gray-500">Loading...</p>}
      {error && <p className="text-red-400">{error}</p>}

      <div className="text-sm text-gray-500 mb-2">{total} sessions</div>

      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-gray-800 text-gray-400 text-left">
              <th className="py-2 pr-4">Name</th>
              <th className="py-2 pr-4">Status</th>
              <th className="py-2 pr-4 hidden md:table-cell">Created</th>
              <th className="py-2 pr-4">Last Active</th>
              <th className="py-2 pr-4 hidden lg:table-cell">CWD</th>
            </tr>
          </thead>
          <tbody>
            {filtered.map((s) => (
              <tr
                key={s.sessionId}
                onClick={() => onSelect(s.sessionId)}
                className="border-b border-gray-800/50 hover:bg-gray-900 cursor-pointer"
              >
                <td className="py-2 pr-4 font-medium text-white">
                  {s.name === s.sessionId ? s.sessionId.slice(0, 8) : s.name}
                </td>
                <td className="py-2 pr-4">
                  <StatusBadge status={s.status} />
                </td>
                <td className="py-2 pr-4 text-gray-400 hidden md:table-cell">
                  {formatTime(s.createdAt)}
                </td>
                <td className="py-2 pr-4 text-gray-400">
                  {formatTime(s.lastActiveAt)}
                </td>
                <td className="py-2 pr-4 text-gray-500 hidden lg:table-cell font-mono text-xs truncate max-w-xs">
                  {s.cwd}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function SessionDetail({
  sessionId,
  onBack,
  onAuthError,
}: {
  sessionId: string;
  onBack: () => void;
  onAuthError: () => void;
}) {
  const [messages, setMessages] = useState<SessionMessage[]>([]);
  const [cost, setCost] = useState<SessionCost | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');

  const load = useCallback(async () => {
    try {
      const [histResp, costResp] = await Promise.allSettled([
        fetchSessionHistory(sessionId),
        fetchSessionCosts(sessionId),
      ]);
      if (histResp.status === 'fulfilled') {
        setMessages(histResp.value.messages || []);
      }
      if (costResp.status === 'fulfilled') {
        setCost(costResp.value);
      }
      setError('');
    } catch (e) {
      if (e instanceof AuthError) { onAuthError(); return; }
      setError('Failed to load session');
    } finally {
      setLoading(false);
    }
  }, [sessionId, onAuthError]);

  useEffect(() => {
    load();
  }, [load]);

  return (
    <div>
      <button
        onClick={onBack}
        className="text-blue-400 hover:text-blue-300 text-sm mb-4 inline-block"
      >
        ← Back to sessions
      </button>

      <div className="flex flex-col lg:flex-row gap-6">
        <div className="flex-1 min-w-0">
          <h2 className="text-lg font-semibold text-white mb-4">
            Session {sessionId.slice(0, 8)}
          </h2>

          {loading && <p className="text-gray-500">Loading...</p>}
          {error && <p className="text-red-400">{error}</p>}

          <div className="space-y-3 max-h-[70vh] overflow-y-auto">
            {messages.map((msg) => (
              <MessageCard key={msg.uuid} message={msg} />
            ))}
            {messages.length === 0 && !loading && (
              <p className="text-gray-500">No messages yet.</p>
            )}
          </div>
        </div>

        {cost && cost.callCount > 0 && (
          <div className="lg:w-64 shrink-0">
            <div className="bg-gray-900 border border-gray-800 rounded-lg p-4 sticky top-6">
              <h3 className="text-sm font-semibold text-gray-400 mb-3">Cost</h3>
              <div className="space-y-2 text-sm">
                <div className="flex justify-between">
                  <span className="text-gray-400">Total</span>
                  <span className="text-white">${cost.totalCost.toFixed(4)}</span>
                </div>
                <div className="flex justify-between">
                  <span className="text-gray-400">Calls</span>
                  <span className="text-white">{cost.callCount}</span>
                </div>
                <div className="flex justify-between">
                  <span className="text-gray-400">Input</span>
                  <span className="text-white">{formatTokens(cost.inputTokens)}</span>
                </div>
                <div className="flex justify-between">
                  <span className="text-gray-400">Output</span>
                  <span className="text-white">{formatTokens(cost.outputTokens)}</span>
                </div>
                {cost.cacheReadTokens > 0 && (
                  <div className="flex justify-between">
                    <span className="text-gray-400">Cache Read</span>
                    <span className="text-white">{formatTokens(cost.cacheReadTokens)}</span>
                  </div>
                )}
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

function MessageCard({ message }: { message: SessionMessage }) {
  const [expanded, setExpanded] = useState(false);
  const isUser = message.type === 'user';
  const isAssistant = message.type === 'assistant';

  // Extract text content from the message
  const content = extractContent(message.message);

  return (
    <div
      className={`rounded-lg p-3 text-sm ${
        isUser
          ? 'bg-blue-950/30 border border-blue-900/50'
          : isAssistant
          ? 'bg-gray-900 border border-gray-800'
          : 'bg-gray-900/50 border border-gray-800/50'
      }`}
    >
      <div className="flex items-center gap-2 mb-1">
        <span className="text-xs font-medium text-gray-500 uppercase">
          {message.type}
        </span>
        <span className="text-xs text-gray-600">
          {formatTime(message.timestamp)}
        </span>
      </div>
      <div
        className={`text-gray-200 whitespace-pre-wrap break-words ${
          !expanded && content.length > 500 ? 'max-h-32 overflow-hidden' : ''
        }`}
      >
        {content}
      </div>
      {content.length > 500 && (
        <button
          onClick={() => setExpanded(!expanded)}
          className="text-blue-400 hover:text-blue-300 text-xs mt-1"
        >
          {expanded ? 'Show less' : 'Show more'}
        </button>
      )}
    </div>
  );
}

function StatusBadge({ status }: { status: string }) {
  return (
    <span
      className={`inline-flex items-center gap-1 text-xs font-medium px-2 py-0.5 rounded-full ${
        status === 'active'
          ? 'bg-green-900/50 text-green-400'
          : 'bg-gray-800 text-gray-400'
      }`}
    >
      {status === 'active' && (
        <span className="w-1.5 h-1.5 rounded-full bg-green-400" />
      )}
      {status}
    </span>
  );
}

function extractContent(message: any): string {
  if (typeof message === 'string') return message;
  if (!message) return '';

  // ChatMessage format: { role, content: [...blocks] }
  if (Array.isArray(message.content)) {
    return message.content
      .map((block: any) => {
        if (block.type === 'text') return block.text || '';
        if (block.type === 'tool_use') return `[Tool: ${block.name}]`;
        if (block.type === 'tool_result') {
          const text = Array.isArray(block.content)
            ? block.content.map((c: any) => c.text || '').join('')
            : '';
          return text ? `[Result: ${text.slice(0, 200)}...]` : '[Tool result]';
        }
        return '';
      })
      .filter(Boolean)
      .join('\n');
  }

  if (typeof message.text === 'string') return message.text;
  return JSON.stringify(message, null, 2);
}

function formatTime(ts: number): string {
  if (!ts) return '—';
  return new Date(ts).toLocaleString();
}

function formatTokens(n: number): string {
  if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1) + 'K';
  return String(n);
}

export default App;
