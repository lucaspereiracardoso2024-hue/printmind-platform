import {
  BillingMode,
  Client,
  Closing,
  ClosingPrinterItemSnapshot,
  PlanTier,
  Printer,
  PrintJobSummary,
  TelemetryReading,
} from '../types/models';
import { getCurrentMonthPrefix } from '../utils/format';

export const FREE_TRIAL_PRINTER_LIMIT = 2;
export const ASAAS_MIN_CHARGE_BRL = 5.0;
export const UNTRACKED_SPOOLER_DEPARTMENT_LABEL =
  'Cópias / Impressão Mobile / Direta (Não rastreado via Spooler)';

export function toCents(amountBrl: number | null | undefined): number {
  const numeric = Number(amountBrl ?? 0);
  if (!Number.isFinite(numeric)) return 0;
  return Math.round(numeric * 100);
}

export function centsToBrl(cents: number): number {
  if (!Number.isFinite(cents)) return 0;
  return Math.round(cents) / 100;
}

export function computePagesAmountCents(pages: number, rateBrl: number): number {
  const safePages = Math.max(0, Math.round(Number(pages || 0)));
  const safeRate = Math.max(0, Number(rateBrl || 0));
  if (safePages === 0 || safeRate === 0 || !Number.isFinite(safeRate)) return 0;
  const microRate = Math.round(safeRate * 1_000_000);
  return Math.round((safePages * microRate) / 10_000);
}

export function distributeCents(totalCents: number, weights: number[]): number[] {
  const count = weights.length;
  if (count === 0) return [];
  const targetCents = Math.round(Number(totalCents) || 0);
  if (targetCents === 0) return weights.map(() => 0);

  const sanitizedWeights = weights.map(w => {
    const n = Number(w);
    return Number.isFinite(n) && n > 0 ? n : 0;
  });
  const totalWeight = sanitizedWeights.reduce((acc, w) => acc + w, 0);
  const effectiveWeights = totalWeight > 0 ? sanitizedWeights : sanitizedWeights.map(() => 1);
  const effectiveTotalWeight = effectiveWeights.reduce((acc, w) => acc + w, 0);

  const sign = targetCents < 0 ? -1 : 1;
  const absTotal = Math.abs(targetCents);

  const entries = effectiveWeights.map((weight, index) => {
    const exactShare = (absTotal * weight) / effectiveTotalWeight;
    const floorShare = Math.floor(exactShare);
    return {
      index,
      floorShare,
      remainder: exactShare - floorShare,
    };
  });

  const allocatedFloorSum = entries.reduce((acc, entry) => acc + entry.floorShare, 0);
  let remaining = absTotal - allocatedFloorSum;

  const ranked = [...entries].sort((a, b) => {
    const diff = b.remainder - a.remainder;
    if (Math.abs(diff) > 1e-12) return diff;
    return a.index - b.index;
  });

  const result = entries.map(e => e.floorShare);
  let k = 0;
  while (remaining > 0 && ranked.length > 0) {
    result[ranked[k % ranked.length].index] += 1;
    remaining -= 1;
    k += 1;
  }
  while (remaining < 0 && ranked.length > 0) {
    const idx = ranked[ranked.length - 1 - (k % ranked.length)].index;
    if (result[idx] > 0) {
      result[idx] -= 1;
      remaining += 1;
    }
    k += 1;
    if (k > ranked.length * 4) break;
  }

  return sign < 0 ? result.map(v => (v === 0 ? 0 : -v)) : result;
}

export interface PlanDefinition {
  tier: Exclude<PlanTier, 'trial'>;
  name: string;
  rate: number;
  badge: string;
  subtitle: string;
  features: string[];
}

export const PLAN_CATALOG: Record<Exclude<PlanTier, 'trial'>, PlanDefinition> = {
  starter: {
    tier: 'starter',
    name: 'Plano 1 — Essencial',
    rate: 1.0,
    badge: 'Monitoramento e Contadores',
    subtitle: 'Ideal para começar sem depender de leitura manual ou fotos do visor.',
    features: [
      'Scanner Windows + Tray silencioso (Rede + USB)',
      'Painel de clientes, unidades e impressoras',
      'Contadores P&B e Colorido + Espelho Formal A4',
      'Exportação ERP (AgoraOS / Bling / Omie)',
    ],
  },
  pro: {
    tier: 'pro',
    name: 'Plano 2 — Gestão & Insumos',
    rate: 4.0,
    badge: 'Mais Popular para Locadoras',
    subtitle: 'Tudo do Essencial + auditoria de toners, pooling, Spooler e Portal QR.',
    features: [
      'Tudo do Plano Essencial',
      'Auditoria de trocas prematuras e caixa de resíduo',
      'Pool compartilhado, estoque reserva e contratos por máquina',
      'Portal QR do Tomador (chamados) + Consultor IA',
    ],
  },
  enterprise: {
    tier: 'enterprise',
    name: 'Plano 3 — Completo / Automação',
    rate: 6.0,
    badge: 'Operação Escalonada',
    subtitle: 'Tudo do Gestão + cobrança automatizada Asaas, Split e Console avançado.',
    features: [
      'Tudo do Plano Gestão & Insumos',
      'Cobrança automática PIX/Boleto via Asaas + Split',
      'Snapshots financeiros imutáveis com hash SHA-256',
      'Prioridade de suporte e rollout MSI via GPO',
    ],
  },
};

