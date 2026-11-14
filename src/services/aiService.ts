import { AiInsightPayload } from '../types/models';
import { getErrorMessage } from '../utils/format';
import { sb } from './supabaseClient';

export interface AiInvokeResult {
  ok: boolean;
  cached?: boolean;
  modelUsed?: string;
  insights?: AiInsightPayload;
  code?: string;
  message?: string;
}

export function normalizeAiPayload(raw: unknown): AiInsightPayload | null {
  if (!raw || typeof raw !== 'object') return null;
  let candidate = raw as Record<string, unknown>;
  if (candidate.insights && typeof candidate.insights === 'object') {
    candidate = candidate.insights as Record<string, unknown>;
  }
  return {
    executive_summary: String(candidate.executive_summary ?? ''),
    urgent_actions: Array.isArray(candidate.urgent_actions) ? candidate.urgent_actions : [],
    cost_saving_tips: Array.isArray(candidate.cost_saving_tips) ? candidate.cost_saving_tips : [],
    anomalies: Array.isArray(candidate.anomalies) ? candidate.anomalies : [],
    whatsapp_briefing_ptbr: String(candidate.whatsapp_briefing_ptbr ?? ''),
  };
}

export async function invokeCloudAiInsights(
  tenantId: string,
  forceRefresh = false,
): Promise<AiInvokeResult> {
  const { data, error } = await sb.functions.invoke<Record<string, unknown>>('gemini-insights', {
    body: {
      tenant_id: tenantId,
      force_refresh: forceRefresh,
    },
  });

  if (error) {
    return {
      ok: false,
      message: getErrorMessage(error, 'Serviço PrintMind Cloud AI temporariamente indisponível.'),
    };
  }
  if (!data || data.status === 'error') {
    return {
      ok: false,
      code: String(data?.code ?? ''),
      message: String(data?.message ?? 'Não foi possível gerar análise de IA.'),
    };
  }

  const parsed = normalizeAiPayload(data.insights ?? data);
  if (!parsed) {
    return { ok: false, message: 'Formato inválido retornado pelo Consultor IA.' };
  }
  return {
    ok: true,
    cached: Boolean(data.cached),
    modelUsed: String(data.model_used ?? ''),
    insights: parsed,
  };
}
