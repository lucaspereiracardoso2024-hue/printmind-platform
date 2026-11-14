import {
  centsToBrl,
  computeBillingPoolsSummary,
  computeClientMetrics,
  computePrinterInvoiceSubtotal,
  getPrinterPeriodMetrics,
  normalizeBillingMode,
} from '../domain/billingEngine';
import { getEffectivePrinterStatus, normalizeSupplyItem } from '../domain/supplyPresenter';
import { invokeCloudAiInsights } from '../services/aiService';
import { toggleClientClosingFreeze } from '../services/billingService';
import { sb } from '../services/supabaseClient';
import { appState, byId, loadTenantData } from '../state/store';
import { AiInsightPayload, SpareTonersStock } from '../types/models';
import { escapeHtml, formatCurrencyBrl, formatIntegerBr, getCurrentMonthPrefix } from '../utils/format';
import { refreshIcons } from '../utils/icons';
import { adjustClientSpareToner, isClientWppReady, openClientAdjustmentModal, openSpareTonersModal, renderSpareTonersInlineWidget, renderSpareTonersSummaryPills } from './clientsView';
import { switchReportsSubTab } from './navigation';
import { buildPixPayloadStrict } from '../domain/pixEmvco';

type ReportsCallbacks = {
  onOpenInvoice: (clientId: string) => void;
  onDataChanged: () => void;
};

let reportsCallbacks: ReportsCallbacks = {
  onOpenInvoice: () => {},
  onDataChanged: () => {},
};

function isUnitWppReady(unitName?: string | null, unitId?: string | null, clientId?: string | null): boolean {
  const u = appState.units.find(
    x => (unitId && x.id === unitId) || (x.client_id === clientId && x.unit_name === unitName),
  );
  return Boolean(u && u.wpp_enabled === true);
}

function getAlertTypeMeta(alertType?: string | null): { label: string; cls: string } {
  const t = String(alertType ?? '').toLowerCase();
  if (t.includes('waste') || t.includes('residuo')) {
    return { label: 'Caixa de Resíduo', cls: 'bg-rose-50 text-rose-800 border-rose-200' };
  }
  if (t.includes('premature') || t.includes('prematur')) {
    return { label: 'Troca Prematura', cls: 'bg-amber-50 text-amber-900 border-amber-300' };
  }
  if (t.includes('low') || t.includes('empty')) {
    return { label: 'Suprimento Baixo', cls: 'bg-orange-50 text-orange-800 border-orange-200' };
  }
  return { label: 'Auditoria Hardware', cls: 'bg-slate-100 text-slate-700 border-slate-200' };
}

function detectSupplyColor(nameOrMsg: string): keyof SpareTonersStock {
  const txt = String(nameOrMsg).toLowerCase();
  if (/ciano|cyan/.test(txt)) return 'C';
  if (/magenta/.test(txt)) return 'M';
  if (/amarelo|yellow/.test(txt)) return 'Y';
  return 'K';
}

