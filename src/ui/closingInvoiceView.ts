import { buildPixPayloadStrict } from '../domain/pixEmvco';
import {
  calculateDepartmentAllocation,
  computeClientMetrics,
  computePrinterInvoiceSubtotal,
} from '../domain/billingEngine';
import { emitSplitCharge, toggleClientClosingFreeze } from '../services/billingService';
import { appState, byId, loadTenantData, openModal } from '../state/store';
import { computeDailyProductionSeries, DailySeriesPoint } from '../utils/dailySeries';
import { escapeHtml, formatCurrencyBrl, formatIntegerBr, getCurrentMonthPrefix } from '../utils/format';
import { refreshIcons } from '../utils/icons';
import { renderQrDataUrl } from '../utils/qrcodeRender';
import { checkTrialPrinterLock, switchTab } from './navigation';

type InvoiceCallbacks = {
  onDataChanged: () => void;
};

let invoiceCallbacks: InvoiceCallbacks = {
  onDataChanged: () => {},
};

function renderFormalDailyMiniChartHtml(
  series: DailySeriesPoint[],
  field: 'mono' | 'color',
  title: string,
  barColorClass: string,
  badgeColorClass: string,
): string {
  const totalSum = series.reduce((acc, d) => acc + d[field], 0);
  const maxVal = Math.max(...series.map(d => d[field]), 1);
  return `
    <div class="p-3 rounded-2xl border border-slate-200 bg-slate-50/60 space-y-2">
      <div class="flex items-center justify-between gap-2">
        <span class="text-[11px] font-black text-slate-800">${escapeHtml(title)}</span>
        <span class="px-2 py-0.5 rounded-full text-[10px] font-bold mono ${badgeColorClass}">${formatIntegerBr(totalSum)} págs (14d)</span>
      </div>
      <div class="h-16 flex items-end gap-1 pt-2 border-b border-slate-200">
        ${series
          .map(d => {
            const val = d[field];
            const pct = totalSum > 0 && val > 0 ? Math.max(8, Math.round((val / maxVal) * 100)) : 2;
            return `<div class="flex-1 flex flex-col items-center justify-end h-full" title="${d.label}: ${formatIntegerBr(val)}">
              <div class="w-full rounded-t ${val > 0 ? barColorClass : 'bg-slate-200'}" style="height:${pct}%"></div>
            </div>`;
          })
          .join('')}
      </div>
    </div>`;
}

function resolveInvoicePeriodMonth(): string {
  const inputVal = byId<HTMLInputElement>('invoicePeriodMonth')?.value.trim() ?? '';
  if (/^\d{4}-\d{2}$/.test(inputVal)) {
    appState.selectedPeriodMonth = inputVal;
    return inputVal;
  }
  const fallback = /^\d{4}-\d{2}$/.test(appState.selectedPeriodMonth)
    ? appState.selectedPeriodMonth
    : getCurrentMonthPrefix();
  return fallback;
}

