import { describe, expect, it } from 'vitest';
import {
  buildFreezeSnapshotPayload,
  calculateDepartmentAllocation,
  computeBillingPoolsSummary,
  computeClientMetrics,
  computeCourtesyMaxPrinters,
  computePrinterInvoiceSubtotal,
  getBillablePrintersSummary,
  getPrinterPeriodMetrics,
  quotePlanLocally,
  resolveEffectiveCourtesyLimit,
  UNTRACKED_SPOOLER_DEPARTMENT_LABEL,
} from '../billingEngine';
import { buildPixPayloadStrict, calcCRC16, sanitizePixKey } from '../pixEmvco';
import { getEffectivePrinterStatus, normalizeSupplyItem, renderSupplyListHtml } from '../supplyPresenter';
import { Client, Closing, Printer, PrintJobSummary, TelemetryReading } from '../../types/models';

const baseClient: Client = {
  id: 'cli-1',
  tenant_id: 'ten-1',
  name: 'Clínica Esperança',
  cnpj: '12.345.678/0001-90',
  closing_day: 15,
  base_rate: 250,
  franchise_mono: 1000,
  extra_mono: 0.05,
  franchise_color: 200,
  extra_color: 0.25,
};

describe('pixEmvco strict validation', () => {
  it('rejeita chave PIX ausente ou cidade ausente sem inventar fallback falso', () => {
    const resMissingKey = buildPixPayloadStrict({
      pixKey: '',
      merchantName: 'PrintMind Locadora',
      merchantCity: 'Belo Horizonte',
      amount: 320.5,
    });
    expect(resMissingKey.ok).toBe(false);
    if (!resMissingKey.ok) {
      expect(resMissingKey.missingFields).toContain('pix_key');
    }

    const resMissingCity = buildPixPayloadStrict({
      pixKey: '12.345.678/0001-90',
      merchantName: 'PrintMind Locadora',
      merchantCity: '',
      amount: 320.5,
    });
    expect(resMissingCity.ok).toBe(false);
    if (!resMissingCity.ok) {
      expect(resMissingCity.missingFields).toContain('city');
    }

    const resZeros = sanitizePixKey('00000000000100');
    expect(resZeros.valid).toBe(false);
  });

  it('gera payload EMVCo valido com CRC16-CCITT usando cidade real e chave EVP/CNPJ', () => {
    const res = buildPixPayloadStrict({
      pixKey: '12.345.678/0001-90',
      merchantName: 'Locadora São José Ltda',
      merchantCity: 'São José dos Campos',
      amount: 199.9,
      txId: 'FAT202611',
    });
    expect(res.ok).toBe(true);
    if (res.ok) {
      expect(res.merchantCity).toBe('SAO JOSE DOS CA');
      expect(res.sanitizedKey).toBe('12345678000190');
      expect(res.payload).not.toContain('SAO PAULO');
      expect(res.payload).not.toContain('00000000000100');
      const bodyWithoutCrc = res.payload.slice(0, -4);
      const crc = res.payload.slice(-4);
      expect(crc).toBe(calcCRC16(bodyWithoutCrc));
    }
  });
});