export function renderGeminiInsights(data: AiInsightPayload): void {
  const container = byId('geminiInsightsBody');
  if (!container) return;
  appState.latestAiInsight = data;

  const formatItem = (item: unknown): string => {
    if (typeof item === 'string') return item;
    if (item && typeof item === 'object') {
      const obj = item as Record<string, unknown>;
      const head = String(obj.target ?? obj.title ?? obj.action ?? '');
      const tail = String(obj.reason ?? obj.detail ?? obj.recommendation ?? obj.estimated_impact ?? '');
      return head && tail ? `${head}: ${tail}` : head || tail || JSON.stringify(obj);
    }
    return String(item ?? '');
  };

  const renderList = (arr: unknown[], emptyMsg: string): string =>
    arr.length === 0
      ? `<li class="text-blue-200/70 italic">${emptyMsg}</li>`
      : arr.map(it => `<li>• ${escapeHtml(formatItem(it))}</li>`).join('');

  container.innerHTML = `
    <div class="p-4 rounded-2xl bg-white/10 border border-white/15 space-y-1.5">
      <span class="text-[10px] font-black uppercase tracking-wider text-amber-300 block">Resumo Executivo da Frota (LGPD Anonimizado)</span>
      <p class="text-xs text-white leading-relaxed">${escapeHtml(data.executive_summary || 'Análise concluída.')}</p>
    </div>
    <div class="grid grid-cols-1 md:grid-cols-3 gap-3 text-xs">
      <div class="p-4 rounded-2xl bg-rose-500/15 border border-rose-300/25 space-y-2">
        <strong class="text-[11px] font-black uppercase text-rose-200 block">Ações Urgentes</strong>
        <ul class="space-y-1.5 text-rose-50/90 leading-relaxed">${renderList(data.urgent_actions, 'Nenhuma ação crítica imediata.')}</ul>
      </div>
      <div class="p-4 rounded-2xl bg-emerald-500/15 border border-emerald-300/25 space-y-2">
        <strong class="text-[11px] font-black uppercase text-emerald-200 block">Dicas de Economia</strong>
        <ul class="space-y-1.5 text-emerald-50/90 leading-relaxed">${renderList(data.cost_saving_tips, 'Sem recomendações adicionais no momento.')}</ul>
      </div>
      <div class="p-4 rounded-2xl bg-amber-500/15 border border-amber-300/25 space-y-2">
        <strong class="text-[11px] font-black uppercase text-amber-200 block">Anomalias Detectadas</strong>
        <ul class="space-y-1.5 text-amber-50/90 leading-relaxed">${renderList(data.anomalies, 'Nenhuma anomalia detectada.')}</ul>
      </div>
    </div>
    <div class="p-4 rounded-2xl bg-slate-950/50 border border-white/15 space-y-2.5">
      <div class="flex flex-wrap items-center justify-between gap-2">
        <strong class="text-xs font-black text-emerald-300">Briefing Pronto para WhatsApp (PT-BR)</strong>
        <div class="flex items-center gap-2">
          <button type="button" id="btnCopyWppBriefing" class="px-3 py-1.5 rounded-xl bg-emerald-500 hover:bg-emerald-400 text-slate-950 font-black text-[11px] transition flex items-center space-x-1">
            <i data-lucide="copy" class="w-3.5 h-3.5"></i><span>Copiar Briefing p/ WhatsApp</span>
          </button>
          <button type="button" id="btnOpenWppBriefing" class="px-3 py-1.5 rounded-xl bg-white/15 hover:bg-white/25 text-white font-bold text-[11px] transition flex items-center space-x-1">
            <i data-lucide="message-circle" class="w-3.5 h-3.5"></i><span>Abrir WhatsApp</span>
          </button>
        </div>
      </div>
      <pre class="whitespace-pre-wrap text-[11px] text-blue-50 font-sans leading-relaxed bg-black/25 p-3 rounded-xl border border-white/10">${escapeHtml(data.whatsapp_briefing_ptbr || data.executive_summary)}</pre>
    </div>`;

  byId('btnCopyWppBriefing')?.addEventListener('click', () => {
    const txt = data.whatsapp_briefing_ptbr || data.executive_summary;
    void navigator.clipboard?.writeText(txt);
    const btn = byId('btnCopyWppBriefing');
    if (btn) btn.innerText = '✓ Copiado!';
  });
  byId('btnOpenWppBriefing')?.addEventListener('click', () => {
    const txt = data.whatsapp_briefing_ptbr || data.executive_summary;
    window.open(`https://api.whatsapp.com/send?text=${encodeURIComponent(txt)}`, '_blank');
  });

  refreshIcons();
}

function resolveReportsPeriodMonth(): string {
  const inputVal = byId<HTMLInputElement>('reportsPeriodMonth')?.value.trim() ?? '';
  if (/^\d{4}-\d{2}$/.test(inputVal)) {
    appState.selectedPeriodMonth = inputVal;
    return inputVal;
  }
  const fallback = /^\d{4}-\d{2}$/.test(appState.selectedPeriodMonth)
    ? appState.selectedPeriodMonth
    : getCurrentMonthPrefix();
  const el = byId<HTMLInputElement>('reportsPeriodMonth');
  if (el) el.value = fallback;
  return fallback;
}