export function openClientInvoice(clientId: string): void {
  const c = appState.clients.find(x => x.id === clientId);
  if (!c) return;
  appState.activeClientForInvoice = c;

  const periodMonth = resolveInvoicePeriodMonth();
  const invMonthInput = byId<HTMLInputElement>('invoicePeriodMonth');
  if (invMonthInput) invMonthInput.value = periodMonth;

  const m = computeClientMetrics(c, appState.printers, appState.telemetryReadings, appState.closings, periodMonth);
  const now = new Date();
  const [yrStr, moStr] = periodMonth.split('-');
  const yyyy = Number(yrStr) || now.getFullYear();
  const monthIdx = Math.max(0, Math.min(11, (Number(moStr) || now.getMonth() + 1) - 1));
  const mm = String(monthIdx + 1).padStart(2, '0');
  const closingDay = String(Math.min(28, Math.max(1, Number(c.closing_day || 15)))).padStart(2, '0');
  const prevDate = new Date(yyyy, monthIdx - 1, Number(closingDay));
  const periodStr = `${prevDate.toLocaleDateString('pt-BR')} a ${closingDay}/${mm}/${yyyy}`;
  const dueDateObj = new Date(yyyy, monthIdx, Math.min(28, Number(closingDay) + 5));

  const ten = appState.tenant;
  const setText = (id: string, text: string): void => {
    const el = byId(id);
    if (el) el.innerText = text;
  };

  setText('formalLocadoraName', (ten?.trade_name || 'MINHA LOCADORA').toUpperCase());
  const cityLabel = ten?.city ? `${ten.city}${ten.state_uf ? '/' + ten.state_uf : ''}` : 'Cidade não cadastrada';
  setText(
    'formalLocadoraDoc',
    `CNPJ/CPF: ${ten?.cnpj_cpf || 'Não informado'} • ${cityLabel} • ${appState.session?.user?.email ?? ''}`,
  );
  setText('formalInvoiceNum', `FAT-${yyyy}/${mm}${closingDay}`);
  setText('formalIssueDate', `Data de Emissão: ${now.toLocaleDateString('pt-BR')}`);
  setText('formalPeriod', periodStr);
  setText('formalDueDate', dueDateObj.toLocaleDateString('pt-BR'));
  setText('formalClientName', c.name);
  setText('formalClientDoc', `CNPJ/CPF: ${c.cnpj || 'Não informado'}`);
  setText('formalClientAddress', `Unidades: ${(c.units ?? ['Matriz']).join(', ')}`);
  setText('formalContractNum', c.contract_num || `CT-${yyyy}/01`);

  const poolRow = byId('formalPoolGroupRow');
  const poolEl = byId('formalPoolGroup');
  if (poolRow && poolEl) {
    poolRow.classList.toggle('hidden', !m.poolGroup);
    poolEl.innerText = m.poolGroup;
  }

  const telemetryBody = byId('formalTelemetryRows');
  if (telemetryBody) {
    telemetryBody.innerHTML =
      m.printerItems.length === 0
        ? `<tr><td colspan="9" class="py-4 text-center text-slate-400">Nenhuma impressora vinculada a este contrato.</td></tr>`
        : m.printerItems
            .map(item => {
              const p = item.printer;
              const subtotal = computePrinterInvoiceSubtotal({ metrics: m, item });
              return `
                <tr class="hover:bg-slate-50">
                  <td class="py-2.5 px-3">
                    <strong>${escapeHtml(p.model)}</strong> (${escapeHtml(p.serial)})
                    ${item.isExempt ? '<span class="ml-1 text-[9px] px-1.5 py-0.5 rounded bg-amber-100 text-amber-900 font-bold">Isenta</span>' : ''}
                    ${item.counterResetDetected ? '<span class="ml-1 text-[9px] px-1.5 py-0.5 rounded bg-rose-100 text-rose-800 font-bold">Reset detectado</span>' : ''}
                  </td>
                  <td class="py-2.5 px-2 text-right">${formatIntegerBr(item.startMono)}</td>
                  <td class="py-2.5 px-2 text-right">${formatIntegerBr(item.finalMono)}</td>
                  <td class="py-2.5 px-2 text-right text-slate-700 font-semibold">${formatIntegerBr(item.deltaMono)}</td>
                  <td class="py-2.5 px-2 text-right">${formatIntegerBr(item.startColor)}</td>
                  <td class="py-2.5 px-2 text-right">${formatIntegerBr(item.finalColor)}</td>
                  <td class="py-2.5 px-2 text-right text-indigo-700 font-bold">${formatIntegerBr(item.deltaColor)}</td>
                  <td class="py-2.5 px-2 text-right font-black text-[#0038A8]">${formatIntegerBr(item.deltaTotal)}</td>
                  <td class="py-2.5 px-3 text-right font-black text-slate-900">${formatCurrencyBrl(subtotal)}</td>
                </tr>`;
            })
            .join('');
  }

  const formalChartsGrid = byId('formalDailyChartsGrid');
  if (formalChartsGrid) {
    const clientSeries = computeDailyProductionSeries(
      appState.telemetryReadings,
      14,
      new Set(m.linked.map(p => p.id)),
    );
    const total14d = clientSeries.reduce((acc, d) => acc + d.mono + d.color, 0);
    formalChartsGrid.classList.toggle('hidden', total14d === 0);
    formalChartsGrid.innerHTML =
      total14d === 0
        ? ''
        : renderFormalDailyMiniChartHtml(clientSeries, 'mono', 'Total P&B por Dia', 'bg-[#0038A8]', 'bg-blue-100 text-[#0038A8]') +
          renderFormalDailyMiniChartHtml(clientSeries, 'color', 'Total Colorido por Dia', 'bg-emerald-500', 'bg-emerald-100 text-emerald-800');
  }

  const deptCards = byId('formalDeptCards');
  if (deptCards) {
    const billablePrinters = m.printerItems.filter(item => !item.isExempt).map(item => item.printer);
    const allocPrinters = billablePrinters.length > 0 ? billablePrinters : m.linked;
    const allocPrinterIds = new Set(allocPrinters.map(p => p.id));
    const clientSpoolerJobs = appState.printJobsSummary.filter(
      j => (j.printer_id && allocPrinterIds.has(j.printer_id)) || (!j.printer_id && j.client_id === c.id),
    );
    const deptAlloc = calculateDepartmentAllocation({
      odometerTotalPages: m.dTotal,
      totalInvoiceCents: m.totalCents,
      printers: allocPrinters,
      spoolerJobs: clientSpoolerJobs,
      periodMonth,
    });

    if (m.printerItems.length === 0 && deptAlloc.allocations.length === 0) {
      deptCards.innerHTML = `<div class="col-span-full text-slate-400 text-xs">Sem máquinas para rateio neste período.</div>`;
    } else {
      const spoolerFallbackBanner = !deptAlloc.hasSpoolerData
        ? `<div class="col-span-full p-3 rounded-2xl bg-slate-50 border border-slate-200 text-[11px] text-slate-600">
            <strong class="text-slate-800">Spooler PC sem registros no período (0 págs):</strong> rateio por setor detalhado indisponível sem eventos de fila Windows/CUPS; volume contabilizado pelo odômetro real do hardware / contrato base.
          </div>`
        : '';

      const shareBasisLabel =
        deptAlloc.totalSpoolerPages + deptAlloc.untrackedPages > 0
          ? 'do volume apurado'
          : 'do rateio contratual';

      const cardsHtml = deptAlloc.allocations
        .map(alloc => {
          const cardClass = alloc.isUntracked
            ? 'p-3 rounded-2xl bg-amber-50/70 border border-amber-200'
            : 'p-3 rounded-2xl bg-indigo-50/60 border border-indigo-100';
          const titleClass = alloc.isUntracked ? 'text-amber-900' : 'text-indigo-800';
          const valueClass = alloc.isUntracked ? 'text-amber-950' : 'text-indigo-950';
          return `
            <div class="${cardClass}">
              <span class="text-[10px] font-black ${titleClass} uppercase block">${escapeHtml(alloc.department)}</span>
              <span class="text-[11px] text-slate-600 block">${alloc.percentage.toFixed(1)}% ${shareBasisLabel}</span>
              <div class="mt-1 flex justify-between items-center text-xs">
                <span class="mono text-slate-500">${formatIntegerBr(alloc.pages)} págs</span>
                <strong class="mono ${valueClass} font-black">${formatCurrencyBrl(alloc.amountBrl)}</strong>
              </div>
            </div>`;
        })
        .join('');

      deptCards.innerHTML = spoolerFallbackBanner + cardsHtml;
    }
  }

  const adjBox = byId('formalAdjustmentBox');
  if (adjBox) {
    const hasAdj =
      m.replacedMono > 0 || m.replacedColor > 0 || m.techDedMono > 0 || m.techDedColor > 0 || Boolean(m.adjustmentNote) || Boolean(m.poolGroup);
    adjBox.classList.toggle('hidden', !hasAdj);
    if (hasAdj) {
      adjBox.innerHTML = `
        <div class="flex flex-wrap items-center justify-between gap-2">
          <strong class="text-xs font-black uppercase tracking-wider text-amber-950">Ajustes Auditados &amp; Apuração Líquida Faturável</strong>
          ${m.poolGroup ? `<span class="px-2 py-0.5 rounded-full text-[10px] font-black bg-indigo-100 text-indigo-900">Pool: ${escapeHtml(m.poolGroup)}</span>` : ''}
        </div>
        <div class="grid grid-cols-2 md:grid-cols-4 gap-2 mono">
          <div class="p-2 rounded-xl bg-white border border-amber-200"><span class="text-[10px] text-slate-400 block">Bruto Hardware</span><strong>${formatIntegerBr(m.rawMono)} P&amp;B • ${formatIntegerBr(m.rawColor)} Cor</strong></div>
          <div class="p-2 rounded-xl bg-white border border-amber-200"><span class="text-[10px] text-indigo-700 block">(+) Troca no Mês</span><strong class="text-indigo-800">+${formatIntegerBr(m.replacedMono)} / +${formatIntegerBr(m.replacedColor)}</strong></div>
          <div class="p-2 rounded-xl bg-white border border-amber-200"><span class="text-[10px] text-rose-700 block">(-) Abatimento Téc.</span><strong class="text-rose-700">-${formatIntegerBr(m.techDedMono)} / -${formatIntegerBr(m.techDedColor)}</strong></div>
          <div class="p-2 rounded-xl bg-blue-50 border border-blue-200"><span class="text-[10px] text-[#0038A8] block">(=) Líquido</span><strong class="text-[#0038A8]">${formatIntegerBr(m.netMono)} P&amp;B • ${formatIntegerBr(m.netColor)} Cor</strong></div>
        </div>
        ${m.adjustmentNote ? `<div class="text-amber-950"><strong>Observação Auditada:</strong> ${escapeHtml(m.adjustmentNote)}</div>` : ''}`;
    } else {
      adjBox.innerHTML = '';
    }
  }

  setText('formalFranqMono', `${formatIntegerBr(c.franchise_mono ?? 0)} págs`);
  setText('formalFranqColor', `${formatIntegerBr(c.franchise_color ?? 0)} págs`);
  setText('formalRateMono', `R$ ${Number(c.extra_mono ?? 0.05).toFixed(3)}`);
  setText('formalRateColor', `R$ ${Number(c.extra_color ?? 0.1).toFixed(3)}`);
  setText('formalBaseRate', formatCurrencyBrl(m.clientContractBase));
  setText('formalExcedMono', formatCurrencyBrl(m.exMono));
  setText('formalExcedColor', formatCurrencyBrl(m.exColor));
  const customMachRow = byId('formalCustomMachinesRow');
  if (customMachRow) {
    customMachRow.classList.toggle('hidden', m.customMachinesAmount <= 0);
  }
  setText('formalCustomMachines', formatCurrencyBrl(m.customMachinesAmount));
  setText('formalGrandTotal', formatCurrencyBrl(m.total));

  // Strict PIX EMVCo check — ZERO fake fallback CNPJ / SAO PAULO!
  const pixResult = buildPixPayloadStrict({
    pixKey: ten?.pix_key,
    merchantName: ten?.trade_name,
    merchantCity: ten?.city,
    amount: m.total,
    txId: `FAT${yyyy}${mm}`,
  });

  const pixInput = byId<HTMLInputElement>('invPixInput');
  const pixWarning = byId('invPixWarning');
  const pixQrImg = byId<HTMLImageElement>('invPixQrImg');
  const pixValidBox = byId('invPixValidBox');

  if (pixResult.ok) {
    pixWarning?.classList.add('hidden');
    pixValidBox?.classList.remove('hidden');
    if (pixInput) pixInput.value = pixResult.payload;
    if (pixQrImg) {
      void renderQrDataUrl(pixResult.payload, 140).then(url => {
        pixQrImg.src = url;
      });
    }
  } else {
    pixValidBox?.classList.add('hidden');
    if (pixInput) pixInput.value = '';
    if (pixWarning) {
      pixWarning.classList.remove('hidden');
      pixWarning.innerHTML = `
        <div class="flex flex-wrap items-center justify-between gap-2">
          <span>${escapeHtml(pixResult.message)}</span>
          <button type="button" id="btnGoCompanyForPix" class="px-3 py-1 rounded-xl bg-amber-900 text-white text-[11px] font-bold">Completar em Minha Empresa</button>
        </div>`;
      byId('btnGoCompanyForPix')?.addEventListener('click', () => {
        byId('invoiceModal')?.classList.add('hidden');
        switchTab('company');
      });
    }
  }

  const freezeLbl = byId('btnFreezeLabel');
  if (freezeLbl) {
    freezeLbl.innerText = m.isFrozen ? 'Fechamento Congelado (Imutável)' : 'Congelar Faturamento';
  }
  const snapHashInfo = byId('invSnapshotHashInfo');
  if (snapHashInfo) {
    if (m.closing?.snapshot_hash) {
      snapHashInfo.classList.remove('hidden');
      snapHashInfo.innerText = `Auditoria SHA-256: ${m.closing.snapshot_hash}`;
    } else {
      snapHashInfo.classList.add('hidden');
    }
  }

  openModal('invoiceModal');
  checkTrialPrinterLock();
  refreshIcons();
}