export interface PrinterPeriodMetrics {
  startTotal: number;
  finalTotal: number;
  startMono: number;
  finalMono: number;
  startColor: number;
  finalColor: number;
  startSource: '1ª leitura mês' | 'Base cadastro';
  deltaMono: number;
  deltaColor: number;
  deltaTotal: number;
  readingsCount: number;
  counterResetDetected: boolean;
}

export interface PrinterCostItem extends PrinterPeriodMetrics {
  printer: Printer;
  billingMode: BillingMode;
  isExempt: boolean;
  customAmountCents: number | null;
  customAmountBrl: number | null;
  subtotalCents: number;
  subtotalBrl: number;
}

export interface ClientComputedMetrics {
  linked: Printer[];
  printerItems: PrinterCostItem[];
  rawMono: number;
  rawColor: number;
  replacedMono: number;
  replacedColor: number;
  techDedMono: number;
  techDedColor: number;
  netMono: number;
  netColor: number;
  netTotal: number;
  dMono: number;
  dColor: number;
  dTotal: number;
  clientContractBaseCents: number;
  clientContractBase: number;
  exMonoCents: number;
  exMono: number;
  exColorCents: number;
  exColor: number;
  customMachinesAmountCents: number;
  customMachinesAmount: number;
  totalCents: number;
  total: number;
  closing: Closing | undefined;
  isFrozen: boolean;
  poolGroup: string;
  adjustmentNote: string;
}

export interface DepartmentAllocationItem {
  department: string;
  pages: number;
  percentage: number;
  amountCents: number;
  amountBrl: number;
  isUntracked: boolean;
}

export interface DepartmentAllocationResult {
  odometerTotalPages: number;
  totalSpoolerPages: number;
  untrackedPages: number;
  totalInvoiceCents: number;
  hasSpoolerData: boolean;
  allocations: DepartmentAllocationItem[];
}

export interface BillingPoolSummary {
  poolName: string;
  clientsCount: number;
  clientNames: string[];
  printersCount: number;
  netMono: number;
  netColor: number;
  netTotal: number;
  franchiseMono: number;
  franchiseColor: number;
  baseRateSum: number;
  individualTotalSum: number;
  pooledTotal: number;
  poolSavings: number;
}

export interface PlanQuoteSummary {
  tier: Exclude<PlanTier, 'trial'>;
  rate: number;
  billablePrinters: number;
  exemptPrinters: number;
  rawAmount: number;
  chargedAmountBrl: number;
  willUsePioneerCourtesy: boolean;
  willUsePrepaidRecharge: boolean;
  prepaidRechargeAmountBrl: number;
  belowMinimumBlocked: boolean;
}

export function normalizeBillingMode(printer: Printer): BillingMode {
  if (printer.billing_exempt) return 'exempt';
  const raw = String(printer.billing_mode ?? 'inherit_client').toLowerCase().trim();
  if (raw === 'fixed_monthly') return 'fixed_only';
  if (raw === 'minimum_plus_excess') return 'fixed_plus_franchise';
  if (raw === 'billing_exempt') return 'exempt';
  if (
    raw === 'fixed_only' ||
    raw === 'fixed_plus_franchise' ||
    raw === 'per_page_only' ||
    raw === 'exempt' ||
    raw === 'inherit_client'
  ) {
    return raw;
  }
  return 'inherit_client';
}

export function getBillablePrintersSummary(printers: Printer[]): {
  billable: Printer[];
  exempt: Printer[];
  exceedsFreeTrial: boolean;
} {
  const exempt: Printer[] = [];
  const billable: Printer[] = [];
  for (const p of printers) {
    const mode = normalizeBillingMode(p);
    if (mode === 'exempt') {
      exempt.push(p);
    } else {
      billable.push(p);
    }
  }
  return {
    billable,
    exempt,
    exceedsFreeTrial: billable.length > FREE_TRIAL_PRINTER_LIMIT,
  };
}

