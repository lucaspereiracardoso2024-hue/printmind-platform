import { getPrinterPeriodMetrics } from '../domain/billingEngine';
import { getEffectivePrinterStatus, normalizeSupplyItem, renderSupplyListHtml } from '../domain/supplyPresenter';
import { appState, byId } from '../state/store';
import { computeDailyProductionSeries } from '../utils/dailySeries';
import { escapeHtml, formatIntegerBr } from '../utils/format';

export function renderGraficaView(): void {
  let sumMono = 0;
  let sumColor = 0;
  appState.printers.forEach(p => {
    const pm = getPrinterPeriodMetrics(p, appState.telemetryReadings);
    sumMono += pm.deltaMono;
    sumColor += pm.deltaColor;
  });
  const sumPages = sumMono + sumColor;

  const setText = (id: string, val: string): void => {
    const el = byId(id);
    if (el) el.innerText = val;
  };

  setText('graficaTotalPages', `${formatIntegerBr(sumPages)} folhas`);
  setText('graficaColorPages', `${formatIntegerBr(sumColor)} págs`);
  setText('graficaMonoPages', `${formatIntegerBr(sumMono)} págs`);

  const colorPct = sumPages > 0 ? Math.round((sumColor / sumPages) * 100) : 0;
  setText('graficaColorPercent', `${colorPct}% • Impressões coloridas`);
  setText('graficaMonoPercent', `${sumPages > 0 ? 100 - colorPct : 0}% • Documentos monocromáticos`);

  const series = computeDailyProductionSeries(appState.telemetryReadings, 14);
  const daysWithProd = series.filter(d => d.total > 0);
  const total14d = series.reduce((acc, d) => acc + d.total, 0);
  const avg = daysWithProd.length > 0 ? Math.round(total14d / daysWithProd.length) : 0;
  const peakDay = series.reduce((best, d) => (d.total > best.total ? d : best), { total: 0, label: '-' });

  setText('graficaAvgDay', `${formatIntegerBr(avg)} págs/dia`);
  setText(
    'graficaPeakRecord',
    peakDay.total > 0
      ? `Pico real (14d): ${formatIntegerBr(peakDay.total)} págs (${peakDay.label})`
      : 'Aguardando múltiplas leituras diárias',
  );

  const barsContainer = byId('graficaDailyBars');
  if (barsContainer) {
    if (appState.telemetryReadings.length === 0 || total14d === 0) {
      barsContainer.innerHTML = `<div class="w-full h-full flex items-center justify-center text-center text-xs text-slate-400 px-4">Ainda não há histórico diário suficiente. Assim que o scanner registrar leituras ao longo do dia, o gráfico aparecerá aqui.</div>`;
    } else {
      const maxVal = Math.max(...series.map(d => d.total), 1);
      barsContainer.innerHTML = series
        .map(d => {
          const totalPct = Math.round((d.total / maxVal) * 100);
          const colorShare = d.total > 0 ? Math.round((d.color / d.total) * 100) : 0;
          const monoShare = Math.max(0, 100 - colorShare);
          return `
            <div class="flex-1 min-w-[34px] flex flex-col items-center gap-1" title="${d.label}: ${formatIntegerBr(d.total)} págs">
              <div class="w-full bg-slate-100 rounded-t-lg flex flex-col justify-end overflow-hidden" style="height:120px">
                <div class="w-full flex flex-col justify-end overflow-hidden" style="height:${totalPct}%">
                  <div class="w-full bg-emerald-500" style="height:${colorShare}%"></div>
                  <div class="w-full bg-blue-700" style="height:${monoShare}%"></div>
                </div>
              </div>
              <span class="text-[10px] font-bold text-slate-600 mono">${d.label}</span>
            </div>`;
        })
        .join('');
    }
  }

  const cmykBuckets: Record<'K' | 'C' | 'M' | 'Y', number[]> = { K: [], C: [], M: [], Y: [] };
  const colorPrinters = appState.printers.filter(p => {
    if (p.is_color_capable) return true;
    const sup = Array.isArray(p.supplies) ? p.supplies : [];
    return sup.some(s => {
      const ck = normalizeSupplyItem(s).colorKey;
      return ck === 'C' || ck === 'M' || ck === 'Y';
    });
  });

  colorPrinters.forEach(p => {
    const sup = Array.isArray(p.supplies) ? p.supplies : [];
    sup.forEach(s => {
      const item = normalizeSupplyItem(s);
      if (item.colorKey && item.hasKnownPct && item.pct !== null) {
        cmykBuckets[item.colorKey].push(item.pct);
      }
    });
  });

  const avgOrLabel = (arr: number[]): string =>
    arr.length > 0 ? `${Math.round(arr.reduce((a, b) => a + b, 0) / arr.length)}%` : 'Sem sensor %';

  const summaryHtml = `
    <div class="col-span-full p-4 rounded-2xl bg-slate-50 border border-slate-200 space-y-2">
      <div class="flex flex-wrap items-center justify-between gap-2">
        <strong class="text-xs font-black text-slate-900">Média Real CMYK (${colorPrinters.length} impressora(s) colorida(s))</strong>
        <span class="text-[11px] text-slate-500">Sem percentuais estimados — apenas hardware com sensor contínuo</span>
      </div>
      <div class="grid grid-cols-2 sm:grid-cols-4 gap-2 text-center text-xs mono">
        <div class="bg-white p-2.5 rounded-xl border border-slate-200"><span class="text-[10px] font-bold text-slate-500 block">BLACK (K)</span><strong class="text-slate-900">${avgOrLabel(cmykBuckets.K)}</strong></div>
        <div class="bg-white p-2.5 rounded-xl border border-slate-200"><span class="text-[10px] font-bold text-cyan-600 block">CYAN (C)</span><strong class="text-cyan-700">${avgOrLabel(cmykBuckets.C)}</strong></div>
        <div class="bg-white p-2.5 rounded-xl border border-slate-200"><span class="text-[10px] font-bold text-pink-600 block">MAGENTA (M)</span><strong class="text-pink-700">${avgOrLabel(cmykBuckets.M)}</strong></div>
        <div class="bg-white p-2.5 rounded-xl border border-slate-200"><span class="text-[10px] font-bold text-amber-500 block">YELLOW (Y)</span><strong class="text-amber-600">${avgOrLabel(cmykBuckets.Y)}</strong></div>
      </div>
    </div>`;

  const parkEl = byId('graficaSuppliesPark');
  if (parkEl) {
    parkEl.innerHTML =
      appState.printers.length === 0
        ? `<div class="col-span-full text-slate-400 py-4 text-center">Nenhuma impressora conectada ainda.</div>`
        : summaryHtml +
          appState.printers
            .map(p => {
              const st = getEffectivePrinterStatus(p);
              return `
                <div class="p-5 rounded-3xl bg-slate-50 border ${st.isOffline ? 'border-rose-200' : 'border-slate-200'} space-y-3">
                  <div class="flex justify-between items-start gap-2">
                    <div>
                      <strong class="text-sm font-black text-slate-900">${escapeHtml(p.model)}</strong>
                      <span class="text-[11px] mono text-slate-500 block">${escapeHtml(p.serial)} • ${escapeHtml(p.department ?? 'Produção')}</span>
                    </div>
                    <span class="px-2 py-0.5 rounded-full text-[10px] font-bold ${st.badgeClass}">${escapeHtml(st.label)}</span>
                  </div>
                  ${renderSupplyListHtml(p.supplies, false)}
                </div>`;
            })
            .join('');
  }
}