export function renderReportsView(): void {
  const monthPrefix = resolveReportsPeriodMonth();
  let sumBillCents = 0;
  let sumPages = 0;
  let sumColor = 0;
  let frozenCount = 0;
  let openCount = 0;

  appState.clients.forEach(c => {
    const m = computeClientMetrics(c, appState.printers, appState.telemetryReadings, appState.closings, monthPrefix);
    sumBillCents += m.totalCents;
    sumPages += m.dTotal;
    sumColor += m.dColor;
    if (m.isFrozen) frozenCount++;
    else openCount++;
  });

  const repTotalBill = byId('repTotalBilling');
  const repStatus = byId('repStatusCount');
  const repPages = byId('repTotalPages');
  const repRatio = byId('repColorRatio');
  if (repTotalBill) repTotalBill.innerText = formatCurrencyBrl(centsToBrl(sumBillCents));
  if (repStatus) repStatus.innerText = `${frozenCount} Congelada${frozenCount === 1 ? '' : 's'} • ${openCount} Em Aberto`;
  if (repPages) repPages.innerText = `${formatIntegerBr(sumPages)} págs`;
  if (repRatio) {
    const colorPct = sumPages > 0 ? Math.round((sumColor / sumPages) * 100) : 0;
    repRatio.innerText = `${colorPct}% Coloridas • ${100 - colorPct}% Monocromáticas`;
  }

  const wppCount = appState.units.filter(u => u.wpp_enabled === true).length;
  const wppEl = byId('wppReadinessSummary');
  if (wppEl) {
    wppEl.classList.toggle('hidden', wppCount === 0);
    if (wppCount > 0) wppEl.innerText = `✓ Prontidão WPP Windows 11: ${wppCount} unidade(s) ativa(s)`;
  }

  const tbody = byId('reportsTableBody');
  if (tbody) {
    tbody.innerHTML =
      appState.clients.length === 0
        ? `<tr><td colspan="7" class="py-8 text-center text-slate-400">Nenhum cliente cadastrado ainda.</td></tr>`
        : appState.clients
            .map(c => {
              const m = computeClientMetrics(c, appState.printers, appState.telemetryReadings, appState.closings, monthPrefix);
              const badge = m.isFrozen
                ? '<span class="px-2.5 py-0.5 rounded-full text-[10px] font-black bg-blue-100 text-[#0038A8]">● TRAVADO</span>'
                : '<span class="px-2.5 py-0.5 rounded-full text-[10px] font-bold bg-amber-50 text-amber-700">● ABERTO</span>';
              const wppPill = isClientWppReady(c.id)
                ? '<span class="ml-1.5 px-2 py-0.5 rounded-full text-[9px] font-bold bg-emerald-100 text-emerald-800">✓ WPP Win11</span>'
                : '';
              const poolPill = m.poolGroup
                ? `<span class="ml-1.5 px-2 py-0.5 rounded-full text-[9px] font-bold bg-indigo-100 text-indigo-800">Pool: ${escapeHtml(m.poolGroup)}</span>`
                : '';
              return `
                <tr class="hover:bg-slate-50 transition">
                  <td class="py-3 px-4 font-bold text-slate-900">
                    <div>${escapeHtml(c.name)}${wppPill}${poolPill}</div>
                    <div class="mt-1 flex flex-wrap gap-1">${renderSpareTonersSummaryPills(c)}</div>
                  </td>
                  <td class="py-3 px-4 mono text-slate-600">Corte Dia ${c.closing_day}</td>
                  <td class="py-3 px-4 text-center mono font-bold">${m.linked.length}</td>
                  <td class="py-3 px-4 text-right mono font-bold">${formatIntegerBr(m.dTotal)} págs</td>
                  <td class="py-3 px-4 text-right mono font-black text-slate-900">${formatCurrencyBrl(m.total)}</td>
                  <td class="py-3 px-4 text-center">${badge}</td>
                  <td class="py-3 px-4 text-right">
                    <div class="inline-flex flex-wrap justify-end items-center gap-1">
                      <button type="button" data-action="rep-adj" data-client-id="${c.id}" class="px-2.5 py-1 rounded-xl bg-amber-50 hover:bg-amber-100 text-amber-900 border border-amber-200 font-bold text-[11px]">Ajuste</button>
                      <button type="button" data-action="rep-invoice" data-client-id="${c.id}" class="px-3 py-1 rounded-xl bg-[#0038A8] text-white font-bold hover:bg-blue-800">Espelho A4</button>
                    </div>
                  </td>
                </tr>`;
            })
            .join('');
  }

  const repPoolCard = byId('reportsPoolSummaryCard');
  if (repPoolCard) {
    const pools = computeBillingPoolsSummary(
      appState.clients,
      appState.printers,
      appState.telemetryReadings,
      appState.closings,
      monthPrefix,
    );
    repPoolCard.classList.toggle('hidden', pools.length === 0);
    if (pools.length > 0) {
      repPoolCard.innerHTML = pools
        .map(
          pl => `
          <div class="p-4 rounded-2xl bg-indigo-50 border border-indigo-200 flex flex-wrap items-center justify-between gap-2 text-xs">
            <div>
              <strong class="text-indigo-950 font-black block">Pool: ${escapeHtml(pl.poolName)} (${pl.clientsCount} unidades)</strong>
              <span class="text-indigo-700">${escapeHtml(pl.clientNames.join(' • '))}</span>
            </div>
            <div class="text-right mono">
              <div class="font-black text-indigo-950">${formatCurrencyBrl(pl.pooledTotal)}</div>
              <span class="text-[11px] font-bold text-emerald-700">Economia vs individual: ${formatCurrencyBrl(pl.poolSavings)}</span>
            </div>
          </div>`,
        )
        .join('');
    } else {
      repPoolCard.innerHTML = '';
    }
  }

  const machineBody = byId('machineClosingTableBody');
  if (machineBody) {
    machineBody.innerHTML =
      appState.printers.length === 0
        ? `<tr><td colspan="7" class="py-8 text-center text-slate-400">Nenhuma impressora conectada para fechamento.</td></tr>`
        : appState.printers
            .map(p => {
              const c = appState.clients.find(x => x.id === p.client_id);
              const st = getEffectivePrinterStatus(p);
              const pm = getPrinterPeriodMetrics(p, appState.telemetryReadings, monthPrefix);
              const mode = normalizeBillingMode(p);
              const inheritedCount = c
                ? Math.max(
                    1,
                    appState.printers.filter(x => x.client_id === c.id && normalizeBillingMode(x) === 'inherit_client')
                      .length,
                  )
                : 1;
              let franqCellHtml = '<span class="text-slate-400">Sem franquia</span>';
              if (mode === 'exempt') {
                franqCellHtml = '<span class="text-amber-700 font-bold">Isenta</span>';
              } else if (mode === 'fixed_only') {
                franqCellHtml = '<span class="text-indigo-700 font-bold">Mensal Fixa</span>';
              } else if (mode === 'per_page_only') {
                franqCellHtml = '<span class="text-indigo-700 font-bold">Por Página</span>';
              } else if (mode === 'fixed_plus_franchise') {
                const machFranq = Number(p.contract_franchise_mono ?? 0) + Number(p.contract_franchise_color ?? 0);
                const machEx = Math.max(0, pm.deltaTotal - machFranq);
                franqCellHtml = `<span class="text-slate-700">${formatIntegerBr(machFranq)} franq.</span><span class="block text-[10px] ${machEx > 0 ? 'text-amber-700 font-bold' : 'text-emerald-700'}">${machEx > 0 ? '+' + formatIntegerBr(machEx) + ' exced.' : 'Dentro da franquia'}</span>`;
              } else if (c) {
                const franqPerMach = Math.round(
                  (Number(c.franchise_mono || 0) + Number(c.franchise_color || 0)) / inheritedCount,
                );
                if (franqPerMach > 0) {
                  const excessPages = Math.max(0, pm.deltaTotal - franqPerMach);
                  franqCellHtml = `<span class="text-slate-700">${formatIntegerBr(franqPerMach)} franq.</span><span class="block text-[10px] ${excessPages > 0 ? 'text-amber-700 font-bold' : 'text-emerald-700'}">${excessPages > 0 ? '+' + formatIntegerBr(excessPages) + ' exced.' : 'Dentro da franquia'}</span>`;
                }
              }
              const wppPill = isUnitWppReady(p.unit, p.unit_id, p.client_id)
                ? '<span class="inline-block mt-0.5 px-1.5 py-0.5 rounded-full text-[9px] font-bold bg-emerald-100 text-emerald-800">✓ WPP Win11</span>'
                : '';

              return `
                <tr class="hover:bg-slate-50 transition">
                  <td class="py-3 px-4">
                    <strong class="text-slate-900 block">${escapeHtml(c ? c.name : 'Avulsa')}</strong>
                    <span class="text-[11px] text-slate-500">${escapeHtml(p.unit || 'Matriz')} • ${escapeHtml(p.department || 'Geral')}</span>
                    <div>${wppPill}</div>
                  </td>
                  <td class="py-3 px-4">
                    <strong class="text-slate-900 block">${escapeHtml(p.model)}</strong>
                    <span class="text-[11px] text-slate-400 mono">${escapeHtml(p.serial)}</span>
                  </td>
                  <td class="py-3 px-4 text-center"><span class="px-2 py-0.5 rounded-full text-[10px] font-bold ${st.badgeClass}">${escapeHtml(st.label)}</span></td>
                  <td class="py-3 px-4 text-right mono"><strong>${formatIntegerBr(pm.startTotal)}</strong><span class="block text-[10px] text-slate-400">${escapeHtml(pm.startSource)}</span></td>
                  <td class="py-3 px-4 text-right mono font-bold text-slate-800">${formatIntegerBr(pm.finalTotal)}</td>
                  <td class="py-3 px-4 text-right mono"><strong class="text-[#0038A8]">${formatIntegerBr(pm.deltaTotal)} págs</strong><span class="block text-[10px] text-slate-400">${formatIntegerBr(pm.deltaMono)} P&amp;B • ${formatIntegerBr(pm.deltaColor)} Cor</span></td>
                  <td class="py-3 px-4 text-right mono">${franqCellHtml}</td>
                </tr>`;
            })
            .join('');
  }

  // Spooler breakdown
  const jobsContainer = byId('jobsBreakdownContainer');
  const jobsBadge = byId('jobsBreakdownCountBadge');
  const monthJobs = appState.printJobsSummary.filter(j => {
    const dtField = String(j.period_month ?? j.month_year ?? j.job_date ?? j.recorded_at ?? j.created_at ?? '');
    return !dtField || dtField.startsWith(monthPrefix) || dtField.includes(monthPrefix.split('-').reverse().join('/'));
  });
  const hardwareMonthTotal = appState.printers.reduce(
    (sum, p) => sum + getPrinterPeriodMetrics(p, appState.telemetryReadings, monthPrefix).deltaTotal,
    0,
  );
  if (jobsBadge) jobsBadge.innerText = `${monthJobs.length} registro${monthJobs.length === 1 ? '' : 's'}`;
  if (jobsContainer) {
    if (monthJobs.length === 0) {
      jobsContainer.innerHTML = `<div class="p-5 rounded-2xl bg-slate-50 border border-slate-200 text-center text-xs text-slate-500">Nenhuma impressão detalhada registrada neste mês via Spooler.${hardwareMonthTotal > 0 ? ` Volume hardware do mês (${formatIntegerBr(hardwareMonthTotal)} págs) referente a cópias físicas, mobile ou impressão direta IP.` : ''}</div>`;
    } else {
      const grouped: Record<string, { dept: string; pc: string; usr: string; jobs: number; mono: number; color: number; duplex: number; total: number; estCost: number }> = {};
      monthJobs.forEach(j => {
        const dept = j.department ?? j.sector ?? 'Geral';
        const pc = j.machine_name ?? j.pc_name ?? j.hostname ?? j.workstation ?? j.computer_name ?? 'Estação';
        const usr = j.user_name ?? j.username ?? j.os_user ?? 'Usuário';
        const prt = appState.printers.find(x => x.id === j.printer_id);
        const cli = prt ? appState.clients.find(c => c.id === prt.client_id) : null;
        const rateMono = cli ? Number(cli.extra_mono ?? 0.05) : 0.05;
        const rateColor = cli ? Number(cli.extra_color ?? 0.35) : 0.35;
        const key = `${dept}||${pc}||${usr}`;
        if (!grouped[key]) grouped[key] = { dept, pc, usr, jobs: 0, mono: 0, color: 0, duplex: 0, total: 0, estCost: 0 };
        const g = grouped[key];
        const mono = Number(j.mono_pages ?? 0);
        const color = Number(j.color_pages ?? 0);
        const duplex = Number(j.duplex_pages ?? 0);
        const tot = Number(j.total_pages ?? j.pages ?? mono + color);
        g.jobs += Number(j.jobs_count ?? j.job_count ?? 1);
        g.mono += mono;
        g.color += color;
        g.duplex += duplex;
        g.total += tot;
        g.estCost += mono * rateMono + color * rateColor;
      });
      const rows = Object.values(grouped).sort((a, b) => b.total - a.total);
      const spoolerMonthTotal = rows.reduce((acc, r) => acc + r.total, 0);
      const untrackedGap = Math.max(0, hardwareMonthTotal - spoolerMonthTotal);
      const reconciliationHtml =
        hardwareMonthTotal > 0
          ? `<div class="mb-3 p-3 rounded-2xl bg-slate-50 border border-slate-200 flex flex-wrap items-center justify-between gap-2 text-[11px]">
              <span class="mono">Hardware auditado: <strong>${formatIntegerBr(hardwareMonthTotal)} págs</strong> • Spooler PC: <strong>${formatIntegerBr(spoolerMonthTotal)} págs</strong></span>
              ${
                untrackedGap > 0
                  ? `<span class="font-bold text-amber-800">Cópias físicas / Mobile / Impressão IP direta sem Spooler: +${formatIntegerBr(untrackedGap)} págs</span>`
                  : '<span class="font-bold text-emerald-700">✓ 100% conciliado com Spooler</span>'
              }
            </div>`
          : '';
      jobsContainer.innerHTML = `${reconciliationHtml}
        <div class="overflow-x-auto">
          <table class="w-full text-left text-xs">
            <thead class="bg-slate-50 text-slate-500 uppercase text-[10px] font-bold border-b border-slate-200">
              <tr><th class="py-2.5 px-3">Setor</th><th class="py-2.5 px-3">Estação</th><th class="py-2.5 px-3">Usuário</th><th class="py-2.5 px-3 text-center">Jobs</th><th class="py-2.5 px-3 text-right">P&amp;B</th><th class="py-2.5 px-3 text-right">Cor</th><th class="py-2.5 px-3 text-right">Total</th><th class="py-2.5 px-3 text-right">Custo Est.</th></tr>
            </thead>
            <tbody class="divide-y divide-slate-100">
              ${rows
                .map(
                  r => `<tr class="hover:bg-slate-50">
                    <td class="py-2.5 px-3 font-bold text-indigo-800">${escapeHtml(r.dept)}</td>
                    <td class="py-2.5 px-3 mono text-slate-700">${escapeHtml(r.pc)}</td>
                    <td class="py-2.5 px-3 text-slate-800">${escapeHtml(r.usr)}</td>
                    <td class="py-2.5 px-3 text-center mono">${formatIntegerBr(r.jobs)}</td>
                    <td class="py-2.5 px-3 text-right mono">${formatIntegerBr(r.mono)}</td>
                    <td class="py-2.5 px-3 text-right mono text-indigo-700">${formatIntegerBr(r.color)}</td>
                    <td class="py-2.5 px-3 text-right mono font-black">${formatIntegerBr(r.total)}</td>
                    <td class="py-2.5 px-3 text-right mono font-bold text-emerald-700">${formatCurrencyBrl(r.estCost)}</td>
                  </tr>`,
                )
                .join('')}
            </tbody>
          </table>
        </div>`;
    }
  }

  // Supply Alerts Feed
  const alertsFeed = byId('supplyAlertsFeed');
  const alertsBadge = byId('supplyAlertsBadge');
  const openAlerts = appState.supplyAlerts.filter(a => !a.resolved && String(a.status ?? '').toLowerCase() !== 'resolved');
  if (alertsBadge) alertsBadge.innerText = `${openAlerts.length} pendente(s) • ${appState.supplyAlerts.length} total`;
  if (alertsFeed) {
    if (appState.supplyAlerts.length === 0) {
      alertsFeed.innerHTML = `<div class="p-4 rounded-2xl bg-slate-50 border border-slate-200 text-xs text-slate-500 text-center">Nenhum alerta automático de insumo até o momento.</div>`;
    } else {
      alertsFeed.innerHTML = appState.supplyAlerts
        .map(a => {
          const prt = appState.printers.find(p => p.id === a.printer_id);
          const cli = prt ? appState.clients.find(c => c.id === prt.client_id) : appState.clients.find(c => c.id === a.client_id);
          const meta = getAlertTypeMeta(a.alert_type ?? a.type);
          const isResolved = Boolean(a.resolved) || String(a.status ?? '').toLowerCase() === 'resolved';
          const colorFocus = detectSupplyColor(String(a.supply_name ?? a.message ?? ''));
          const spareWidget = !isResolved && cli ? renderSpareTonersInlineWidget(cli, colorFocus) : '';
          return `
            <div class="p-3.5 rounded-2xl border ${isResolved ? 'bg-slate-50 border-slate-200 opacity-70' : 'bg-white border-amber-200 shadow-sm'} space-y-2 text-xs">
              <div class="flex flex-wrap items-center justify-between gap-3">
                <div class="space-y-1">
                  <div class="flex flex-wrap items-center gap-2">
                    <span class="px-2 py-0.5 rounded-full text-[10px] font-black uppercase border ${meta.cls}">${escapeHtml(meta.label)}</span>
                    <strong class="text-slate-900">${escapeHtml(prt ? prt.model : (a.printer_model ?? 'Impressora'))}</strong>
                    ${cli ? `<span class="text-[11px] text-indigo-700 font-semibold">• ${escapeHtml(cli.name)}</span>` : ''}
                  </div>
                  <p class="text-slate-600">${escapeHtml(a.message ?? a.details ?? a.supply_name ?? 'Evento de auditoria de insumo.')}</p>
                </div>
                <div>
                  ${
                    isResolved
                      ? '<span class="px-2.5 py-1 rounded-xl bg-emerald-50 text-emerald-700 font-bold text-[11px] border border-emerald-200">✓ Resolvido</span>'
                      : `<button type="button" data-action="resolve-alert" data-alert-id="${a.id}" class="px-3 py-1.5 rounded-xl bg-slate-900 hover:bg-slate-800 text-white font-bold text-[11px] transition">Marcar como Resolvido</button>`
                  }
                </div>
              </div>
              ${spareWidget}
            </div>`;
        })
        .join('');
    }
  }

  const suppliesAuditGrid = byId('suppliesAuditGrid');
  if (suppliesAuditGrid) {
    suppliesAuditGrid.innerHTML =
      appState.printers.length === 0
        ? `<div class="col-span-full text-slate-400 py-4 text-center">Nenhuma impressora no radar.</div>`
        : appState.printers
            .map(p => {
              const list = Array.isArray(p.supplies) ? p.supplies : [];
              const norm = list.map(normalizeSupplyItem);
              const lowItem = norm.find(x => x.isLowAlert);
              const hasCrit = norm.some(x => x.isWasteAlert || x.isLowAlert);
              const cli = appState.clients.find(c => c.id === p.client_id);
              const badgeCls = hasCrit ? 'bg-rose-100 text-rose-800' : 'bg-emerald-100 text-emerald-800';
              return `
                <div class="p-4 rounded-2xl border border-slate-200 bg-white space-y-2.5 text-xs">
                  <div class="flex items-center justify-between gap-2">
                    <strong class="text-slate-900 truncate">${escapeHtml(p.model)}</strong>
                    <span class="px-2 py-0.5 rounded-full text-[10px] font-black ${badgeCls}">${hasCrit ? 'ATENÇÃO' : 'MONITORADO'}</span>
                  </div>
                  <div class="text-[11px] text-slate-400 mono">${escapeHtml(p.serial)} • ${escapeHtml(cli ? cli.name : 'Avulsa')}</div>
                  ${lowItem && cli ? renderSpareTonersInlineWidget(cli, lowItem.colorKey ?? 'K') : ''}
                </div>`;
            })
            .join('');
  }

  if (appState.latestAiInsight) {
    renderGeminiInsights(appState.latestAiInsight);
  }
  refreshIcons();
}