export function getPrinterPeriodMetrics(
  p: Printer,
  telemetryReadings: TelemetryReading[],
  monthPrefix = getCurrentMonthPrefix(),
): PrinterPeriodMetrics {
  const readings = telemetryReadings
    .filter(r => r.printer_id === p.id && String(r.recorded_at ?? '').startsWith(monthPrefix))
    .sort((a, b) => new Date(a.recorded_at).getTime() - new Date(b.recorded_at).getTime());

  const earliest = readings.length > 0 ? readings[0] : null;
  const startTotal =
    earliest && earliest.total_pages !== null && earliest.total_pages !== undefined
      ? Number(earliest.total_pages)
      : Number(p.initial_total_pages ?? 0);
  const startMono =
    earliest && earliest.mono_pages !== null && earliest.mono_pages !== undefined
      ? Number(earliest.mono_pages)
      : Number(p.initial_mono_pages ?? 0);
  const startColor =
    earliest && earliest.color_pages !== null && earliest.color_pages !== undefined
      ? Number(earliest.color_pages)
      : Number(p.initial_color_pages ?? 0);

  const finalTotal = Number(p.total_pages ?? 0);
  const finalMono = Number(p.mono_pages ?? 0);
  const finalColor = Number(p.color_pages ?? 0);

  const replacedMono = Math.max(0, Number(p.replaced_delta_mono ?? 0));
  const replacedColor = Math.max(0, Number(p.replaced_delta_color ?? 0));
  const counterResetDetected =
    Boolean(p.counter_reset_detected) ||
    finalTotal < startTotal ||
    finalMono < startMono ||
    finalColor < startColor;

  const deltaMono = Math.max(0, finalMono - startMono) + replacedMono;
  const deltaColor = Math.max(0, finalColor - startColor) + replacedColor;
  const rawTotalDelta = Math.max(0, finalTotal - startTotal) + replacedMono + replacedColor;
  const deltaTotal = rawTotalDelta > 0 ? rawTotalDelta : deltaMono + deltaColor;

  return {
    startTotal,
    finalTotal,
    startMono,
    finalMono,
    startColor,
    finalColor,
    startSource: earliest ? '1ª leitura mês' : 'Base cadastro',
    deltaMono,
    deltaColor,
    deltaTotal,
    readingsCount: readings.length,
    counterResetDetected,
  };
}

export function computeSinglePrinterCustomAmountCents(
  p: Printer,
  pm: PrinterPeriodMetrics,
  mode: BillingMode,
): number | null {
  if (mode === 'inherit_client') return null;
  if (mode === 'exempt') return 0;

  const fixedCents = toCents(Math.max(0, Number(p.contract_base_rate ?? 0)));
  const franqMono = Math.max(0, Math.round(Number(p.contract_franchise_mono ?? 0)));
  const extraMonoRate = Math.max(0, Number(p.contract_extra_mono ?? 0.05));
  const franqColor = Math.max(0, Math.round(Number(p.contract_franchise_color ?? 0)));
  const extraColorRate = Math.max(0, Number(p.contract_extra_color ?? 0.1));

  if (mode === 'fixed_only') {
    return fixedCents;
  }
  if (mode === 'per_page_only') {
    return (
      computePagesAmountCents(pm.deltaMono, extraMonoRate) +
      computePagesAmountCents(pm.deltaColor, extraColorRate)
    );
  }
  // fixed_plus_franchise
  const overMonoCents = computePagesAmountCents(Math.max(0, pm.deltaMono - franqMono), extraMonoRate);
  const overColorCents = computePagesAmountCents(Math.max(0, pm.deltaColor - franqColor), extraColorRate);
  return fixedCents + overMonoCents + overColorCents;
}

export function computeSinglePrinterCustomAmount(
  p: Printer,
  pm: PrinterPeriodMetrics,
  mode: BillingMode,
): number | null {
  const cents = computeSinglePrinterCustomAmountCents(p, pm, mode);
  return cents === null ? null : centsToBrl(cents);
}