describe('supplyPresenter honest presentation', () => {
  it('nunca inventa percentual para tanques state_only nem para niveis -2/-3', () => {
    const tankOk = normalizeSupplyItem({
      name: 'Tanque Ciano',
      color: 'cyan',
      supply_class: 'ink',
      measurement_mode: 'state_only',
      supply_state: 'ok',
      level: -3,
      percent: null,
    });
    expect(tankOk.isStateOnly).toBe(true);
    expect(tankOk.hasKnownPct).toBe(false);
    expect(tankOk.pct).toBeNull();
    expect(tankOk.honestLabel).toContain('sem sensor');

    const tankLow = normalizeSupplyItem({
      name: 'Tanque Preto',
      color: 'black',
      measurement_mode: 'state_only',
      supply_state: 'low',
      level: -3,
      percent: null,
    });
    expect(tankLow.isLowAlert).toBe(true);
    expect(tankLow.honestLabel).toContain('Baixo');

    const unknownItem = normalizeSupplyItem({
      name: 'Toner Amarelo',
      color: 'yellow',
      level: -2,
      level_state: 'unknown',
      percent: null,
    });
    expect(unknownItem.hasKnownPct).toBe(false);
    expect(unknownItem.pct).toBeNull();
    expect(unknownItem.honestLabel).toBe('Nível não informado pela impressora');

    const html = renderSupplyListHtml([
      {
        name: 'Cabeçote Preto/Tricolor',
        supply_class: 'printhead',
        part_number: 'M0H50A',
        serial_number: 'PH-9988',
        is_genuine: true,
      },
    ]);
    expect(html).toContain('M0H50A');
    expect(html).toContain('Genuíno');
  });

  it('mantem impressora online <24h com sinal recente como Online sem falso alarme offline', () => {
    const now = Date.now();
    const recentPrinter: Printer = {
      id: 'prt-1',
      tenant_id: 'ten-1',
      model: 'HP Smart Tank 580',
      serial: 'BR1234',
      status: 'online',
      last_seen: new Date(now - 5 * 60 * 1000).toISOString(),
    };
    const st = getEffectivePrinterStatus(recentPrinter, now);
    expect(st.isOffline).toBe(false);
    expect(st.isRecentlyActiveZeroPages).toBe(true);
    expect(st.label).toBe('Online');
  });
});

