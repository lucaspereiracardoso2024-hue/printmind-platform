export type PlanTier = 'trial' | 'starter' | 'pro' | 'enterprise';

export type SubscriptionStatus =
  | 'trialing'
  | 'active'
  | 'pending_payment'
  | 'past_due'
  | 'canceled';

export type BillingMode =
  | 'inherit_client'
  | 'fixed_only'
  | 'fixed_plus_franchise'
  | 'per_page_only'
  | 'exempt';

export type MeasurementMode = 'exact' | 'state_only' | 'unknown';

export type SupplyState = 'ok' | 'low' | 'empty' | 'unknown';

export type TicketCategory =
  | 'toner'
  | 'atolagem'
  | 'qualidade'
  | 'offline'
  | 'chamado_tecnico'
  | 'outro';

export type TicketStatus = 'aberto' | 'em_atendimento' | 'resolvido' | 'cancelado';

export interface SupplyItem {
  name?: string;
  type?: string;
  supply_class?: string;
  color?: string;
  level?: number | null;
  max_capacity?: number | null;
  percent?: number | null;
  level_state?: string;
  measurement_mode?: MeasurementMode | string;
  supply_state?: SupplyState | string;
  part_number?: string;
  serial_number?: string;
  manufacture_date?: string;
  is_genuine?: boolean | null;
  PartNumber?: string;
  SerialNumber?: string;
  ManufactureDate?: string;
  IsGenuine?: boolean | null;
  MeasurementMode?: string;
  SupplyState?: string;
}

export interface SpareTonersStock {
  K: number;
  C: number;
  M: number;
  Y: number;
}

export interface Tenant {
  id: string;
  trade_name: string;
  legal_name?: string | null;
  cnpj_cpf?: string | null;
  email: string;
  phone?: string | null;
  city?: string | null;
  state_uf?: string | null;
  pix_key?: string | null;
  baas_wallet_id?: string | null;
  plan_tier: PlanTier | string;
  subscription_status: SubscriptionStatus | string;
  max_printers?: number | null;
  is_pioneer_tenant?: boolean;
  pioneer_courtesy_active?: boolean;
  trial_ends_at?: string | null;
  gateway_customer_id?: string | null;
  gateway_subscription_id?: string | null;
  operation_mode?: 'outsourcing' | 'grafica' | string;
  created_at?: string;
}

export interface Profile {
  id: string;
  tenant_id: string;
  full_name?: string | null;
  role?: string | null;
}

export interface Client {
  id: string;
  tenant_id: string;
  name: string;
  cnpj?: string | null;
  email_billing?: string | null;
  phone?: string | null;
  closing_day: number;
  contract_num?: string | null;
  base_rate: number;
  franchise_mono: number;
  extra_mono: number;
  franchise_color: number;
  extra_color: number;
  is_corporate?: boolean;
  units?: string[] | null;
  departments?: string[] | null;
  billing_pool_group?: string | null;
  tech_deduction_mono?: number | null;
  tech_deduction_color?: number | null;
  replaced_delta_mono?: number | null;
  replaced_delta_color?: number | null;
  adjustment_note?: string | null;
  spare_toners?: Partial<SpareTonersStock> | null;
  baas_customer_id?: string | null;
  created_at?: string;
}

export interface ClientUnit {
  id: string;
  tenant_id: string;
  client_id?: string | null;
  unit_name: string;
  pairing_token?: string | null;
  scan_subnets?: string[] | null;
  agent_version?: string | null;
  wpp_enabled?: boolean | null;
  last_heartbeat?: string | null;
  update_msi_url?: string | null;
  update_sha256?: string | null;
  update_exe_url?: string | null;
  update_exe_sha256?: string | null;
  update_version?: string | null;
  created_at?: string;
}