export function computeClientMetrics(
  c: Client,
  printers: Printer[],
  telemetryReadings: TelemetryReading[],
  closings: Closing[],
  monthPrefix = getCurrentMonthPrefix(),
): ClientComputedMetrics {
  const linked = printers.filter(p => p.client_id === c.id);

  let rawMono = 0;
  let rawColor = 0;
  let inheritedMono = 0;
  let inheritedColor = 0;
  let customMachinesAmountCents = 0;
  let hasInheritedMachine = false;

  const baseItems = linked.map(p => {
    const pm = getPrinterPeriodMetrics(p, telemetryReadings, monthPrefix);
    const mode = normalizeBillingMode(p);
    const isExempt = mode === 'exempt';
    const customAmountCents = computeSinglePrinterCustomAmountCents(p, pm, mode);
    const customAmountBrl = customAmountCents === null ? null : centsToBrl(customAmountCents);

    if (!isExempt) {
      rawMono += pm.deltaMono;
      rawColor += pm.deltaColor;
    }

    if (mode === 'inherit_client') {
      hasInheritedMachine = true;
      inheritedMono += pm.deltaMono;
      inheritedColor += pm.deltaColor;
    } else if (customAmountCents !== null) {
      customMachinesAmountCents += customAmountCents;
    }

    return {
      ...pm,
      printer: p,
      billingMode: mode,
      isExempt,
      customAmountCents,
      customAmountBrl,
    };
  });

  const replacedMono = Math.max(0, Math.round(Number(c.replaced_delta_mono ?? 0)));
  const replacedColor = Math.max(0, Math.round(Number(c.replaced_delta_color ?? 0)));
  const techDedMono = Math.max(0, Math.round(Number(c.tech_deduction_mono ?? 0)));
  const techDedColor = Math.max(0, Math.round(Number(c.tech_deduction_color ?? 0)));

  const netMono = Math.max(0, rawMono + replacedMono - techDedMono);
  const netColor = Math.max(0, rawColor + replacedColor - techDedColor);
  const netTotal = netMono + netColor;

  const contractMonoBase =
    linked.length === 0 || hasInheritedMachine
      ? Math.max(0, inheritedMono + replacedMono - techDedMono)
      : 0;
  const contractColorBase =
    linked.length === 0 || hasInheritedMachine
      ? Math.max(0, inheritedColor + replacedColor - techDedColor)
      : 0;

  const extraMonoRate = Number(c.extra_mono ?? 0.05);
  const extraColorRate = Number(c.extra_color ?? 0.1);
  const exMonoCents = computePagesAmountCents(
    Math.max(0, contractMonoBase - Math.max(0, Math.round(Number(c.franchise_mono ?? 0)))),
    extraMonoRate,
  );
  const exColorCents = computePagesAmountCents(
    Math.max(0, contractColorBase - Math.max(0, Math.round(Number(c.franchise_color ?? 0)))),
    extraColorRate,
  );

  const clientContractBaseCents =
    linked.length === 0 || hasInheritedMachine ? toCents(Math.max(0, Number(c.base_rate ?? 0))) : 0;
  const totalCents = clientContractBaseCents + exMonoCents + exColorCents + customMachinesAmountCents;

  const inheritedIndices: number[] = [];
  baseItems.forEach((item, index) => {
    if (item.billingMode === 'inherit_client') {
      inheritedIndices.push(index);
    }
  });

  const inheritedBaseShares = distributeCents(
    clientContractBaseCents,
    inheritedIndices.map(() => 1),
  );
  const inheritedMonoShares = distributeCents(
    exMonoCents,
    inheritedIndices.map(idx => baseItems[idx].deltaMono),
  );
  const inheritedColorShares = distributeCents(
    exColorCents,
    inheritedIndices.map(idx => baseItems[idx].deltaColor),
  );

  const inheritedPositionByIndex = new Map<number, number>();
  inheritedIndices.forEach((itemIdx, pos) => {
    inheritedPositionByIndex.set(itemIdx, pos);
  });

  const printerItems: PrinterCostItem[] = baseItems.map((item, idx) => {
    let subtotalCents = 0;
    if (item.isExempt) {
      subtotalCents = 0;
    } else if (item.customAmountCents !== null) {
      subtotalCents = item.customAmountCents;
    } else {
      const pos = inheritedPositionByIndex.get(idx);
      if (pos !== undefined) {
        subtotalCents =
          inheritedBaseShares[pos] + inheritedMonoShares[pos] + inheritedColorShares[pos];
      }
    }
    return {
      ...item,
      subtotalCents,
      subtotalBrl: centsToBrl(subtotalCents),
    };
  });

  const cl =
    closings.find(x => x.client_id === c.id && (!x.period_month || x.period_month === monthPrefix)) ??
    closings.find(x => x.client_id === c.id);
  const isFrozen = Boolean(cl && (cl.status === 'congelado' || cl.snapshot_frozen));

  return {
    linked,
    printerItems,
    rawMono,
    rawColor,
    replacedMono,
    replacedColor,
    techDedMono,
    techDedColor,
    netMono,
    netColor,
    netTotal,
    dMono: netMono,
    dColor: netColor,
    dTotal: netTotal,
    clientContractBaseCents,
    clientContractBase: centsToBrl(clientContractBaseCents),
    exMonoCents,
    exMono: centsToBrl(exMonoCents),
    exColorCents,
    exColor: centsToBrl(exColorCents),
    customMachinesAmountCents,
    customMachinesAmount: centsToBrl(customMachinesAmountCents),
    totalCents,
    total: centsToBrl(totalCents),
    closing: cl,
    isFrozen,
    poolGroup: String(c.billing_pool_group ?? '').trim(),
    adjustmentNote: String(c.adjustment_note ?? '').trim(),
  };
}