export function sendFormalWhatsApp(): void {
  const c = appState.activeClientForInvoice;
  if (!c) return;
  const periodMonth = resolveInvoicePeriodMonth();
  const m = computeClientMetrics(c, appState.printers, appState.telemetryReadings, appState.closings, periodMonth);
  const phone = String(c.phone ?? '').replace(/\D/g, '');
  const pixKey = String(appState.tenant?.pix_key ?? '').trim();

  const lines: string[] = [
    `Olá ${c.name}!`,
    '',
    'Segue o Demonstrativo de Fechamento de Impressão:',
    `- Período: Corte Dia ${c.closing_day}`,
    `- Total Líquido Faturável: ${formatIntegerBr(m.dTotal)} págs (${formatIntegerBr(m.dMono)} P&B • ${formatIntegerBr(m.dColor)} Cor)`,
    `- Total da Fatura: ${formatCurrencyBrl(m.total)}`,
  ];
  if (pixKey) {
    lines.push(`- Chave PIX (${appState.tenant?.trade_name || 'Locadora'}): ${pixKey}`);
  }
  if (m.poolGroup) lines.push(`- Grupo / Pool Compartilhado: ${m.poolGroup}`);
  if (m.adjustmentNote) lines.push(`- Observação: ${m.adjustmentNote}`);
  lines.push('', 'Podemos emitir a Nota Fiscal? Obrigado!');

  const text = encodeURIComponent(lines.join('\n'));
  const url = phone ? `https://api.whatsapp.com/send?phone=55${phone}&text=${text}` : `https://api.whatsapp.com/send?text=${text}`;
  window.open(url, '_blank');
}

