export function escapeHtml(str: unknown): string {
  return String(str ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

export function formatCurrencyBrl(value: number): string {
  const safe = Number.isFinite(value) ? value : 0;
  return safe.toLocaleString('pt-BR', { style: 'currency', currency: 'BRL' });
}

export function formatIntegerBr(value: number): string {
  const safe = Number.isFinite(value) ? value : 0;
  return Math.round(safe).toLocaleString('pt-BR');
}

export function normalizeDigits(value: string | null | undefined): string {
  return String(value ?? '').replace(/\D/g, '');
}

export function getCurrentMonthPrefix(now = new Date()): string {
  return `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}`;
}

export function formatElapsedSince(diffMs: number): string {
  if (!Number.isFinite(diffMs) || diffMs < 0) return 'Sem sinal';
  const totalMin = Math.floor(diffMs / 60000);
  if (totalMin < 1) return 'agora';
  if (totalMin < 60) return `${totalMin}m`;
  const hrs = Math.floor(totalMin / 60);
  const mins = totalMin % 60;
  if (hrs < 24) return mins > 0 ? `${hrs}h ${mins}m` : `${hrs}h`;
  const days = Math.floor(hrs / 24);
  const remHrs = hrs % 24;
  return remHrs > 0 ? `${days}d ${remHrs}h` : `${days}d`;
}

export function getErrorMessage(err: unknown, fallback = 'Erro inesperado.'): string {
  if (err instanceof Error && err.message) return err.message;
  if (typeof err === 'object' && err !== null && 'message' in err) {
    const msg = String((err as { message?: unknown }).message ?? '');
    if (msg) return msg;
  }
  if (typeof err === 'string' && err.trim()) return err;
  return fallback;
}

export function isMissingColumnError(err: unknown): boolean {
  if (!err || typeof err !== 'object') return false;
  const e = err as { code?: unknown; message?: unknown; details?: unknown; hint?: unknown };
  const msg = String(e.message ?? e.details ?? e.hint ?? '').toLowerCase();
  return String(e.code ?? '') === '42703' || msg.includes('column') || msg.includes('schema cache') || msg.includes('does not exist');
}