export function computeBillingPoolsSummary(
  clients: Client[],
  printers: Printer[],
  telemetryReadings: TelemetryReading[],
  closings: Closing[],
  monthPrefix = getCurrentMonthPrefix(),
): BillingPoolSummary[] {
  const groups: Record<string, { poolName: string; members: Array<{ client: Client; metrics: ClientComputedMetrics }> }> =
    {};

  for (const c of clients) {
    const name = String(c.billing_pool_group ?? '').trim();
    if (!name) continue;
    const key = name.toLowerCase();
    if (!groups[key]) {
      groups[key] = { poolName: name, members: [] };
    }
    groups[key].members.push({
      client: c,
      metrics: computeClientMetrics(c, printers, telemetryReadings, closings, monthPrefix),
    });
  }

  return Object.values(groups)
    .filter(g => g.members.length >= 2)
    .map(g => {
      let printersCount = 0;
      let netMono = 0;
      let netColor = 0;
      let poolMono = 0;
      let poolColor = 0;
      let franchiseMono = 0;
      let franchiseColor = 0;
      let baseRateSumCents = 0;
      let customMachinesSumCents = 0;
      let individualTotalSumCents = 0;
      let extraMonoRate = 0.05;
      let extraColorRate = 0.1;

      g.members.forEach(({ client: c, metrics: m }, idx) => {
        printersCount += m.linked.length;
        netMono += m.netMono;
        netColor += m.netColor;
        const inherited = m.printerItems.filter(i => i.billingMode === 'inherit_client');
        if (m.linked.length === 0 || inherited.length > 0) {
          poolMono += Math.max(0, inherited.reduce((s, i) => s + i.deltaMono, 0) + m.replacedMono - m.techDedMono);
          poolColor += Math.max(0, inherited.reduce((s, i) => s + i.deltaColor, 0) + m.replacedColor - m.techDedColor);
        }
        franchiseMono += Math.max(0, Math.round(Number(c.franchise_mono ?? 0)));
        franchiseColor += Math.max(0, Math.round(Number(c.franchise_color ?? 0)));
        baseRateSumCents += m.clientContractBaseCents;
        customMachinesSumCents += m.customMachinesAmountCents;
        individualTotalSumCents += m.totalCents;
        if (idx === 0) {
          extraMonoRate = Number(c.extra_mono ?? 0.05);
          extraColorRate = Number(c.extra_color ?? 0.1);
        }
      });

      const pooledExMonoCents = computePagesAmountCents(Math.max(0, poolMono - franchiseMono), extraMonoRate);
      const pooledExColorCents = computePagesAmountCents(Math.max(0, poolColor - franchiseColor), extraColorRate);
      const pooledTotalCents = baseRateSumCents + pooledExMonoCents + pooledExColorCents + customMachinesSumCents;
      const poolSavingsCents = Math.max(0, individualTotalSumCents - pooledTotalCents);

      return {
        poolName: g.poolName,
        clientsCount: g.members.length,
        clientNames: g.members.map(x => x.client.name),
        printersCount,
        netMono,
        netColor,
        netTotal: netMono + netColor,
        franchiseMono,
        franchiseColor,
        baseRateSum: centsToBrl(baseRateSumCents),
        individualTotalSum: centsToBrl(individualTotalSumCents),
        pooledTotal: centsToBrl(pooledTotalCents),
        poolSavings: centsToBrl(poolSavingsCents),
      };
    });
}