export interface Printer {
  id: string;
  tenant_id: string;
  client_id?: string | null;
  unit_id?: string | null;
  unit?: string | null;
  department?: string | null;
  location?: string | null;
  ownership?: 'locada' | 'propria' | 'cliente' | string | null;
  model: string;
  serial: string;
  ip?: string | null;
  usb_port?: string | null;
  mac_address?: string | null;
  discovery_source?: string | null;
  initial_mono_pages?: number | null;
  mono_pages?: number | null;
  initial_color_pages?: number | null;
  color_pages?: number | null;
  initial_total_pages?: number | null;
  total_pages?: number | null;
  is_color_capable?: boolean | null;
  status?: string | null;
  error_flags?: string[] | null;
  supplies?: SupplyItem[] | null;
  billing_exempt?: boolean | null;
  billing_mode?: BillingMode | string | null;
  contract_base_rate?: number | null;
  contract_franchise_mono?: number | null;
  contract_extra_mono?: number | null;
  contract_franchise_color?: number | null;
  contract_extra_color?: number | null;
  counter_reset_detected?: boolean | null;
  last_reset_at?: string | null;
  replaced_delta_mono?: number | null;
  replaced_delta_color?: number | null;
  wpp_at_risk?: boolean | null;
  last_seen?: string | null;
  created_at?: string;
}

export interface TelemetryReading {
  printer_id: string;
  total_pages: number | null;
  mono_pages: number | null;
  color_pages: number | null;
  c_level?: number | null;
  m_level?: number | null;
  y_level?: number | null;
  k_level?: number | null;
  recorded_at: string;
}

export interface ClosingPrinterItemSnapshot {
  printer_id: string;
  model: string;
  serial: string;
  unit: string;
  department: string;
  initial_mono: number;
  final_mono: number;
  delta_mono: number;
  initial_color: number;
  final_color: number;
  delta_color: number;
  delta_total: number;
  start_source: string;
  applied_billing_mode: BillingMode;
  subtotal_brl: number;
  is_exempt: boolean;
  counter_reset_detected: boolean;
}

export interface Closing {
  id: string;
  tenant_id: string;
  client_id: string;
  status: 'aberto' | 'congelado' | 'faturado' | 'pago' | string;
  total_pages: number;
  total_amount: number;
  period_month?: string | null;
  client_name_snapshot?: string | null;
  client_cnpj_snapshot?: string | null;
  pricing_rules_snapshot?: Record<string, unknown> | null;
  printer_items_snapshot?: ClosingPrinterItemSnapshot[] | null;
  due_date?: string | null;
  snapshot_frozen?: boolean;
  frozen_at?: string | null;
  snapshot_hash?: string | null;
  pix_payload?: string | null;
  asaas_payment_id?: string | null;
  invoice_url?: string | null;
  created_at?: string;
  updated_at?: string;
}

export interface SupplyAlert {
  id: string;
  tenant_id: string;
  printer_id?: string | null;
  client_id?: string | null;
  alert_type?: string | null;
  type?: string | null;
  supply_name?: string | null;
  printer_model?: string | null;
  serial?: string | null;
  message?: string | null;
  details?: string | null;
  new_level_pct?: number | null;
  resolved?: boolean | null;
  status?: string | null;
  created_at?: string;
}

export interface PrintJobSummary {
  id?: string;
  tenant_id?: string;
  client_id?: string | null;
  printer_id?: string | null;
  period_month?: string | null;
  month_year?: string | null;
  job_date?: string | null;
  recorded_at?: string | null;
  created_at?: string | null;
  department?: string | null;
  sector?: string | null;
  machine_name?: string | null;
  pc_name?: string | null;
  hostname?: string | null;
  workstation?: string | null;
  computer_name?: string | null;
  user_name?: string | null;
  username?: string | null;
  os_user?: string | null;
  jobs_count?: number | null;
  job_count?: number | null;
  mono_pages?: number | null;
  color_pages?: number | null;
  duplex_pages?: number | null;
  total_pages?: number | null;
  pages?: number | null;
}

export interface ServiceTicket {
  id: string;
  tenant_id: string;
  printer_id?: string | null;
  client_id?: string | null;
  protocol_code: string;
  requester_name: string;
  requester_phone?: string | null;
  category: TicketCategory | string;
  status: TicketStatus | string;
  description: string;
  resolution_note?: string | null;
  created_at: string;
  updated_at?: string;
}

