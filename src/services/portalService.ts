import {
  PortalCreateTicketResult,
  PortalPublicData,
  PortalTokenIssueResult,
  PortalTokenScope,
  ServiceTicket,
  TicketCategory,
  TicketStatus,
} from '../types/models';
import { getErrorMessage } from '../utils/format';
import { sb } from './supabaseClient';

export async function getOrCreatePortalToken(params: {
  printerId?: string | null;
  clientId?: string | null;
  scope?: PortalTokenScope;
  pin?: string;
  rotate?: boolean;
  label?: string;
}): Promise<PortalTokenIssueResult> {
  const scope: PortalTokenScope =
    params.scope ?? (params.clientId && !params.printerId ? 'full_client' : 'printer_ticket_only');
  const cleanPin = params.pin !== undefined && params.pin.trim() !== '' ? params.pin.trim() : null;
  const { data, error } = await sb.rpc('get_or_create_portal_token', {
    p_printer_id: scope === 'printer_ticket_only' ? params.printerId ?? null : null,
    p_pin: cleanPin,
    p_rotate: Boolean(params.rotate),
    p_label: params.label ?? null,
    p_scope: scope,
    p_client_id: scope === 'full_client' ? params.clientId ?? null : null,
  });

  if (error) {
    return { status: 'error', message: getErrorMessage(error, 'Falha ao gerar token do portal.') };
  }
  if (!data || typeof data !== 'object') {
    return { status: 'error', message: 'Resposta inválida ao emitir token.' };
  }
  return data as PortalTokenIssueResult;
}

export async function getPortalDataByToken(token: string): Promise<PortalPublicData> {
  const { data, error } = await sb.rpc('get_portal_data_by_token', {
    p_token: token,
  });
  if (error) {
    return { status: 'error', message: getErrorMessage(error, 'Não foi possível validar o link ou QR Code.') };
  }
  if (!data || typeof data !== 'object') {
    return { status: 'error', message: 'Link ou QR Code inválido.' };
  }
  return data as PortalPublicData;
}

export async function createPortalTicket(params: {
  token: string;
  pin: string;
  requesterName: string;
  requesterPhone?: string;
  category: TicketCategory;
  description: string;
  printerId?: string | null;
}): Promise<PortalCreateTicketResult> {
  const { data, error } = await sb.rpc('create_portal_ticket_by_token', {
    p_token: params.token,
    p_pin: params.pin,
    p_requester_name: params.requesterName,
    p_requester_phone: params.requesterPhone ?? null,
    p_category: params.category,
    p_description: params.description,
    p_printer_id: params.printerId ?? null,
  });
  if (error) {
    return { status: 'error', message: getErrorMessage(error, 'Erro ao abrir chamado.') };
  }
  if (!data || typeof data !== 'object') {
    return { status: 'error', message: 'Resposta inesperada do servidor.' };
  }
  return data as PortalCreateTicketResult;
}

export const createPortalTicketByToken = createPortalTicket;

export async function fetchServiceTickets(tenantId: string): Promise<ServiceTicket[]> {
  const { data, error } = await sb
    .from('service_tickets')
    .select('*')
    .eq('tenant_id', tenantId)
    .order('created_at', { ascending: false })
    .limit(200);
  if (error) {
    console.warn('Erro ao carregar chamados do portal:', error.message);
    return [];
  }
  return (data ?? []) as ServiceTicket[];
}

export async function updateServiceTicketStatus(
  ticketId: string,
  status: TicketStatus,
  resolutionNote?: string,
): Promise<{ ok: boolean; message?: string }> {
  const { error } = await sb
    .from('service_tickets')
    .update({
      status,
      resolution_note: resolutionNote ?? null,
      updated_at: new Date().toISOString(),
    })
    .eq('id', ticketId);
  if (error) {
    return { ok: false, message: error.message };
  }
  return { ok: true };
}