export function sendQuickWhatsAppClient(clientId: string): void {
  const c = appState.clients.find(x => x.id === clientId);
  if (!c) return;
  appState.activeClientForInvoice = c;
  sendFormalWhatsApp();
}

export function initClosingInvoiceView(callbacks: InvoiceCallbacks): void {
  invoiceCallbacks = callbacks;

  byId('btnPrintFormalInvoice')?.addEventListener('click', () => window.print());
  byId('btnSendFormalWhatsApp')?.addEventListener('click', () => sendFormalWhatsApp());

  byId('btnCopyPixCode')?.addEventListener('click', () => {
    const val = byId<HTMLInputElement>('invPixInput')?.value ?? '';
    if (!val) {
      alert('Configure Chave PIX e Cidade em Minha Empresa para gerar o código Copia e Cola.');
      return;
    }
    void navigator.clipboard?.writeText(val);
    alert('Código PIX Copia e Cola copiado!');
  });

  byId('invoicePeriodMonth')?.addEventListener('change', () => {
    resolveInvoicePeriodMonth();
    if (appState.activeClientForInvoice) {
      openClientInvoice(appState.activeClientForInvoice.id);
    }
  });

  byId('btnToggleFreezeClosing')?.addEventListener('click', () => {
    void (async () => {
      const c = appState.activeClientForInvoice;
      const ten = appState.tenant;
      if (!c || !ten) return;
      const periodMonth = resolveInvoicePeriodMonth();
      const [yrStr, moStr] = periodMonth.split('-');
      const yyyy = Number(yrStr) || new Date().getFullYear();
      const monthIdx = Math.max(0, Math.min(11, (Number(moStr) || new Date().getMonth() + 1) - 1));
      const m = computeClientMetrics(c, appState.printers, appState.telemetryReadings, appState.closings, periodMonth);
      const dueDateObj = new Date(yyyy, monthIdx, Math.min(28, Number(c.closing_day || 15) + 5));
      const dueDateIso = dueDateObj.toISOString().slice(0, 10);
      const pixRes = buildPixPayloadStrict({
        pixKey: ten.pix_key,
        merchantName: ten.trade_name,
        merchantCity: ten.city,
        amount: m.total,
        txId: `FAT${periodMonth.replace('-', '')}`,
      });

      const res = await toggleClientClosingFreeze({
        tenantId: ten.id,
        client: c,
        metrics: m,
        pixPayload: pixRes.ok ? pixRes.payload : null,
        periodMonth,
        dueDateIso,
      });

      if (!res.ok) {
        alert(res.message ?? 'Não foi possível atualizar status do fechamento.');
        return;
      }
      await loadTenantData();
      openClientInvoice(c.id);
      invoiceCallbacks.onDataChanged();
    })();
  });

  byId('btnEmitAsaasSplit')?.addEventListener('click', () => {
    void (async () => {
      const c = appState.activeClientForInvoice;
      const ten = appState.tenant;
      if (!c || !ten) return;

      const periodMonth = resolveInvoicePeriodMonth();
      const [yrStr, moStr] = periodMonth.split('-');
      const yyyy = Number(yrStr) || new Date().getFullYear();
      const monthIdx = Math.max(0, Math.min(11, (Number(moStr) || new Date().getMonth() + 1) - 1));
      const m = computeClientMetrics(c, appState.printers, appState.telemetryReadings, appState.closings, periodMonth);
      const dueDateObj = new Date(yyyy, monthIdx, Math.min(28, Number(c.closing_day || 15) + 5));
      const dueDateIso = dueDateObj.toISOString().slice(0, 10);

      const res = await emitSplitCharge({
        tenantId: ten.id,
        clientId: c.id,
        closingId: m.closing?.id,
        clientName: c.name,
        totalAmount: m.total,
        dueDateIso,
        billingType: 'PIX',
        description: `Fatura PrintMind (${periodMonth}) — ${c.name}`,
      });

      if (res.status !== 'created') {
        alert(res.message ?? 'Não foi possível emitir cobrança no Asaas.');
        return;
      }

      if (res.pix_payload && byId<HTMLInputElement>('invPixInput')) {
        byId('invPixValidBox')?.classList.remove('hidden');
        byId('invPixWarning')?.classList.add('hidden');
        byId<HTMLInputElement>('invPixInput')!.value = res.pix_payload;
        if (byId<HTMLImageElement>('invPixQrImg') && res.pix_qr_base64) {
          byId<HTMLImageElement>('invPixQrImg')!.src = `data:image/png;base64,${res.pix_qr_base64}`;
        }
      }
      if (res.invoice_url) {
        window.open(res.invoice_url, '_blank');
      }
      await loadTenantData();
      invoiceCallbacks.onDataChanged();
    })();
  });
}