export type PortalTokenScope = 'full_client' | 'printer_ticket_only';

export interface PortalTokenIssueResult {
  status: 'success' | 'error';
  token_id?: string;
  token_raw?: string;
  token_scope?: PortalTokenScope;
  reused?: boolean;
  message?: string;
}

export interface PortalPublicPrinter {
  id: string;
  model: string;
  serial: string;
  status: string;
  unit?: string | null;
  department?: string | null;
  location?: string | null;
  is_color_capable?: boolean | null;
  supplies?: SupplyItem[] | null;
  last_seen?: string | null;
}

export interface PortalPublicData {
  status: 'success' | 'error';
  message?: string;
  token_scope?: PortalTokenScope;
  requires_pin?: boolean;
  provider_name?: string;
  provider_phone?: string;
  locked_until?: string | null;
  client?: {
    id: string;
    name: string;
  } | null;
  printer?: PortalPublicPrinter;
  printers?: PortalPublicPrinter[];
  recent_tickets?: Array<{
    protocol_code: string;
    printer_id?: string | null;
    printer_model?: string | null;
    printer_serial?: string | null;
    category: string;
    status: string;
    description: string;
    resolution_note?: string | null;
    created_at: string;
  }>;
}

export interface PortalCreateTicketResult {
  status: 'success' | 'duplicate' | 'invalid_pin' | 'locked' | 'rate_limited' | 'error';
  ticket_id?: string;
  protocol_code?: string;
  message?: string;
}

export interface UrgentAction {
  priority: 'alta' | 'media' | 'baixa' | string;
  target: string;
  action: string;
  reason: string;
}

export interface CostSavingTip {
  title: string;
  estimated_impact: string;
  recommendation: string;
}

export interface FleetAnomaly {
  type: string;
  severity: 'critical' | 'warning' | 'info' | string;
  target: string;
  detail: string;
}

export interface AiInsightPayload {
  executive_summary: string;
  urgent_actions: Array<UrgentAction | string>;
  cost_saving_tips: Array<CostSavingTip | string>;
  anomalies: Array<FleetAnomaly | string>;
  whatsapp_briefing_ptbr: string;
}

export interface PlatformSettings {
  id: string;
  pioneer_slots_total: number;
  pioneer_slots_used: number;
  enforce_min_prepaid_recharge: boolean;
  min_prepaid_recharge_brl: number;
  asaas_min_charge_brl: number;
}

export interface AsaasQuoteResult {
  status: 'success' | 'error';
  code?: string;
  message?: string;
  plan_tier: PlanTier;
  rate_per_printer: number;
  non_exempt_printers?: number;
  free_tier_printers?: number;
  billable_printers: number;
  courtesy_max_printers?: number;
  raw_amount: number;
  charged_amount?: number;
  asaas_min_charge_brl: number;
  enforce_min_prepaid_recharge?: boolean;
  min_prepaid_recharge_brl?: number;
  pioneer_slots_remaining: number;
  pioneer_eligible: boolean;
  will_use_pioneer_courtesy: boolean;
  will_use_prepaid_recharge?: boolean;
  prepaid_recharge_amount_brl?: number;
  below_minimum_blocked: boolean;
}

export interface AsaasCheckoutResult {
  status: 'free_tier' | 'pioneer_courtesy' | 'pending_payment' | 'canceled' | 'error';
  code?: string;
  message?: string;
  plan_tier?: PlanTier;
  non_exempt_printers?: number;
  billable_printers?: number;
  max_printers?: number;
  courtesy_max_printers?: number;
  raw_amount?: number;
  min_charge_brl?: number;
  charged_amount?: number;
  is_prepaid_recharge?: boolean;
  subscription_id?: string;
  payment_id?: string | null;
  invoice_url?: string | null;
  pix_payload?: string | null;
  pix_qr_base64?: string | null;
}

export interface AsaasSplitResult {
  status: 'created' | 'error';
  code?: string;
  message?: string;
  payment_id?: string;
  invoice_url?: string | null;
  pix_payload?: string | null;
  pix_qr_base64?: string | null;
  charged_amount?: number;
}