describe('billingEngine contratos, Fair Billing, pools e snapshot', () => {
  const monthPrefix = '2026-11';
  const printers: Printer[] = [
    {
      id: 'p1',
      tenant_id: 'ten-1',
      client_id: 'cli-1',
      model: 'Ricoh MP 305',
      serial: 'R001',
      initial_mono_pages: 1000,
      mono_pages: 2500,
      initial_color_pages: 0,
      color_pages: 0,
      initial_total_pages: 1000,
      total_pages: 2500,
      billing_mode: 'inherit_client',
      billing_exempt: false,
    },
    {
      id: 'p2',
      tenant_id: 'ten-1',
      client_id: 'cli-1',
      model: 'Epson WF C5790',
      serial: 'E002',
      initial_mono_pages: 100,
      mono_pages: 300,
      initial_color_pages: 200,
      color_pages: 500,
      initial_total_pages: 300,
      total_pages: 800,
      billing_mode: 'fixed_only',
      contract_base_rate: 180,
      billing_exempt: false,
    },
    {
      id: 'p3',
      tenant_id: 'ten-1',
      client_id: 'cli-1',
      model: 'HP LaserJet Bancada',
      serial: 'H003',
      initial_mono_pages: 50,
      mono_pages: 120,
      initial_color_pages: 0,
      color_pages: 0,
      initial_total_pages: 50,
      total_pages: 120,
      billing_exempt: true,
    },
  ];

  const readings: TelemetryReading[] = [];
  const closings: Closing[] = [];

  it('aplica Fair Billing isentando maquinas exempt da contagem faturavel e dos totais', () => {
    const summary = getBillablePrintersSummary(printers);
    expect(summary.billable.map(p => p.id)).toEqual(['p1', 'p2']);
    expect(summary.exempt.map(p => p.id)).toEqual(['p3']);

    const metrics = computeClientMetrics(baseClient, printers, readings, closings, monthPrefix);
    // p1: 1500 mono (500 extra @ 0.05 = R$25) + base 250 = R$275
    // p2: fixed_only R$180
    // p3: exempt R$0
    expect(metrics.exMono).toBeCloseTo(25, 2);
    expect(metrics.customMachinesAmount).toBeCloseTo(180, 2);
    expect(metrics.total).toBeCloseTo(455, 2);
  });

  it('detecta counter_reset quando contador final fica abaixo do inicial e preserva hash SHA-256 do fechamento', async () => {
    const resetPrinter: Printer = {
      id: 'p-reset',
      tenant_id: 'ten-1',
      client_id: 'cli-1',
      model: 'Brother L5652',
      serial: 'BR5652',
      initial_mono_pages: 5000,
      mono_pages: 120,
      initial_color_pages: 0,
      color_pages: 0,
      initial_total_pages: 5000,
      total_pages: 120,
      replaced_delta_mono: 420,
      counter_reset_detected: true,
    };
    const pm = getPrinterPeriodMetrics(resetPrinter, [], monthPrefix);
    expect(pm.counterResetDetected).toBe(true);
    expect(pm.deltaMono).toBe(420);

    const metrics = computeClientMetrics(baseClient, [resetPrinter], [], [], monthPrefix);
    const snap = await buildFreezeSnapshotPayload({
      client: baseClient,
      metrics,
      pixPayload: null,
      periodMonth: monthPrefix,
      dueDateIso: '2026-11-20',
    });
    expect(snap.snapshotHash).toHaveLength(64);
    expect(snap.printerItemsSnapshot).toHaveLength(1);
    expect(snap.printerItemsSnapshot[0].counter_reset_detected).toBe(true);
  });

  it('calcula economia de pool compartilhado e regras de Cortesia Pioneiros vs minimo R$5 Asaas', () => {
    const cliA: Client = { ...baseClient, id: 'a', name: 'Filial A', billing_pool_group: 'Rede Alfa', franchise_mono: 1000 };
    const cliB: Client = { ...baseClient, id: 'b', name: 'Filial B', billing_pool_group: 'Rede Alfa', franchise_mono: 1000 };
    const pA: Printer = {
      id: 'pa', tenant_id: 't', client_id: 'a', model: 'M1', serial: 'SA',
      initial_mono_pages: 0, mono_pages: 1500, initial_color_pages: 0, color_pages: 0, initial_total_pages: 0, total_pages: 1500,
    };
    const pB: Printer = {
      id: 'pb', tenant_id: 't', client_id: 'b', model: 'M2', serial: 'SB',
      initial_mono_pages: 0, mono_pages: 400, initial_color_pages: 0, color_pages: 0, initial_total_pages: 0, total_pages: 400,
    };
    const pools = computeBillingPoolsSummary([cliA, cliB], [pA, pB], [], [], monthPrefix);
    expect(pools).toHaveLength(1);
    expect(pools[0].poolSavings).toBeGreaterThan(0);

    // 3 billable machines * R$1 Starter = R$3.00 (< R$5 Asaas min)
    const threeMachines: Printer[] = [
      { id: '1', tenant_id: 't', model: 'A', serial: '1' },
      { id: '2', tenant_id: 't', model: 'B', serial: '2' },
      { id: '3', tenant_id: 't', model: 'C', serial: '3' },
    ];
    const pioneerQuote = quotePlanLocally('starter', threeMachines, false, 10);
    expect(pioneerQuote.rawAmount).toBe(3);
    expect(pioneerQuote.willUsePioneerCourtesy).toBe(true);
    expect(pioneerQuote.belowMinimumBlocked).toBe(false);

    const noSlotsQuote = quotePlanLocally('starter', threeMachines, false, 0);
    expect(noSlotsQuote.willUsePioneerCourtesy).toBe(false);
    expect(noSlotsQuote.belowMinimumBlocked).toBe(true);
    expect(noSlotsQuote.willUsePrepaidRecharge).toBe(false);

    const prepaidQuote = quotePlanLocally('starter', threeMachines, false, 0, 5.0, {
      enforceMinPrepaidRecharge: true,
      minPrepaidRechargeBrl: 30.0,
    });
    expect(prepaidQuote.belowMinimumBlocked).toBe(false);
    expect(prepaidQuote.willUsePrepaidRecharge).toBe(true);
    expect(prepaidQuote.chargedAmountBrl).toBe(30.0);
  });

  it('soma exatamente os subtotais por maquina no snapshot com contrato misto e respeita limite >2 gratis', async () => {
    const twoFree = getBillablePrintersSummary(printers); // p1 & p2 billable, p3 exempt
    expect(twoFree.billable).toHaveLength(2);
    expect(twoFree.exceedsFreeTrial).toBe(false);

    const threeBillable = getBillablePrintersSummary([
      ...printers,
      { id: 'p4', tenant_id: 'ten-1', model: 'Brother 8912', serial: 'B4', billing_exempt: false },
    ]);
    expect(threeBillable.billable).toHaveLength(3);
    expect(threeBillable.exceedsFreeTrial).toBe(true);

    const metrics = computeClientMetrics(baseClient, printers, readings, closings, monthPrefix);
    const snap = await buildFreezeSnapshotPayload({
      client: baseClient,
      metrics,
      pixPayload: null,
      periodMonth: monthPrefix,
      dueDateIso: '2026-11-20',
    });
    const subtotalSum = Number(
      snap.printerItemsSnapshot.reduce((acc, item) => acc + item.subtotal_brl, 0).toFixed(2),
    );
    expect(subtotalSum).toBeCloseTo(metrics.total, 2);
  });

  it('distribui R$ 100,00 entre 3 maquinas iguais inherit_client sem deriva de centavos (soma exata = 10000 cents)', async () => {
    const cli100: Client = {
      id: 'cli-100',
      tenant_id: 'ten-1',
      name: 'Rateio Exato Ltda',
      closing_day: 10,
      base_rate: 100,
      franchise_mono: 5000,
      extra_mono: 0.05,
      franchise_color: 0,
      extra_color: 0.1,
    };
    const eqMachines: Printer[] = [
      {
        id: 'eq-1',
        tenant_id: 'ten-1',
        client_id: 'cli-100',
        model: 'HP 408dn',
        serial: 'S1',
        billing_mode: 'inherit_client',
        initial_mono_pages: 1000,
        mono_pages: 1500,
        initial_total_pages: 1000,
        total_pages: 1500,
      },
      {
        id: 'eq-2',
        tenant_id: 'ten-1',
        client_id: 'cli-100',
        model: 'HP 408dn',
        serial: 'S2',
        billing_mode: 'inherit_client',
        initial_mono_pages: 2000,
        mono_pages: 2500,
        initial_total_pages: 2000,
        total_pages: 2500,
      },
      {
        id: 'eq-3',
        tenant_id: 'ten-1',
        client_id: 'cli-100',
        model: 'HP 408dn',
        serial: 'S3',
        billing_mode: 'inherit_client',
        initial_mono_pages: 3000,
        mono_pages: 3500,
        initial_total_pages: 3000,
        total_pages: 3500,
      },
    ];

    const metrics = computeClientMetrics(cli100, eqMachines, [], [], monthPrefix);
    expect(metrics.total).toBe(100);
    expect(metrics.totalCents).toBe(10000);

    const subtotalsCents = metrics.printerItems.map(item =>
      Math.round(computePrinterInvoiceSubtotal({ metrics, item }) * 100),
    );
    expect(subtotalsCents).toEqual([3334, 3333, 3333]);
    expect(subtotalsCents.reduce((s, v) => s + v, 0)).toBe(10000);

    const snap = await buildFreezeSnapshotPayload({
      client: cli100,
      metrics,
      pixPayload: null,
      periodMonth: monthPrefix,
      dueDateIso: '2026-11-20',
    });
    const snapCentsSum = snap.printerItemsSnapshot.reduce(
      (s, it) => s + Math.round(it.subtotal_brl * 100),
      0,
    );
    expect(snapCentsSum).toBe(Math.round(metrics.total * 100));
  });

  it('valida contrato misto completo (exempt + fixed_monthly + per_page_only + minimum_plus_excess + inherit_client + pool) sem deriva de centavos', async () => {
    const cliMixed: Client = {
      id: 'cli-mix',
      tenant_id: 'ten-1',
      name: 'Matriz Mix',
      closing_day: 15,
      base_rate: 100,
      franchise_mono: 300,
      extra_mono: 0.03,
      franchise_color: 50,
      extra_color: 0.17,
      billing_pool_group: 'Grupo Mix',
    };
    const cliBranch: Client = {
      id: 'cli-branch',
      tenant_id: 'ten-1',
      name: 'Filial Mix',
      closing_day: 15,
      base_rate: 100,
      franchise_mono: 1000,
      extra_mono: 0.03,
      franchise_color: 200,
      extra_color: 0.17,
      billing_pool_group: 'Grupo Mix',
    };

    const mixPrinters: Printer[] = [
      {
        id: 'm-exempt',
        tenant_id: 'ten-1',
        client_id: 'cli-mix',
        model: 'Epson L3250',
        serial: 'MX0',
        billing_exempt: true,
        billing_mode: 'exempt',
        initial_mono_pages: 100,
        mono_pages: 900,
      },
      {
        id: 'm-fixed',
        tenant_id: 'ten-1',
        client_id: 'cli-mix',
        model: 'Kyocera M2040',
        serial: 'MX1',
        billing_mode: 'fixed_monthly',
        contract_base_rate: 149.99,
        initial_mono_pages: 0,
        mono_pages: 300,
      },
      {
        id: 'm-page',
        tenant_id: 'ten-1',
        client_id: 'cli-mix',
        model: 'Ricoh C300',
        serial: 'MX2',
        billing_mode: 'per_page_only',
        contract_extra_mono: 0.033,
        contract_extra_color: 0.197,
        initial_mono_pages: 0,
        mono_pages: 101,
        initial_color_pages: 0,
        color_pages: 37,
      },
      {
        id: 'm-min',
        tenant_id: 'ten-1',
        client_id: 'cli-mix',
        model: 'Brother 8912',
        serial: 'MX3',
        billing_mode: 'minimum_plus_excess',
        contract_base_rate: 80.55,
        contract_franchise_mono: 200,
        contract_extra_mono: 0.045,
        initial_mono_pages: 0,
        mono_pages: 333,
      },
      {
        id: 'm-inh-1',
        tenant_id: 'ten-1',
        client_id: 'cli-mix',
        model: 'HP M404',
        serial: 'MX4',
        billing_mode: 'inherit_client',
        initial_mono_pages: 0,
        mono_pages: 211,
        initial_color_pages: 0,
        color_pages: 41,
      },
      {
        id: 'm-inh-2',
        tenant_id: 'ten-1',
        client_id: 'cli-mix',
        model: 'HP M404',
        serial: 'MX5',
        billing_mode: 'inherit_client',
        initial_mono_pages: 0,
        mono_pages: 211,
        initial_color_pages: 0,
        color_pages: 41,
      },
      {
        id: 'm-inh-3',
        tenant_id: 'ten-1',
        client_id: 'cli-mix',
        model: 'HP M404',
        serial: 'MX6',
        billing_mode: 'inherit_client',
        initial_mono_pages: 0,
        mono_pages: 211,
        initial_color_pages: 0,
        color_pages: 41,
      },
      {
        id: 'b-inh-1',
        tenant_id: 'ten-1',
        client_id: 'cli-branch',
        model: 'HP Filial',
        serial: 'BR1',
        billing_mode: 'inherit_client',
        initial_mono_pages: 0,
        mono_pages: 100,
      },
    ];

    const metrics = computeClientMetrics(cliMixed, mixPrinters, [], [], monthPrefix);
    const snap = await buildFreezeSnapshotPayload({
      client: cliMixed,
      metrics,
      pixPayload: null,
      periodMonth: monthPrefix,
      dueDateIso: '2026-11-25',
    });

    const exemptItem = snap.printerItemsSnapshot.find(i => i.printer_id === 'm-exempt');
    expect(exemptItem?.is_exempt).toBe(true);
    expect(exemptItem?.subtotal_brl).toBe(0);

    const sumCents = snap.printerItemsSnapshot.reduce(
      (acc, item) => acc + Math.round(item.subtotal_brl * 100),
      0,
    );
    expect(sumCents).toBe(Math.round(metrics.total * 100));

    const pools = computeBillingPoolsSummary([cliMixed, cliBranch], mixPrinters, [], [], monthPrefix);
    expect(pools).toHaveLength(1);
    expect(pools[0].poolSavings).toBeGreaterThanOrEqual(0);
    expect(Number.isInteger(Math.round(pools[0].pooledTotal * 100))).toBe(true);
  });

  it('calculateDepartmentAllocation trata volume parcial de Spooler com linha de nao rastreados e soma exata em centavos', () => {
    const printers: Printer[] = [
      { id: 'p1', tenant_id: 't1', client_id: 'c1', model: 'M1', serial: 'S1', department: 'Financeiro' },
      { id: 'p2', tenant_id: 't1', client_id: 'c1', model: 'M2', serial: 'S2', department: 'Comercial' },
    ];
    const jobs: PrintJobSummary[] = [
      { printer_id: 'p1', period_month: monthPrefix, department: 'Financeiro', total_pages: 500 },
      { printer_id: 'p2', period_month: monthPrefix, department: 'Comercial', total_pages: 250 },
    ];

    // Total odômetro = 1200, Spooler = 750 -> Gap não rastreado = 450, Fatura = R$ 100,00 (10000 cents)
    const result = calculateDepartmentAllocation({
      odometerTotalPages: 1200,
      totalInvoiceCents: 10000,
      printers,
      spoolerJobs: jobs,
      periodMonth: monthPrefix,
    });

    expect(result.hasSpoolerData).toBe(true);
    expect(result.totalSpoolerPages).toBe(750);
    expect(result.untrackedPages).toBe(450);

    const untrackedRow = result.allocations.find(
      a => a.department === UNTRACKED_SPOOLER_DEPARTMENT_LABEL,
    );
    expect(untrackedRow).toBeDefined();
    expect(untrackedRow?.isUntracked).toBe(true);
    expect(untrackedRow?.pages).toBe(450);

    const centsSum = result.allocations.reduce((s, a) => s + a.amountCents, 0);
    expect(centsSum).toBe(10000);
    expect(result.allocations.every(a => Number.isFinite(a.percentage))).toBe(true);
  });

  it('calculateDepartmentAllocation protege spooler === 0 contra NaN/divisao por zero mantendo soma exata', () => {
    const printers: Printer[] = [
      { id: 'p1', tenant_id: 't1', client_id: 'c1', model: 'M1', serial: 'S1', department: 'Geral' },
    ];

    const withOdometerOnly = calculateDepartmentAllocation({
      odometerTotalPages: 900,
      totalInvoiceCents: 10000,
      printers,
      spoolerJobs: [],
      periodMonth: monthPrefix,
    });
    expect(withOdometerOnly.hasSpoolerData).toBe(false);
    expect(withOdometerOnly.totalSpoolerPages).toBe(0);
    expect(withOdometerOnly.untrackedPages).toBe(900);
    expect(withOdometerOnly.allocations).toHaveLength(1);
    expect(withOdometerOnly.allocations[0].department).toBe(UNTRACKED_SPOOLER_DEPARTMENT_LABEL);
    expect(withOdometerOnly.allocations[0].amountCents).toBe(10000);
    expect(Number.isNaN(withOdometerOnly.allocations[0].percentage)).toBe(false);

    const emptyAll = calculateDepartmentAllocation({
      odometerTotalPages: 0,
      totalInvoiceCents: 0,
      printers: [],
      spoolerJobs: [],
      periodMonth: monthPrefix,
    });
    expect(emptyAll.hasSpoolerData).toBe(false);
    expect(emptyAll.allocations).toEqual([]);
  });

  it('cobre rateio com replaced_delta_mono (pesos zero) e casos extremos de calculateDepartmentAllocation sem deriva', () => {
    const cliReplaced: Client = {
      id: 'cli-rep',
      tenant_id: 'ten-1',
      name: 'Troca Zero Peso Ltda',
      closing_day: 20,
      base_rate: 50,
      franchise_mono: 100,
      extra_mono: 0.037,
      franchise_color: 0,
      extra_color: 0.1,
      replaced_delta_mono: 333,
    };
    const zeroDeltaMachines: Printer[] = [
      { id: 'z1', tenant_id: 'ten-1', client_id: 'cli-rep', model: 'M1', serial: 'Z1', department: 'RH', mono_pages: 0, initial_mono_pages: 0 },
      { id: 'z2', tenant_id: 'ten-1', client_id: 'cli-rep', model: 'M2', serial: 'Z2', department: 'TI', mono_pages: 0, initial_mono_pages: 0 },
      { id: 'z3', tenant_id: 'ten-1', client_id: 'cli-rep', model: 'M3', serial: 'Z3', department: 'Ops', mono_pages: 0, initial_mono_pages: 0 },
    ];

    const m = computeClientMetrics(cliReplaced, zeroDeltaMachines, [], [], monthPrefix);
    const itemSumCents = m.printerItems.reduce(
      (s, it) => s + Math.round(computePrinterInvoiceSubtotal({ metrics: m, item: it }) * 100),
      0,
    );
    expect(m.totalCents).toBeGreaterThan(5000);
    expect(itemSumCents).toBe(Math.round(m.total * 100));

    // Odometer < Spooler (ex.: ajuste técnico) não cria linha untracked nem valor negativo
    const overSpooler = calculateDepartmentAllocation({
      odometerTotalPages: 300,
      totalInvoiceCents: 10000,
      printers: zeroDeltaMachines,
      spoolerJobs: [
        { printer_id: 'z1', period_month: monthPrefix, total_pages: 300 },
        { printer_id: 'z2', period_month: monthPrefix, total_pages: 200 },
      ],
      periodMonth: monthPrefix,
    });
    expect(overSpooler.untrackedPages).toBe(0);
    expect(overSpooler.allocations.some(a => a.isUntracked)).toBe(false);
    expect(overSpooler.allocations.reduce((s, a) => s + a.amountCents, 0)).toBe(10000);

    // Odometer = 0 e Spooler = 0 com contrato fixo R$ 100,00 entre 3 departamentos
    const fixedOnlyNoVolume = calculateDepartmentAllocation({
      odometerTotalPages: 0,
      totalInvoiceCents: 10000,
      printers: zeroDeltaMachines,
      spoolerJobs: [],
      periodMonth: monthPrefix,
    });
    expect(fixedOnlyNoVolume.hasSpoolerData).toBe(false);
    expect(fixedOnlyNoVolume.allocations.map(a => a.amountCents)).toEqual([3334, 3333, 3333]);
    expect(fixedOnlyNoVolume.allocations.reduce((s, a) => s + a.amountCents, 0)).toBe(10000);
  });

  it('limita rigorosamente o teto da Cortesia Pioneiros (< R$ 5,00) e clampa legado 500', () => {
    expect(computeCourtesyMaxPrinters(5.0, 1.0)).toBe(6); // 2 grátis + 4 cobráveis (< R$ 5,00)
    expect(computeCourtesyMaxPrinters(5.0, 4.0)).toBe(3); // 2 grátis + 1 cobrável (< R$ 5,00)
    expect(computeCourtesyMaxPrinters(5.0, 6.0)).toBe(2); // apenas 2 grátis
    expect(computeCourtesyMaxPrinters(6.01, 3.0)).toBe(4); // (601-1)/300 = 2 cobráveis + 2 grátis

    // Se banco tiver legado max_printers = 500 de versão anterior, clamp garante teto real
    expect(resolveEffectiveCourtesyLimit(500, 5.0, 'starter')).toBe(6);
    expect(resolveEffectiveCourtesyLimit(500, 5.0, 'pro')).toBe(3);
    expect(resolveEffectiveCourtesyLimit(500, 5.0, 'enterprise')).toBe(2);
    expect(resolveEffectiveCourtesyLimit(500, 5.0, 'trial')).toBe(2);
    expect(resolveEffectiveCourtesyLimit(null, 5.0, 'starter')).toBe(6);
  });

  it('nao cobra cor retroativa quando telemetry_readings anterior tem color=0 e baseline foi ajustado', () => {
    const cliNoFranq: Client = {
      id: 'cli-retro',
      tenant_id: 'ten-1',
      name: 'Clínica Retro Cor',
      closing_day: 15,
      base_rate: 100,
      franchise_mono: 0,
      extra_mono: 0.05,
      franchise_color: 0,
      extra_color: 0.1,
    };
    const prtAdjusted: Printer = {
      id: 'prt-retro',
      tenant_id: 'ten-1',
      client_id: 'cli-retro',
      model: 'HP Smart Tank 580',
      serial: 'E2E-SN-001',
      initial_total_pages: 1000,
      initial_mono_pages: 800,
      initial_color_pages: 200,
      total_pages: 1250,
      mono_pages: 1050,
      color_pages: 200,
      is_color_capable: true,
    };
    const legacyZeroColorReading: TelemetryReading[] = [
      {
        printer_id: 'prt-retro',
        total_pages: 1000,
        mono_pages: 1000,
        color_pages: 0,
        recorded_at: `${monthPrefix}-05T10:00:00Z`,
      },
    ];

    const pm = getPrinterPeriodMetrics(prtAdjusted, legacyZeroColorReading, monthPrefix);
    expect(pm.deltaMono).toBe(250);
    expect(pm.deltaColor).toBe(0);
    expect(pm.deltaTotal).toBe(250);

    const metrics = computeClientMetrics(cliNoFranq, [prtAdjusted], legacyZeroColorReading, [], monthPrefix);
    expect(metrics.total).toBeCloseTo(112.5, 2);
  });
});
