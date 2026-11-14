import type { Session } from '@supabase/supabase-js';
import {
  AiInsightPayload,
  Client,
  ClientUnit,
  Closing,
  PlatformSettings,
  PrintJobSummary,
  Printer,
  Profile,
  ServiceTicket,
  SupplyAlert,
  TelemetryReading,
  Tenant,
} from '../types/models';
import { MASTER_ADMIN_EMAIL, sb } from '../services/supabaseClient';
import { fetchServiceTickets } from '../services/portalService';
import { getCurrentMonthPrefix } from '../utils/format';

export type AppTabId =
  | 'clients'
  | 'reports'
  | 'tickets'
  | 'grafica'
  | 'company'
  | 'plans'
  | 'master';

export type ReportsSubTabId = 'closing' | 'supplies' | 'ai';

export interface AppState {
  session: Session | null;
  profile: Profile | null;
  tenant: Tenant | null;
  platformSettings: PlatformSettings;
  clients: Client[];
  units: ClientUnit[];
  printers: Printer[];
  closings: Closing[];
  telemetryReadings: TelemetryReading[];
  supplyAlerts: SupplyAlert[];
  printJobsSummary: PrintJobSummary[];
  serviceTickets: ServiceTicket[];
  latestAiInsight: AiInsightPayload | null;
  currentTab: AppTabId;
  currentReportsSubTab: ReportsSubTabId;
  selectedPeriodMonth: string;
  activeClientForInvoice: Client | null;
  activeDueClientId: string | null;
  activeSpareClientId: string | null;
  activePortalPrinterId: string | null;
  forceShowOnboarding: boolean;
  allPlatformTenants: Tenant[];
  allPlatformPrinters: Printer[];
}

export const DEFAULT_PLATFORM_SETTINGS: PlatformSettings = {
  id: 'default',
  pioneer_slots_total: 15,
  pioneer_slots_used: 0,
  enforce_min_prepaid_recharge: false,
  min_prepaid_recharge_brl: 30.0,
  asaas_min_charge_brl: 5.0,
};

export const appState: AppState = {
  session: null,
  profile: null,
  tenant: null,
  platformSettings: { ...DEFAULT_PLATFORM_SETTINGS },
  clients: [],
  units: [],
  printers: [],
  closings: [],
  telemetryReadings: [],
  supplyAlerts: [],
  printJobsSummary: [],
  serviceTickets: [],
  latestAiInsight: null,
  currentTab: 'clients',
  currentReportsSubTab: 'closing',
  selectedPeriodMonth: getCurrentMonthPrefix(),
  activeClientForInvoice: null,
  activeDueClientId: null,
  activeSpareClientId: null,
  activePortalPrinterId: null,
  forceShowOnboarding: false,
  allPlatformTenants: [],
  allPlatformPrinters: [],
};

export function byId<T extends HTMLElement = HTMLElement>(id: string): T | null {
  return document.getElementById(id) as T | null;
}

export function openModal(id: string): void {
  const el = byId(id);
  if (el) el.classList.remove('hidden');
}

export function closeModal(id: string): void {
  const el = byId(id);
  if (el) el.classList.add('hidden');
}

export function isMasterAdminUser(): boolean {
  const em = String(appState.session?.user?.email ?? '').toLowerCase().trim();
  return em === MASTER_ADMIN_EMAIL;
}

export function getActiveUnitPairingToken(): string {
  if (appState.units.length > 0 && appState.units[0].pairing_token) {
    return appState.units[0].pairing_token;
  }
  return '';
}

async function safeQueryList<T>(queryPromise: PromiseLike<{ data: T[] | null; error: { message: string } | null }>): Promise<T[]> {
  try {
    const res = await queryPromise;
    if (!res.error && Array.isArray(res.data)) {
      return res.data;
    }
    if (res.error) {
      console.warn('Consulta auxiliar ignorada:', res.error.message);
    }
  } catch (err) {
    console.warn('Exceção em consulta auxiliar:', err);
  }
  return [];
}

export async function refreshTenantRecord(): Promise<void> {
  const tenantId = appState.tenant?.id ?? appState.profile?.tenant_id;
  if (!tenantId) return;
  const { data, error } = await sb.from('tenants').select('*').eq('id', tenantId).single();
  if (!error && data) {
    appState.tenant = data as Tenant;
  }
}

export async function loadPlatformSettings(): Promise<void> {
  const { data, error } = await sb
    .from('platform_settings')
    .select('*')
    .eq('id', 'default')
    .maybeSingle();
  if (!error && data) {
    appState.platformSettings = data as PlatformSettings;
  }
}

export async function loadTenantData(): Promise<void> {
  if (!appState.session) return;
  const tenantId = appState.tenant?.id ?? appState.profile?.tenant_id ?? null;
  const thirtyDaysAgoIso = new Date(Date.now() - 30 * 24 * 60 * 60 * 1000).toISOString();

  let trQuery = sb
    .from('telemetry_readings')
    .select('printer_id, total_pages, mono_pages, color_pages, c_level, m_level, y_level, k_level, recorded_at')
    .gte('recorded_at', thirtyDaysAgoIso)
    .order('recorded_at', { ascending: true })
    .limit(5000);
  if (tenantId) trQuery = trQuery.eq('tenant_id', tenantId);

  let saQuery = sb
    .from('supply_alerts')
    .select('*')
    .order('created_at', { ascending: false })
    .limit(100);
  if (tenantId) saQuery = saQuery.eq('tenant_id', tenantId);

  let pjQuery = sb.from('print_jobs_summary').select('*').limit(1000);
  if (tenantId) pjQuery = pjQuery.eq('tenant_id', tenantId);

  let aiQuery = sb
    .from('ai_insights')
    .select('*')
    .order('created_at', { ascending: false })
    .limit(1);
  if (tenantId) aiQuery = aiQuery.eq('tenant_id', tenantId);

  const [cRes, uRes, pRes, clRes, trData, saData, pjData, aiData, ticketsData] = await Promise.all([
    sb.from('clients').select('*').order('created_at', { ascending: false }),
    sb.from('client_units').select('*').order('created_at', { ascending: false }),
    sb.from('printers').select('*').order('created_at', { ascending: false }),
    sb.from('closings').select('*').order('created_at', { ascending: false }),
    safeQueryList<TelemetryReading>(trQuery),
    safeQueryList<SupplyAlert>(saQuery),
    safeQueryList<PrintJobSummary>(pjQuery),
    safeQueryList<Record<string, unknown>>(aiQuery),
    tenantId ? fetchServiceTickets(tenantId) : Promise.resolve([]),
  ]);

  if (cRes.error) console.warn('Erro ao carregar clientes:', cRes.error.message);
  if (pRes.error) console.warn('Erro ao carregar impressoras:', pRes.error.message);

  appState.clients = (cRes.data ?? []) as Client[];
  appState.units = (uRes.data ?? []) as ClientUnit[];
  appState.printers = (pRes.data ?? []) as Printer[];
  appState.closings = (clRes.data ?? []) as Closing[];
  appState.telemetryReadings = trData;
  appState.supplyAlerts = saData;
  appState.printJobsSummary = pjData;
  appState.serviceTickets = ticketsData;

  if (aiData.length > 0 && !appState.latestAiInsight) {
    const row = aiData[0];
    const rawPayload = (row.insights_json ?? row.insights ?? row.payload ?? row) as AiInsightPayload;
    appState.latestAiInsight = rawPayload;
  }

  await Promise.all([refreshTenantRecord(), loadPlatformSettings()]);
}
