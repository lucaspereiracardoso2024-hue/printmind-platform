import {
  AsaasCheckoutResult,
  AsaasQuoteResult,
  AsaasSplitResult,
  Client,
  Closing,
  PlanTier,
} from '../types/models';
import {
  buildFreezeSnapshotPayload,
  ClientComputedMetrics,
} from '../domain/billingEngine';
import { sb } from './supabaseClient';
import { getErrorMessage } from '../utils/format';

export async function quoteSaasPlan(
  tenantId: string,
  planTier: Exclude<PlanTier, 'trial'>,
): Promise<AsaasQuoteResult> {
  const { data, error } = await sb.functions.invoke<AsaasQuoteResult>('asaas-checkout', {
    body: {
      action: 'quote_plan',
      tenant_id: tenantId,
      plan_tier: planTier,
    },
  });
  if (error) {
    throw new Error(getErrorMessage(error, 'Falha ao cotar plano no servidor.'));
  }
  if (!data || data.status === 'error') {
    throw new Error(data?.message ?? 'Cotação indisponível no momento.');
  }
  return data;
}

export async function createSaasCheckout(params: {
  tenantId: string;
  planTier: Exclude<PlanTier, 'trial'>;
  billingType: 'PIX' | 'CREDIT_CARD' | 'BOLETO';
}): Promise<AsaasCheckoutResult> {
  const { data, error } = await sb.functions.invoke<AsaasCheckoutResult>('asaas-checkout', {
    body: {
      action: 'create_saas_checkout',
      tenant_id: params.tenantId,
      plan_tier: params.planTier,
      billing_type: params.billingType,
    },
  });

  if (error) {
    const contextMsg = getErrorMessage(error, 'Erro ao iniciar checkout no Asaas.');
    return { status: 'error', message: contextMsg };
  }
  if (!data) {
    return { status: 'error', message: 'Resposta vazia do servidor de checkout.' };
  }
  return data;
}

export async function cancelSaasSubscription(tenantId: string): Promise<{ ok: boolean; message: string }> {
  const { data, error } = await sb.functions.invoke<{ status?: string; message?: string }>('asaas-checkout', {
    body: {
      action: 'cancel_saas_subscription',
      tenant_id: tenantId,
    },
  });
  if (error) {
    return { ok: false, message: getErrorMessage(error, 'Erro ao cancelar assinatura.') };
  }
  return {
    ok: data?.status === 'canceled',
    message: data?.message ?? 'Assinatura cancelada com sucesso.',
  };
}

export async function emitSplitCharge(params: {
  tenantId: string;
  clientId: string;
  closingId?: string;
  clientName: string;
  totalAmount: number;
  dueDateIso: string;
  billingType: 'PIX' | 'BOLETO' | 'CREDIT_CARD';
  description?: string;
}): Promise<AsaasSplitResult> {
  const { data, error } = await sb.functions.invoke<AsaasSplitResult>('asaas-checkout', {
    body: {
      action: 'emit_split_charge',
      tenant_id: params.tenantId,
      client_id: params.clientId,
      closing_id: params.closingId,
      client_name: params.clientName,
      total_amount: params.totalAmount,
      due_date: params.dueDateIso,
      billing_type: params.billingType,
      description: params.description,
    },
  });
  if (error) {
    return { status: 'error', message: getErrorMessage(error, 'Falha ao emitir cobrança via Asaas.') };
  }
  if (!data) {
    return { status: 'error', message: 'Resposta vazia da Edge Function asaas-checkout.' };
  }
  return data;
}

export async function toggleClientClosingFreeze(params: {
  tenantId: string;
  client: Client;
  metrics: ClientComputedMetrics;
  pixPayload: string | null;
  periodMonth: string;
  dueDateIso: string;
}): Promise<{ ok: boolean; closing?: Closing; message?: string }> {
  const { tenantId, client, metrics, pixPayload, periodMonth, dueDateIso } = params;
  const targetFrozen = !metrics.isFrozen;
  const nextStatus = targetFrozen ? 'congelado' : 'aberto';

  if (targetFrozen) {
    const snap = await buildFreezeSnapshotPayload({
      client,
      metrics,
      pixPayload,
      periodMonth,
      dueDateIso,
    });
    const nowIso = new Date().toISOString();

    const payload = {
      tenant_id: tenantId,
      client_id: client.id,
      client_name: client.name,
      month_year: periodMonth,
      status: 'congelado',
      total_pages: metrics.dTotal,
      mono_pages: metrics.dMono,
      color_pages: metrics.dColor,
      technical_discount_mono: metrics.techDedMono,
      technical_discount_color: metrics.techDedColor,
      base_rate: client.base_rate,
      extra_mono: client.extra_mono,
      extra_color: client.extra_color,
      total_amount: metrics.total,
      net_tenant_amount: metrics.total,
      period_month: periodMonth,
      client_name_snapshot: client.name,
      client_cnpj_snapshot: client.cnpj ?? null,
      pricing_rules_snapshot: snap.pricingRulesSnapshot,
      printer_items_snapshot: snap.printerItemsSnapshot,
      due_date: dueDateIso,
      snapshot_frozen: true,
      frozen_at: nowIso,
      snapshot_hash: snap.snapshotHash,
      pix_payload: pixPayload,
      pix_copy_paste: pixPayload,
      updated_at: nowIso,
    };

    let savedClosing: Closing | null = null;
    if (metrics.closing) {
      const { data, error } = await sb
        .from('closings')
        .update(payload)
        .eq('id', metrics.closing.id)
        .select('*')
        .single();
      if (error) return { ok: false, message: error.message };
      savedClosing = data as Closing;
    } else {
      const { data, error } = await sb.from('closings').insert(payload).select('*').single();
      if (error) return { ok: false, message: error.message };
      savedClosing = data as Closing;
    }

    if (savedClosing && snap.printerItemsSnapshot.length > 0) {
      const rows = snap.printerItemsSnapshot.map(item => ({
        tenant_id: tenantId,
        closing_id: savedClosing.id,
        printer_id: item.printer_id,
        model_snapshot: item.model,
        serial_snapshot: item.serial,
        unit_snapshot: item.unit,
        department_snapshot: item.department,
        initial_mono: item.initial_mono,
        final_mono: item.final_mono,
        delta_mono: item.delta_mono,
        initial_color: item.initial_color,
        final_color: item.final_color,
        delta_color: item.delta_color,
        applied_billing_mode: item.applied_billing_mode,
        subtotal_brl: item.subtotal_brl,
        is_exempt: item.is_exempt,
      }));
      const { error: itemsErr } = await sb.from('closing_printer_items').insert(rows);
      if (itemsErr) {
        console.warn('Falha ao gravar closing_printer_items:', itemsErr.message);
      }
    }

    return { ok: true, closing: savedClosing ?? undefined };
  }

  // Unfreeze attempt (note DB trigger allows status transition congelado->aberto ONLY when unlocking cleanly if allowed;
  // if DB trigger blocks frozen financial edits, display clear trigger explanation)
  if (!metrics.closing) {
    return { ok: false, message: 'Nenhum fechamento encontrado para descongelar.' };
  }
  const { data, error } = await sb
    .from('closings')
    .update({ status: nextStatus, snapshot_frozen: false, updated_at: new Date().toISOString() })
    .eq('id', metrics.closing.id)
    .select('*')
    .single();
  if (error) {
    return {
      ok: false,
      message:
        error.message.includes('immutable') || error.message.includes('congelado')
          ? 'Fechamento congelado é imutável no banco por integridade contábil.'
          : error.message,
    };
  }
  return { ok: true, closing: data as Closing };
}