function downloadCsv(filename: string, lines: string[]): void {
  const blob = new Blob(['\uFEFF' + lines.join('\n')], { type: 'text/csv;charset=utf-8;' });
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
}

export function initReportsView(callbacks: ReportsCallbacks): void {
  reportsCallbacks = callbacks;

  byId('repSubBtnClosing')?.addEventListener('click', () => switchReportsSubTab('closing'));
  byId('repSubBtnSupplies')?.addEventListener('click', () => switchReportsSubTab('supplies'));
  byId('repSubBtnAi')?.addEventListener('click', () => switchReportsSubTab('ai'));

  byId('reportsTableBody')?.addEventListener('click', e => {
    const btn = (e.target as HTMLElement).closest<HTMLElement>('[data-action]');
    if (!btn) return;
    const cid = btn.dataset.clientId ?? '';
    if (btn.dataset.action === 'rep-adj' && cid) openClientAdjustmentModal(cid);
    else if (btn.dataset.action === 'rep-invoice' && cid) reportsCallbacks.onOpenInvoice(cid);
  });

  byId('supplyAlertsFeed')?.addEventListener('click', e => {
    const btn = (e.target as HTMLElement).closest<HTMLElement>('[data-action]');
    if (!btn) return;
    const action = btn.dataset.action;
    if (action === 'resolve-alert') {
      const aid = btn.dataset.alertId ?? '';
      if (!aid) return;
      void (async () => {
        await sb
          .from('supply_alerts')
          .update({ resolved: true, resolved_at: new Date().toISOString() })
          .eq('id', aid);
        await loadTenantData();
        reportsCallbacks.onDataChanged();
      })();
    } else if (action === 'spare-delta') {
      const cid = btn.dataset.clientId ?? '';
      const col = (btn.dataset.color ?? 'K') as keyof SpareTonersStock;
      const delta = Number(btn.dataset.delta ?? 0);
      if (cid && delta) void adjustClientSpareToner(cid, col, delta);
    } else if (action === 'open-spare') {
      const cid = btn.dataset.clientId ?? '';
      if (cid) openSpareTonersModal(cid);
    }
  });

  byId('reportsPeriodMonth')?.addEventListener('change', () => {
    resolveReportsPeriodMonth();
    renderReportsView();
  });

  byId('btnExportErpCSV')?.addEventListener('click', () => {
    const periodMonth = resolveReportsPeriodMonth();
    const header = [
      'Cliente',
      'CNPJ',
      'Contrato',
      'Grupo_Pool',
      'Franquia_PB',
      'Franquia_Cor',
      'Delta_PB',
      'Delta_Color',
      'Desconto_Tecnico',
      'Delta_Troca',
      'Excedente_PB_R$',
      'Excedente_Cor_R$',
      'Contratos_Maq_R$',
      'Total_Fatura_R$',
    ];
    const lines = [header.join(';')];
    appState.clients.forEach(c => {
      const m = computeClientMetrics(c, appState.printers, appState.telemetryReadings, appState.closings, periodMonth);
      lines.push(
        [
          c.name,
          c.cnpj ?? '',
          c.contract_num ?? '',
          m.poolGroup,
          c.franchise_mono ?? 0,
          c.franchise_color ?? 0,
          m.netMono,
          m.netColor,
          m.techDedMono + m.techDedColor,
          m.replacedMono + m.replacedColor,
          m.exMono.toFixed(2),
          m.exColor.toFixed(2),
          m.customMachinesAmount.toFixed(2),
          m.total.toFixed(2),
        ]
          .map(v => `"${String(v).replace(/"/g, '""')}"`)
          .join(';'),
      );
    });
    downloadCsv(`printmind_erp_faturamento_${periodMonth}.csv`, lines);
  });

  byId('btnExportMachineCSV')?.addEventListener('click', () => {
    const periodMonth = resolveReportsPeriodMonth();
    const header = [
      'Cliente',
      'Unidade',
      'Setor',
      'Impressora',
      'Serial',
      'Modo_Cobranca',
      'Status_Real',
      'Ini_PB',
      'Fin_PB',
      'Delta_PB',
      'Ini_Cor',
      'Fin_Cor',
      'Delta_Cor',
      'Delta_Total',
      'Subtotal_R$',
    ];
    const lines = [header.join(';')];
    appState.printers.forEach(p => {
      const c = appState.clients.find(x => x.id === p.client_id);
      const st = getEffectivePrinterStatus(p);
      const pm = getPrinterPeriodMetrics(p, appState.telemetryReadings, periodMonth);
      const mode = normalizeBillingMode(p);
      let subtotal = 0;
      if (c) {
        const cm = computeClientMetrics(c, appState.printers, appState.telemetryReadings, appState.closings, periodMonth);
        const foundItem = cm.printerItems.find(i => i.printer.id === p.id);
        if (foundItem) {
          subtotal = computePrinterInvoiceSubtotal({ metrics: cm, item: foundItem });
        }
      }
      lines.push(
        [
          c ? c.name : 'Avulsa',
          p.unit ?? 'Matriz',
          p.department ?? 'Geral',
          p.model,
          p.serial,
          mode,
          st.label,
          pm.startMono,
          pm.finalMono,
          pm.deltaMono,
          pm.startColor,
          pm.finalColor,
          pm.deltaColor,
          pm.deltaTotal,
          subtotal.toFixed(2),
        ]
          .map(v => `"${String(v).replace(/"/g, '""')}"`)
          .join(';'),
      );
    });
    downloadCsv(`printmind_fechamento_maquinas_${periodMonth}.csv`, lines);
  });

  byId('btnFreezeAllClosings')?.addEventListener('click', () => {
    void (async () => {
      const ten = appState.tenant;
      if (!ten) return;
      const periodMonth = resolveReportsPeriodMonth();
      if (!confirm(`Deseja congelar todos os faturamentos em aberto de ${periodMonth}? Fechamentos congelados geram snapshot imutável.`)) {
        return;
      }
      const [yrStr, moStr] = periodMonth.split('-');
      const yyyy = Number(yrStr) || new Date().getFullYear();
      const monthIdx = Math.max(0, Math.min(11, (Number(moStr) || new Date().getMonth() + 1) - 1));
      for (const c of appState.clients) {
        const m = computeClientMetrics(c, appState.printers, appState.telemetryReadings, appState.closings, periodMonth);
        if (!m.isFrozen) {
          const dueDateObj = new Date(yyyy, monthIdx, Math.min(28, Number(c.closing_day || 15) + 5));
          const pixRes = buildPixPayloadStrict({
            pixKey: ten.pix_key,
            merchantName: ten.trade_name,
            merchantCity: ten.city,
            amount: m.total,
            txId: `FAT${periodMonth.replace('-', '')}`,
          });
          await toggleClientClosingFreeze({
            tenantId: ten.id,
            client: c,
            metrics: m,
            pixPayload: pixRes.ok ? pixRes.payload : null,
            periodMonth,
            dueDateIso: dueDateObj.toISOString().slice(0, 10),
          });
        }
      }
      await loadTenantData();
      reportsCallbacks.onDataChanged();
      alert('Fechamentos congelados com sucesso!');
    })();
  });

  byId('btnGenerateGeminiInsights')?.addEventListener('click', () => {
    void (async () => {
      const ten = appState.tenant;
      if (!ten) return;
      const statusEl = byId('geminiStatusMsg');
      const btn = byId<HTMLButtonElement>('btnGenerateGeminiInsights');
      if (btn) btn.disabled = true;
      if (statusEl) {
        statusEl.classList.remove('hidden', 'text-rose-200', 'text-emerald-200');
        statusEl.innerText = 'Consultando PrintMind Cloud AI (dados anonimizados)...';
      }

      const res = await invokeCloudAiInsights(ten.id, false);
      if (btn) btn.disabled = false;

      if (!res.ok || !res.insights) {
        if (statusEl) {
          statusEl.classList.add('text-rose-200');
          statusEl.innerText = res.message ?? 'Não foi possível gerar análise.';
        }
        return;
      }

      renderGeminiInsights(res.insights);
      if (statusEl) {
        statusEl.classList.add('text-emerald-200');
        statusEl.innerText = res.cached
          ? `✓ Exibindo análise recente em cache (${res.modelUsed || 'Cloud AI'}).`
          : `✓ Análise gerada via PrintMind Cloud AI (${res.modelUsed || 'Cloud AI'}).`;
      }
    })();
  });
}