export async function computeSha256Hex(text: string): Promise<string> {
  if (typeof crypto !== 'undefined' && crypto.subtle) {
    const bytes = new TextEncoder().encode(text);
    const digest = await crypto.subtle.digest('SHA-256', bytes);
    return Array.from(new Uint8Array(digest))
      .map(b => b.toString(16).padStart(2, '0'))
      .join('');
  }
  // Fallback FNV/hash should never normally execute in browser/Node20+ (both have crypto.subtle)
  let h = 2166136261;
  for (let i = 0; i < text.length; i++) {
    h ^= text.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return (h >>> 0).toString(16).padStart(64, '0');
}

export function computePrinterInvoiceSubtotal(params: {
  metrics: ClientComputedMetrics;
  item: PrinterCostItem;
}): number {
  const { metrics, item } = params;
  if (item.isExempt) return 0;
  if (typeof item.subtotalCents === 'number' && Number.isFinite(item.subtotalCents)) {
    return centsToBrl(item.subtotalCents);
  }
  if (typeof item.customAmountCents === 'number' && Number.isFinite(item.customAmountCents)) {
    return centsToBrl(item.customAmountCents);
  }
  if (item.customAmountBrl !== null) {
    return centsToBrl(toCents(item.customAmountBrl));
  }

  const inheritedItems = metrics.printerItems.filter(i => i.billingMode === 'inherit_client');
  if (inheritedItems.length === 0) return 0;

  let itemIdx = inheritedItems.indexOf(item);
  if (itemIdx < 0) {
    itemIdx = inheritedItems.findIndex(i => i.printer.id === item.printer.id);
  }
  if (itemIdx < 0) return 0;

  const baseCents =
    typeof metrics.clientContractBaseCents === 'number'
      ? metrics.clientContractBaseCents
      : toCents(metrics.clientContractBase);
  const monoCents =
    typeof metrics.exMonoCents === 'number' ? metrics.exMonoCents : toCents(metrics.exMono);
  const colorCents =
    typeof metrics.exColorCents === 'number' ? metrics.exColorCents : toCents(metrics.exColor);

  const baseShares = distributeCents(
    baseCents,
    inheritedItems.map(() => 1),
  );
  const monoShares = distributeCents(
    monoCents,
    inheritedItems.map(i => i.deltaMono),
  );
  const colorShares = distributeCents(
    colorCents,
    inheritedItems.map(i => i.deltaColor),
  );

  return centsToBrl(baseShares[itemIdx] + monoShares[itemIdx] + colorShares[itemIdx]);
}

export function calculateDepartmentAllocation(params: {
  odometerTotalPages: number;
  totalInvoiceCents: number;
  printers: Printer[];
  spoolerJobs: PrintJobSummary[];
  periodMonth?: string;
}): DepartmentAllocationResult {
  const odometerTotalPages = Math.max(0, Math.round(Number(params.odometerTotalPages || 0)));
  const totalInvoiceCents = Math.max(0, Math.round(Number(params.totalInvoiceCents || 0)));
  const printers = params.printers ?? [];
  const spoolerJobs = params.spoolerJobs ?? [];
  const periodMonth = params.periodMonth;

  const printerIds = new Set(printers.map(p => p.id));
  const clientIds = new Set(
    printers.map(p => p.client_id).filter((id): id is string => Boolean(id)),
  );
  const printerDeptMap = new Map<string, string>(
    printers.map(p => [p.id, String(p.department ?? '').trim()]),
  );

  const monthInverted =
    periodMonth && /^\d{4}-\d{2}$/.test(periodMonth)
      ? periodMonth.split('-').reverse().join('/')
      : '';

  const deptPages = new Map<string, number>();
  let totalSpoolerPages = 0;

  for (const job of spoolerJobs) {
    if (periodMonth && /^\d{4}-\d{2}$/.test(periodMonth)) {
      const dtField = String(
        job.period_month ?? job.month_year ?? job.job_date ?? job.recorded_at ?? job.created_at ?? '',
      ).trim();
      if (dtField && !dtField.startsWith(periodMonth) && (!monthInverted || !dtField.includes(monthInverted))) {
        continue;
      }
    }

    if (printerIds.size > 0) {
      if (job.printer_id) {
        if (!printerIds.has(job.printer_id)) continue;
      } else if (job.client_id && clientIds.size > 0 && !clientIds.has(job.client_id)) {
        continue;
      }
    }

    const mono = Math.max(0, Math.round(Number(job.mono_pages ?? 0) || 0));
    const color = Math.max(0, Math.round(Number(job.color_pages ?? 0) || 0));
    const jobPages = Math.max(
      0,
      Math.round(Number(job.total_pages ?? job.pages ?? mono + color) || 0),
    );
    if (jobPages <= 0) continue;

    const printerDept = job.printer_id ? printerDeptMap.get(job.printer_id) ?? '' : '';
    const dept =
      String(job.department ?? job.sector ?? printerDept ?? '').trim() || 'Geral';

    deptPages.set(dept, (deptPages.get(dept) ?? 0) + jobPages);
    totalSpoolerPages += jobPages;
  }

  // Guard explicitly against division by zero / NaN when Spooler has 0 pages.
  if (totalSpoolerPages === 0) {
    if (odometerTotalPages > 0) {
      return {
        odometerTotalPages,
        totalSpoolerPages: 0,
        untrackedPages: odometerTotalPages,
        totalInvoiceCents,
        hasSpoolerData: false,
        allocations: [
          {
            department: UNTRACKED_SPOOLER_DEPARTMENT_LABEL,
            pages: odometerTotalPages,
            percentage: 100,
            amountCents: totalInvoiceCents,
            amountBrl: centsToBrl(totalInvoiceCents),
            isUntracked: true,
          },
        ],
      };
    }

    if (totalInvoiceCents > 0) {
      const uniqueDepts = Array.from(
        new Set(printers.map(p => String(p.department ?? '').trim() || 'Geral')),
      ).sort((a, b) => a.localeCompare(b, 'pt-BR'));

      if (uniqueDepts.length > 0) {
        const shares = distributeCents(
          totalInvoiceCents,
          uniqueDepts.map(() => 1),
        );
        return {
          odometerTotalPages: 0,
          totalSpoolerPages: 0,
          untrackedPages: 0,
          totalInvoiceCents,
          hasSpoolerData: false,
          allocations: uniqueDepts.map((dept, idx) => ({
            department: dept,
            pages: 0,
            percentage: Number(((shares[idx] / totalInvoiceCents) * 100).toFixed(2)),
            amountCents: shares[idx],
            amountBrl: centsToBrl(shares[idx]),
            isUntracked: false,
          })),
        };
      }

      return {
        odometerTotalPages: 0,
        totalSpoolerPages: 0,
        untrackedPages: 0,
        totalInvoiceCents,
        hasSpoolerData: false,
        allocations: [
          {
            department: UNTRACKED_SPOOLER_DEPARTMENT_LABEL,
            pages: 0,
            percentage: 100,
            amountCents: totalInvoiceCents,
            amountBrl: centsToBrl(totalInvoiceCents),
            isUntracked: true,
          },
        ],
      };
    }

    return {
      odometerTotalPages: 0,
      totalSpoolerPages: 0,
      untrackedPages: 0,
      totalInvoiceCents: 0,
      hasSpoolerData: false,
      allocations: [],
    };
  }

  const untrackedPages = Math.max(0, odometerTotalPages - totalSpoolerPages);
  const rawEntries: Array<{ department: string; pages: number; isUntracked: boolean }> = Array.from(
    deptPages.entries(),
  )
    .sort((a, b) => {
      if (b[1] !== a[1]) return b[1] - a[1];
      return a[0].localeCompare(b[0], 'pt-BR');
    })
    .map(([department, pages]) => ({
      department,
      pages,
      isUntracked: false,
    }));

  if (untrackedPages > 0) {
    rawEntries.push({
      department: UNTRACKED_SPOOLER_DEPARTMENT_LABEL,
      pages: untrackedPages,
      isUntracked: true,
    });
  }

  const denominator = Math.max(1, totalSpoolerPages + untrackedPages);
  const shares = distributeCents(
    totalInvoiceCents,
    rawEntries.map(entry => entry.pages),
  );

  const allocations: DepartmentAllocationItem[] = rawEntries.map((entry, idx) => ({
    department: entry.department,
    pages: entry.pages,
    percentage: Number(((entry.pages / denominator) * 100).toFixed(2)),
    amountCents: shares[idx],
    amountBrl: centsToBrl(shares[idx]),
    isUntracked: entry.isUntracked,
  }));

  return {
    odometerTotalPages,
    totalSpoolerPages,
    untrackedPages,
    totalInvoiceCents,
    hasSpoolerData: true,
    allocations,
  };
}

export async function buildFreezeSnapshotPayload(params: {
  client: Client;
  metrics: ClientComputedMetrics;
  pixPayload: string | null;
  periodMonth: string;
  dueDateIso: string;
}): Promise<{
  pricingRulesSnapshot: Record<string, unknown>;
  printerItemsSnapshot: ClosingPrinterItemSnapshot[];
  snapshotHash: string;
}> {
  const { client, metrics, pixPayload, periodMonth, dueDateIso } = params;
  const pricingRulesSnapshot: Record<string, unknown> = {
    period_month: periodMonth,
    closing_day: client.closing_day,
    base_rate: Number(client.base_rate ?? 0),
    effective_client_base_rate: Number(metrics.clientContractBase.toFixed(2)),
    franchise_mono: Number(client.franchise_mono ?? 0),
    extra_mono: Number(client.extra_mono ?? 0.05),
    franchise_color: Number(client.franchise_color ?? 0),
    extra_color: Number(client.extra_color ?? 0.1),
    replaced_delta_mono: metrics.replacedMono,
    replaced_delta_color: metrics.replacedColor,
    tech_deduction_mono: metrics.techDedMono,
    tech_deduction_color: metrics.techDedColor,
    billing_pool_group: metrics.poolGroup || null,
    adjustment_note: metrics.adjustmentNote || null,
    excedente_mono_brl: Number(metrics.exMono.toFixed(2)),
    excedente_color_brl: Number(metrics.exColor.toFixed(2)),
    custom_machines_brl: Number(metrics.customMachinesAmount.toFixed(2)),
    total_amount_brl: metrics.total,
    due_date: dueDateIso,
    pix_payload: pixPayload,
  };

  const printerItemsSnapshot: ClosingPrinterItemSnapshot[] = metrics.printerItems.map(item => {
    const subtotal = computePrinterInvoiceSubtotal({ metrics, item });

    return {
      printer_id: item.printer.id,
      model: item.printer.model,
      serial: item.printer.serial,
      unit: item.printer.unit ?? 'Matriz',
      department: item.printer.department ?? 'Geral',
      initial_mono: item.startMono,
      final_mono: item.finalMono,
      delta_mono: item.deltaMono,
      initial_color: item.startColor,
      final_color: item.finalColor,
      delta_color: item.deltaColor,
      delta_total: item.deltaTotal,
      start_source: item.startSource,
      applied_billing_mode: item.billingMode,
      subtotal_brl: subtotal,
      is_exempt: item.isExempt,
      counter_reset_detected: item.counterResetDetected,
    };
  });

  const canonical = JSON.stringify({
    client_id: client.id,
    period_month: periodMonth,
    total_pages: metrics.dTotal,
    total_amount: metrics.total,
    pricing_rules: pricingRulesSnapshot,
    printer_items: printerItemsSnapshot,
  });
  const snapshotHash = await computeSha256Hex(canonical);

  return {
    pricingRulesSnapshot,
    printerItemsSnapshot,
    snapshotHash,
  };
}

export function normalizePlanTier(planTier: string | null | undefined): Exclude<PlanTier, 'trial'> {
  const s = String(planTier ?? '').toLowerCase().trim();
  if (s === 'enterprise' || s === '6' || s.includes('completo')) return 'enterprise';
  if (s === 'pro' || s === '4' || s.includes('gest')) return 'pro';
  return 'starter';
}

export function computeCourtesyMaxPrinters(
  asaasMinChargeBrl: number,
  unitPriceBrl: number,
): number {
  const rateCents = toCents(Math.max(0, Number(unitPriceBrl || 0)));
  if (rateCents <= 0) return FREE_TRIAL_PRINTER_LIMIT;
  const minChargeCents = toCents(Math.max(0, Number(asaasMinChargeBrl || 0)));
  const maxBillableUnderMin =
    minChargeCents <= 0 ? 0 : Math.floor((minChargeCents - 1) / rateCents);
  return FREE_TRIAL_PRINTER_LIMIT + Math.max(0, maxBillableUnderMin);
}

export function resolveEffectiveCourtesyLimit(
  maxPrinters: number | null | undefined,
  asaasMinChargeBrl: number,
  planTier: string | null | undefined,
): number {
  const pt = String(planTier ?? 'trial').toLowerCase().trim();
  const rate = pt === 'trial' || pt === '' ? 0 : PLAN_CATALOG[normalizePlanTier(pt)].rate;
  const computedMax = computeCourtesyMaxPrinters(asaasMinChargeBrl, rate);
  const stored = Number(maxPrinters ?? 0);
  if (Number.isFinite(stored) && stored > 0) {
    return Math.min(Math.round(stored), computedMax);
  }
  return computedMax;
}

export function quotePlanLocally(
  planTier: string | null | undefined,
  printers: Printer[],
  isPioneerTenant: boolean,
  pioneerSlotsRemaining: number,
  asaasMinBrl = ASAAS_MIN_CHARGE_BRL,
  options?: {
    enforceMinPrepaidRecharge?: boolean;
    minPrepaidRechargeBrl?: number;
  },
): PlanQuoteSummary {
  const tier = normalizePlanTier(planTier);
  const rate = PLAN_CATALOG[tier].rate;
  const { billable, exempt } = getBillablePrintersSummary(printers);
  const rawAmountCents = computePagesAmountCents(billable.length, rate);
  const rawAmount = centsToBrl(rawAmountCents);
  const minChargeCents = toCents(asaasMinBrl);
  const belowMin = rawAmountCents < minChargeCents;
  const pioneerEligible = isPioneerTenant || pioneerSlotsRemaining > 0;
  const prepaidValCents = toCents(options?.minPrepaidRechargeBrl ?? 0);
  const prepaidVal = centsToBrl(prepaidValCents);
  const usePrepaid =
    belowMin &&
    !pioneerEligible &&
    Boolean(options?.enforceMinPrepaidRecharge) &&
    prepaidValCents >= minChargeCents;
  const chargedAmountBrl =
    belowMin && pioneerEligible ? 0 : usePrepaid ? prepaidVal : rawAmount;
  return {
    tier,
    rate,
    billablePrinters: billable.length,
    exemptPrinters: exempt.length,
    rawAmount,
    chargedAmountBrl,
    willUsePioneerCourtesy: belowMin && pioneerEligible,
    willUsePrepaidRecharge: usePrepaid,
    prepaidRechargeAmountBrl: usePrepaid ? prepaidVal : 0,
    belowMinimumBlocked: belowMin && !pioneerEligible && !usePrepaid,
  };
}
