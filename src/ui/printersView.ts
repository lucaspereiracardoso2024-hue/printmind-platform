import { APP_VERSION, sb } from '../services/supabaseClient';
import { getOrCreatePortalToken } from '../services/portalService';
import { appState, byId, closeModal, getActiveUnitPairingToken, isMasterAdminUser, loadTenantData, openModal } from '../state/store';
import { BillingMode, Printer, SpareTonersStock } from '../types/models';
import {
  FREE_TRIAL_PRINTER_LIMIT,
  getBillablePrintersSummary,
  getPrinterPeriodMetrics,
  normalizeBillingMode,
  resolveEffectiveCourtesyLimit,
} from '../domain/billingEngine';
import { getEffectivePrinterStatus, normalizeSupplyItem, renderSupplyListHtml } from '../domain/supplyPresenter';
import { escapeHtml, formatIntegerBr, getCurrentMonthPrefix } from '../utils/format';
import { refreshIcons } from '../utils/icons';
import { renderQrDataUrl } from '../utils/qrcodeRender';
import { adjustClientSpareToner, openClientModalForCreate, openSpareTonersModal, renderSpareTonersInlineWidget } from './clientsView';

type PrintersCallbacks = {
  onDataChanged: () => void;
  onOpenPricing: () => void;
};

let printerCallbacks: PrintersCallbacks = {
  onDataChanged: () => {},
  onOpenPricing: () => {},
};

let activeDetailPrinterId: string | null = null;

export function downloadPairedAgent(osType: 'windows' | 'linux' = 'windows', explicitToken = ''): void {
  const token = String(explicitToken || getActiveUnitPairingToken()).trim();
  if (!token) {
    alert('Cadastre seu primeiro cliente/unidade antes de baixar o scanner para que ele já saia pareado automaticamente.');
    closeModal('agentModal');
    closeModal('newPrinterModal');
    openClientModalForCreate();
    return;
  }
  void navigator.clipboard?.writeText(token);
  const url = osType === 'linux' ? './printmind-agent-linux-amd64' : './printmind-agent.exe';
  const filename =
    osType === 'linux' ? `printmind-agent-linux-amd64__${token}` : `printmind-agent__${token}.exe`;

  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
}

export function copyMsiSilentCommand(explicitToken = ''): void {
  const tok = String(explicitToken || getActiveUnitPairingToken() || 'SEU_TOKEN_AQUI').trim();
  const cmd = `msiexec /i PrintMindAgentSetup-v${APP_VERSION}-x64.msi PAIRINGTOKEN="${tok}" /quiet`;
  void navigator.clipboard?.writeText(cmd);
  const txt = byId('btnCopyMsiText');
  if (txt && !explicitToken) {
    txt.innerText = '✓ Comando MSI Copiado!';
    setTimeout(() => {
      txt.innerText = 'Copiar Comando MSI';
    }, 2500);
  } else {
    alert(`Comando MSI copiado!\n\n${cmd}`);
  }
}

export function switchPrinterAddMode(mode: 'auto' | 'manual'): void {
  const autoBox = byId('prtAddAutoPane');
  const manBox = byId('prtAddManualPane');
  const btnAuto = byId('btnPrtModeAuto');
  const btnMan = byId('btnPrtModeManual');
  const isAuto = mode === 'auto';

  autoBox?.classList.toggle('hidden', !isAuto);
  manBox?.classList.toggle('hidden', isAuto);
  if (btnAuto) {
    btnAuto.className = isAuto
      ? 'flex-1 py-2 rounded-xl bg-[#0038A8] text-white font-bold text-xs shadow-sm transition'
      : 'flex-1 py-2 rounded-xl bg-slate-100 text-slate-700 font-bold text-xs transition';
  }
  if (btnMan) {
    btnMan.className = !isAuto
      ? 'flex-1 py-2 rounded-xl bg-[#0038A8] text-white font-bold text-xs shadow-sm transition'
      : 'flex-1 py-2 rounded-xl bg-slate-100 text-slate-700 font-bold text-xs transition';
  }
}

export function populatePrinterFormSelects(): void {
  const sel = byId<HTMLSelectElement>('prtClientSelect');
  if (sel) {
    sel.innerHTML =
      '<option value="">Sem cliente (Avulsa / Em estoque)</option>' +
      appState.clients.map(c => `<option value="${c.id}">${escapeHtml(c.name)}</option>`).join('');
  }
  const autoSel = byId<HTMLSelectElement>('autoClientTokenSelect');
  if (autoSel) {
    const opts = appState.units
      .filter(u => Boolean(u.pairing_token))
      .map(u => {
        const c = appState.clients.find(x => x.id === u.client_id);
        return `<option value="${escapeHtml(u.pairing_token ?? '')}">${escapeHtml(c ? c.name : 'Conta')} • ${escapeHtml(u.unit_name)}</option>`;
      });
    autoSel.innerHTML = opts.length > 0 ? opts.join('') : '<option value="">Cadastre um cliente primeiro</option>';
  }
}

function billingModeLabel(mode: BillingMode): string {
  switch (mode) {
    case 'fixed_only':
      return 'Fixo Mensal';
    case 'fixed_plus_franchise':
      return 'Fixo + Franquia/Máq';
    case 'per_page_only':
      return 'Por Página/Máq';
    case 'exempt':
      return 'Isenta (Fair Billing)';
    default:
      return 'Contrato do Cliente';
  }
}

function isPrinterUnitWppReady(p: Printer): boolean {
  return appState.units.some(
    u =>
      ((p.unit_id && u.id === p.unit_id) || (u.client_id === p.client_id && u.unit_name === p.unit)) &&
      u.wpp_enabled === true,
  );
}

export function renderPrintersGrid(): void {
  const grid = byId('printersGrid');
  const countBadge = byId('printersCountBadge');
  if (!grid) return;

  const searchTerm = (byId<HTMLInputElement>('printerSearchInput')?.value ?? '').trim().toLowerCase();
  const ownFilter = (byId<HTMLSelectElement>('printerOwnershipFilter')?.value ?? 'all').toLowerCase();

  const filtered = appState.printers.filter(p => {
    const c = appState.clients.find(x => x.id === p.client_id);
    const own = String(p.ownership ?? 'locada').toLowerCase();
    if (ownFilter === 'unlinked' && p.client_id) return false;
    if ((ownFilter === 'locada' || ownFilter === 'propria' || ownFilter === 'cliente') && own !== ownFilter) return false;
    if (!searchTerm) return true;
    const hay = `${p.model} ${p.serial} ${p.ip ?? ''} ${p.usb_port ?? ''} ${p.unit ?? ''} ${p.department ?? ''} ${c ? c.name : 'avulsa'}`.toLowerCase();
    return hay.includes(searchTerm);
  });

  if (countBadge) {
    countBadge.innerText =
      filtered.length !== appState.printers.length
        ? `${filtered.length}/${appState.printers.length}`
        : String(appState.printers.length);
  }

  if (appState.printers.length === 0) {
    grid.innerHTML = `
      <div class="col-span-full p-8 rounded-3xl bg-white border border-slate-200 text-center text-xs text-slate-400 space-y-2">
        <div class="font-bold text-slate-600">Nenhuma impressora conectada ainda.</div>
        <p>Clique em "+ Impressora" ou execute o Scanner Windows v${APP_VERSION} para localizar automaticamente na rede e no USB.</p>
      </div>`;
    return;
  }

  if (filtered.length === 0) {
    grid.innerHTML = `
      <div class="col-span-full p-8 rounded-3xl bg-white border border-slate-200 text-center text-xs text-slate-500">
        Nenhuma impressora encontrada para o filtro atual.
      </div>`;
    return;
  }

  const isGrafica = appState.tenant?.operation_mode === 'grafica';
  grid.innerHTML = filtered
    .map(p => {
      const pm = getPrinterPeriodMetrics(p, appState.telemetryReadings, appState.selectedPeriodMonth);
      const c = appState.clients.find(x => x.id === p.client_id);
      const st = getEffectivePrinterStatus(p);
      const isExempt = Boolean(p.billing_exempt);
      const mode = normalizeBillingMode(p);
      const own = String(p.ownership ?? 'locada').toLowerCase();
      const ownLabel = own === 'propria' ? 'Própria' : own === 'cliente' ? 'Do Cliente' : 'Locada';
      const suppliesArr = Array.isArray(p.supplies) ? p.supplies : [];
      const lowSupply = suppliesArr.map(normalizeSupplyItem).find(s => s.isLowAlert);
      const lowSpareWidget = !isGrafica && lowSupply && c ? renderSpareTonersInlineWidget(c, lowSupply.colorKey ?? 'K') : '';
      const resetBadge = pm.counterResetDetected
        ? '<span class="px-1.5 py-0.5 rounded-full text-[9px] font-black bg-rose-100 text-rose-800 border border-rose-300">Reset de Contador</span>'
        : '';
      const wppBadge = p.wpp_at_risk
        ? '<span class="px-1.5 py-0.5 rounded-full text-[9px] font-black bg-amber-100 text-amber-900 border border-amber-300">⚠ Risco WPP Win11</span>'
        : isPrinterUnitWppReady(p)
          ? '<span class="px-1.5 py-0.5 rounded-full text-[9px] font-bold bg-emerald-100 text-emerald-800">✓ WPP Win11</span>'
          : '';

      return `
        <div class="bg-white p-5 rounded-3xl border ${isExempt ? 'border-amber-300 bg-amber-50/15' : st.isOffline ? 'border-rose-200' : 'border-slate-200'} shadow-sm space-y-3 hover:shadow-md transition">
          <div class="flex justify-between items-start gap-2">
            <div class="min-w-0">
              <h4 data-action="detail-printer" data-printer-id="${p.id}" class="font-black text-slate-900 text-sm truncate cursor-pointer hover:text-[#0038A8] transition">${escapeHtml(p.model)}</h4>
              <span class="text-xs text-slate-400 mono block truncate">Série: ${escapeHtml(p.serial)} • ${escapeHtml(p.ip || p.usb_port || 'Local')}</span>
              <div class="text-[11px] font-bold text-indigo-700 mt-1 flex flex-wrap items-center gap-1">
                <span>${escapeHtml(!isGrafica && c ? c.name : p.unit || 'Matriz')} • ${escapeHtml(p.department || 'Geral')}</span>
                ${!isGrafica ? `<span class="px-1.5 py-0.5 rounded-full text-[9px] font-bold bg-slate-100 text-slate-700 border border-slate-200">${ownLabel}</span>` : ''}
                ${!isGrafica && mode !== 'inherit_client' && !isExempt ? `<span class="px-1.5 py-0.5 rounded-full text-[9px] font-bold bg-indigo-100 text-indigo-800">${billingModeLabel(mode)}</span>` : ''}
                ${!isGrafica && isExempt ? '<span class="px-1.5 py-0.5 rounded-full text-[9px] font-black bg-amber-100 text-amber-900 border border-amber-300">ISENTA</span>' : ''}
                ${wppBadge}
                ${resetBadge}
              </div>
            </div>
            <span class="px-2 py-0.5 rounded-full text-[10px] font-black uppercase whitespace-nowrap ${st.badgeClass}">${escapeHtml(st.label)}</span>
          </div>
          <div class="pt-2 border-t border-slate-100 grid grid-cols-3 gap-2 text-center text-xs mono">
            <div class="bg-slate-50 p-2 rounded-xl"><span class="text-[10px] text-slate-400 block">P&amp;B Mês</span><strong>${formatIntegerBr(pm.deltaMono)}</strong></div>
            <div class="bg-indigo-50/60 p-2 rounded-xl"><span class="text-[10px] text-indigo-500 block">Color Mês</span><strong class="text-indigo-700">${formatIntegerBr(pm.deltaColor)}</strong></div>
            <div class="bg-blue-50/60 p-2 rounded-xl"><span class="text-[10px] text-[#0038A8] block">Total Mês</span><strong class="text-[#0038A8]">${formatIntegerBr(pm.deltaTotal)}</strong></div>
          </div>
          <div class="px-2.5 py-1.5 rounded-xl bg-slate-50 border border-slate-100 flex items-center justify-between text-[10px] text-slate-600 mono">
            <span>Odômetro acumulado</span>
            <strong>${formatIntegerBr(Number(p.total_pages ?? 0))} (${formatIntegerBr(Number(p.mono_pages ?? 0))} P&amp;B • ${formatIntegerBr(Number(p.color_pages ?? 0))} Cor)</strong>
          </div>
          <div class="pt-2 border-t border-slate-100 space-y-1.5">
            <div class="flex items-center justify-between text-[10px] font-bold uppercase text-slate-400">
              <span>Suprimentos &amp; Hardware</span>
              <span class="mono">${escapeHtml(p.discovery_source || 'rede')}</span>
            </div>
            ${renderSupplyListHtml(p.supplies, true)}
            ${lowSpareWidget}
          </div>
          <div class="pt-2 border-t border-slate-100 flex flex-wrap items-center justify-between gap-1.5 text-[11px]">
            ${
              !isGrafica
                ? `<button type="button" data-action="toggle-exempt" data-printer-id="${p.id}" class="px-2.5 py-1 rounded-xl border font-bold transition ${isExempt ? 'bg-amber-100 border-amber-300 text-amber-950' : 'bg-slate-50 border-slate-200 text-slate-600 hover:bg-slate-100'}">${isExempt ? 'Reativar Cobrança' : 'Isentar'}</button>`
                : '<span></span>'
            }
            <div class="flex items-center gap-1.5">
              ${!isGrafica ? `<button type="button" data-action="portal-qr" data-printer-id="${p.id}" class="px-2.5 py-1 rounded-xl bg-emerald-50 hover:bg-emerald-100 text-emerald-800 border border-emerald-200 font-bold transition">QR Portal</button>` : ''}
              <button type="button" data-action="detail-printer" data-printer-id="${p.id}" class="px-3 py-1 rounded-xl bg-blue-50 hover:bg-blue-100 text-[#0038A8] font-bold transition">Ficha / Contrato</button>
            </div>
          </div>
        </div>`;
    })
    .join('');

  refreshIcons();
}

function renderPrinterDetailModalBody(): void {
  const p = appState.printers.find(x => x.id === activeDetailPrinterId);
  const body = byId('printerDetailBody');
  const title = byId('printerDetailTitle');
  if (!p || !body) return;

  if (title) title.innerText = `${p.model} • ${p.serial}`;
  const pm = getPrinterPeriodMetrics(p, appState.telemetryReadings);
  const mode = normalizeBillingMode(p);

  body.innerHTML = `
    <div class="grid grid-cols-1 md:grid-cols-2 gap-4 text-xs">
      <div class="p-4 rounded-2xl bg-slate-50 border border-slate-200 space-y-3">
        <strong class="text-slate-900 uppercase text-[11px] block">Vínculo &amp; Regra Contratual da Máquina</strong>
        <form id="formPrinterContractDetail" class="space-y-2.5">
          <div>
            <label class="text-[11px] font-bold text-slate-600 block">Cliente Vinculado</label>
            <select id="editPrtClientId" class="w-full px-3 py-2 rounded-xl border border-slate-300 bg-white">
              <option value="">Sem cliente (Avulsa)</option>
              ${appState.clients.map(c => `<option value="${c.id}" ${p.client_id === c.id ? 'selected' : ''}>${escapeHtml(c.name)}</option>`).join('')}
            </select>
          </div>
          <div class="grid grid-cols-2 gap-2">
            <div>
              <label class="text-[11px] font-bold text-slate-600 block">Unidade</label>
              <input id="editPrtUnit" type="text" value="${escapeHtml(p.unit ?? 'Matriz')}" class="w-full px-3 py-2 rounded-xl border border-slate-300 bg-white" />
            </div>
            <div>
              <label class="text-[11px] font-bold text-slate-600 block">Setor</label>
              <input id="editPrtDept" type="text" value="${escapeHtml(p.department ?? 'Geral')}" class="w-full px-3 py-2 rounded-xl border border-slate-300 bg-white" />
            </div>
          </div>
          <div class="grid grid-cols-2 gap-2">
            <div>
              <label class="text-[11px] font-bold text-slate-600 block">Propriedade</label>
              <select id="editPrtOwnership" class="w-full px-3 py-2 rounded-xl border border-slate-300 bg-white">
                <option value="locada" ${p.ownership === 'locada' ? 'selected' : ''}>Locada (Nossa)</option>
                <option value="propria" ${p.ownership === 'propria' ? 'selected' : ''}>Própria (Gráfica)</option>
                <option value="cliente" ${p.ownership === 'cliente' ? 'selected' : ''}>Do Cliente</option>
              </select>
            </div>
            <div>
              <label class="text-[11px] font-bold text-slate-600 block">Modo de Cobrança</label>
              <select id="editPrtBillingMode" class="w-full px-3 py-2 rounded-xl border border-slate-300 bg-white">
                <option value="inherit_client" ${mode === 'inherit_client' ? 'selected' : ''}>Herdar do Cliente</option>
                <option value="fixed_only" ${mode === 'fixed_only' ? 'selected' : ''}>Valor Fixo da Máquina</option>
                <option value="fixed_plus_franchise" ${mode === 'fixed_plus_franchise' ? 'selected' : ''}>Fixo + Franquia Individual</option>
                <option value="per_page_only" ${mode === 'per_page_only' ? 'selected' : ''}>Somente por Página</option>
                <option value="exempt" ${mode === 'exempt' ? 'selected' : ''}>Isenta (Fair Billing)</option>
              </select>
            </div>
          </div>
          <div class="grid grid-cols-3 gap-2">
            <div>
              <label class="text-[10px] font-bold text-slate-600 block">Fixo Máq (R$)</label>
              <input id="editPrtContractBase" type="number" step="0.01" value="${Number(p.contract_base_rate ?? 0)}" class="w-full px-2.5 py-1.5 rounded-xl border border-slate-300 bg-white mono" />
            </div>
            <div>
              <label class="text-[10px] font-bold text-slate-600 block">Franq. P&amp;B</label>
              <input id="editPrtFranMono" type="number" value="${Number(p.contract_franchise_mono ?? 0)}" class="w-full px-2.5 py-1.5 rounded-xl border border-slate-300 bg-white mono" />
            </div>
            <div>
              <label class="text-[10px] font-bold text-slate-600 block">Exc. P&amp;B (R$)</label>
              <input id="editPrtExtraMono" type="number" step="0.001" value="${Number(p.contract_extra_mono ?? 0.05)}" class="w-full px-2.5 py-1.5 rounded-xl border border-slate-300 bg-white mono" />
            </div>
          </div>
          <div class="grid grid-cols-2 gap-2">
            <div>
              <label class="text-[10px] font-bold text-slate-600 block">Franq. Color</label>
              <input id="editPrtFranColor" type="number" value="${Number(p.contract_franchise_color ?? 0)}" class="w-full px-2.5 py-1.5 rounded-xl border border-slate-300 bg-white mono" />
            </div>
            <div>
              <label class="text-[10px] font-bold text-slate-600 block">Exc. Color (R$)</label>
              <input id="editPrtExtraColor" type="number" step="0.001" value="${Number(p.contract_extra_color ?? 0.1)}" class="w-full px-2.5 py-1.5 rounded-xl border border-slate-300 bg-white mono" />
            </div>
          </div>
          <div class="grid grid-cols-2 gap-2">
            <div>
              <label class="text-[11px] font-bold text-slate-600 block">Modelo</label>
              <input id="editPrtModel" type="text" value="${escapeHtml(p.model)}" class="w-full px-3 py-2 rounded-xl border border-slate-300 bg-white" />
            </div>
            <div>
              <label class="text-[11px] font-bold text-slate-600 block">IP / Porta</label>
              <input id="editPrtIp" type="text" value="${escapeHtml(p.ip ?? p.usb_port ?? '')}" class="w-full px-3 py-2 rounded-xl border border-slate-300 bg-white mono" />
            </div>
          </div>
          <button type="submit" class="w-full py-2.5 rounded-xl bg-[#0038A8] hover:bg-blue-800 text-white font-black transition">Salvar Dados &amp; Regra da Máquina</button>
        </form>
      </div>

      <div class="space-y-4">
        <div class="p-4 rounded-2xl bg-slate-50 border border-slate-200 space-y-3">
          <strong class="text-slate-900 uppercase text-[11px] block">Leitura &amp; Auditoria de Contadores (${escapeHtml(pm.startSource)})</strong>
          ${pm.counterResetDetected ? `<div class="p-2.5 rounded-xl bg-rose-50 border border-rose-200 text-rose-800 font-semibold">Reset de placa/contador detectado. Delta preservado em replaced_delta.</div>` : ''}
          <form id="formPrinterManualReading" class="grid grid-cols-2 gap-2.5">
            <div>
              <label class="text-[10px] font-bold text-slate-600 block">Total P&amp;B Atual</label>
              <input id="manualMonoInput" type="number" value="${Number(p.mono_pages ?? 0)}" class="w-full px-3 py-2 rounded-xl border border-slate-300 bg-white mono" />
            </div>
            <div>
              <label class="text-[10px] font-bold text-slate-600 block">Total Colorido Atual</label>
              <input id="manualColorInput" type="number" value="${Number(p.color_pages ?? 0)}" class="w-full px-3 py-2 rounded-xl border border-slate-300 bg-white mono" />
            </div>
            <div>
              <label class="text-[10px] font-bold text-slate-600 block">Base Inicial P&amp;B</label>
              <input id="manualInitMonoInput" type="number" value="${Number(p.initial_mono_pages ?? 0)}" class="w-full px-3 py-2 rounded-xl border border-slate-300 bg-white mono" />
            </div>
            <div>
              <label class="text-[10px] font-bold text-slate-600 block">Base Inicial Color</label>
              <input id="manualInitColorInput" type="number" value="${Number(p.initial_color_pages ?? 0)}" class="w-full px-3 py-2 rounded-xl border border-slate-300 bg-white mono" />
            </div>
            <button type="submit" class="col-span-2 py-2 rounded-xl bg-slate-900 hover:bg-slate-800 text-white font-bold transition">Registrar Leitura Manual</button>
          </form>
        </div>

        <div class="p-4 rounded-2xl bg-slate-50 border border-slate-200 space-y-2">
          <strong class="text-slate-900 uppercase text-[11px] block">Suprimentos &amp; Cabeçotes Detalhados</strong>
          ${renderSupplyListHtml(p.supplies, false)}
        </div>

        <button type="button" id="btnDeletePrinterModal" class="w-full py-2.5 rounded-xl bg-rose-50 hover:bg-rose-100 text-rose-700 border border-rose-200 font-bold transition">
          Excluir Impressora
        </button>
      </div>
    </div>`;

  byId<HTMLFormElement>('formPrinterContractDetail')?.addEventListener('submit', e => {
    e.preventDefault();
    void savePrinterDetails(p.id);
  });

  byId<HTMLFormElement>('formPrinterManualReading')?.addEventListener('submit', e => {
    e.preventDefault();
    void saveManualPrinterReading(p.id);
  });

  byId('btnDeletePrinterModal')?.addEventListener('click', () => void deletePrinter(p.id));
}

export function openPrinterDetailModal(printerId: string): void {
  activeDetailPrinterId = printerId;
  renderPrinterDetailModalBody();
  openModal('printerDetailModal');
}

async function savePrinterDetails(printerId: string): Promise<void> {
  const client_id = byId<HTMLSelectElement>('editPrtClientId')?.value.trim() || null;
  const ownership = byId<HTMLSelectElement>('editPrtOwnership')?.value ?? 'locada';
  const billing_mode = byId<HTMLSelectElement>('editPrtBillingMode')?.value ?? 'inherit_client';
  const unit = byId<HTMLInputElement>('editPrtUnit')?.value.trim() || 'Matriz';
  const department = byId<HTMLInputElement>('editPrtDept')?.value.trim() || 'Geral';
  const model = byId<HTMLInputElement>('editPrtModel')?.value.trim() || '';
  const ip = byId<HTMLInputElement>('editPrtIp')?.value.trim() || null;

  const contract_base_rate = Number(byId<HTMLInputElement>('editPrtContractBase')?.value ?? 0);
  const contract_franchise_mono = Number(byId<HTMLInputElement>('editPrtFranMono')?.value ?? 0);
  const contract_extra_mono = Number(byId<HTMLInputElement>('editPrtExtraMono')?.value ?? 0.05);
  const contract_franchise_color = Number(byId<HTMLInputElement>('editPrtFranColor')?.value ?? 0);
  const contract_extra_color = Number(byId<HTMLInputElement>('editPrtExtraColor')?.value ?? 0.1);

  const { error } = await sb
    .from('printers')
    .update({
      client_id,
      ownership,
      billing_mode,
      billing_exempt: billing_mode === 'exempt',
      unit,
      department,
      model,
      ip,
      contract_base_rate,
      contract_franchise_mono,
      contract_extra_mono,
      contract_franchise_color,
      contract_extra_color,
    })
    .eq('id', printerId);

  if (error) {
    alert('Erro ao salvar regra da impressora: ' + error.message);
    return;
  }
  await loadTenantData();
  renderPrinterDetailModalBody();
  printerCallbacks.onDataChanged();
  alert('Dados e regra contratual atualizados!');
}

async function saveManualPrinterReading(printerId: string): Promise<void> {
  const p = appState.printers.find(x => x.id === printerId);
  if (!p || !appState.tenant) return;
  const newMono = Math.max(0, Number(byId<HTMLInputElement>('manualMonoInput')?.value ?? 0));
  const newColor = Math.max(0, Number(byId<HTMLInputElement>('manualColorInput')?.value ?? 0));
  const initMono = Math.max(0, Number(byId<HTMLInputElement>('manualInitMonoInput')?.value ?? 0));
  const initColor = Math.max(0, Number(byId<HTMLInputElement>('manualInitColorInput')?.value ?? 0));
  const newTotal = newMono + newColor;

  const { error } = await sb
    .from('printers')
    .update({
      mono_pages: newMono,
      color_pages: newColor,
      total_pages: newTotal,
      initial_mono_pages: initMono,
      initial_color_pages: initColor,
      initial_total_pages: initMono + initColor,
      last_seen: new Date().toISOString(),
    })
    .eq('id', printerId);

  if (error) {
    alert('Erro ao atualizar leitura manual: ' + error.message);
    return;
  }

  const monthPrefix = getCurrentMonthPrefix();
  const hasMonthReading = appState.telemetryReadings.some(
    r => r.printer_id === printerId && String(r.recorded_at ?? '').startsWith(monthPrefix),
  );
  if (!hasMonthReading) {
    const baseIso = new Date(Date.now() - 60000).toISOString();
    const { error: baseReadErr } = await sb.from('telemetry_readings').insert({
      tenant_id: appState.tenant.id,
      printer_id: printerId,
      client_id: p.client_id ?? null,
      idempotency_key: `${printerId}-base-${baseIso.slice(0, 16).replace(/[:T]/g, '-')}`,
      total_pages: initMono + initColor,
      mono_pages: initMono,
      color_pages: initColor,
      recorded_at: baseIso,
    });
    if (baseReadErr) {
      console.warn('Falha ao gravar leitura inicial de histórico:', baseReadErr.message);
    }
  }
  const nowIso = new Date().toISOString();
  const { error: curReadErr } = await sb.from('telemetry_readings').insert({
    tenant_id: appState.tenant.id,
    printer_id: printerId,
    client_id: p.client_id ?? null,
    idempotency_key: `${printerId}-${nowIso.slice(0, 16).replace(/[:T]/g, '-')}`,
    total_pages: newTotal,
    mono_pages: newMono,
    color_pages: newColor,
    recorded_at: nowIso,
  });
  if (curReadErr) {
    console.warn('Falha ao gravar leitura manual de histórico:', curReadErr.message);
  }

  await loadTenantData();
  renderPrinterDetailModalBody();
  printerCallbacks.onDataChanged();
  alert('Contador atualizado!');
}

async function deletePrinter(printerId: string): Promise<void> {
  const p = appState.printers.find(x => x.id === printerId);
  if (!p) return;
  if (!confirm(`Excluir a impressora "${p.model}" (${p.serial})?`)) return;
  const { error } = await sb.from('printers').delete().eq('id', printerId);
  if (error) {
    alert('Erro ao excluir: ' + error.message);
    return;
  }
  closeModal('printerDetailModal');
  await loadTenantData();
  printerCallbacks.onDataChanged();
}

export async function togglePrinterBillingExempt(printerId: string): Promise<void> {
  const p = appState.printers.find(x => x.id === printerId);
  if (!p) return;
  const nextVal = !p.billing_exempt;
  const { error } = await sb
    .from('printers')
    .update({
      billing_exempt: nextVal,
      billing_mode: nextVal ? 'exempt' : 'inherit_client',
    })
    .eq('id', printerId);
  if (error) {
    alert('Erro ao atualizar isenção Fair Billing: ' + error.message);
    return;
  }
  p.billing_exempt = nextVal;
  p.billing_mode = nextVal ? 'exempt' : 'inherit_client';
  printerCallbacks.onDataChanged();
}

async function renderPortalQrSticker(printerId: string, rotate: boolean, applyPin = false): Promise<void> {
  const p = appState.printers.find(x => x.id === printerId);
  const body = byId('portalQrModalBody');
  if (!p || !body) return;

  const pinInput = byId<HTMLInputElement>('portalStickerPinInput');
  const rawPin = (pinInput?.value ?? '').replace(/[^A-Za-z0-9]/g, '');
  if (applyPin && rawPin.length < 4) {
    alert('O PIN do portal deve conter ao menos 4 caracteres alfanuméricos.');
    return;
  }

  body.innerHTML = `<div class="py-6 text-center text-xs text-slate-500">Gerando / recuperando adesivo QR permanente...</div>`;
  const res = await getOrCreatePortalToken({
    printerId: p.id,
    pin: applyPin ? rawPin : undefined,
    rotate,
    label: `${p.model} (${p.serial})`,
  });

  if (res.status !== 'success' || !res.token_raw) {
    body.innerHTML = `<div class="p-4 rounded-2xl bg-rose-50 text-rose-700 text-xs font-bold">${escapeHtml(res.message ?? 'Erro ao gerar QR do Portal.')}</div>`;
    return;
  }

  const portalUrl = `${window.location.origin}${window.location.pathname}#portal=${res.token_raw}`;
  const qrDataUrl = await renderQrDataUrl(portalUrl, 220);
  const displayedPinLabel = applyPin
    ? `PIN atualizado: ${escapeHtml(rawPin)}`
    : res.reused
      ? 'PIN atual mantido (padrão inicial: 1234 ou o PIN já personalizado)'
      : 'PIN inicial da etiqueta: 1234';

  body.innerHTML = `
    <div id="printableStickerArea" class="p-5 rounded-3xl border-2 border-dashed border-slate-300 bg-white text-center space-y-3">
      <div class="text-[11px] font-black uppercase tracking-wider text-[#0038A8]">PrintMind • Suporte &amp; Suprimentos</div>
      <div class="text-sm font-black text-slate-900">${escapeHtml(p.model)}</div>
      <div class="text-xs mono text-slate-500">Série: ${escapeHtml(p.serial)} • ${escapeHtml(p.unit ?? 'Matriz')}</div>
      <img src="${qrDataUrl}" alt="QR Code Portal do Tomador" class="w-44 h-44 mx-auto rounded-xl border border-slate-200 p-1 bg-white" />
      <div class="text-xs text-slate-700 font-bold">Escaneie para pedir Toner ou Abrir Chamado</div>
      <div class="inline-block px-3 py-1 rounded-full bg-slate-100 text-slate-800 mono text-[11px] font-black">${displayedPinLabel}</div>
      <div class="text-[10px] text-slate-400">${res.reused ? 'Token permanente reutilizado — adesivos impressos anteriormente continuam válidos.' : 'Novo token emitido e salvo com segurança.'}</div>
    </div>
    <div class="flex flex-wrap gap-2 text-xs">
      <button type="button" id="btnCopyPortalLink" class="flex-1 py-2.5 rounded-xl bg-slate-100 hover:bg-slate-200 text-slate-800 font-bold">Copiar Link</button>
      <button type="button" id="btnPrintPortalSticker" class="flex-1 py-2.5 rounded-xl bg-[#0038A8] hover:bg-blue-800 text-white font-black">Imprimir Etiqueta</button>
      <button type="button" id="btnRotatePortalToken" class="px-3 py-2.5 rounded-xl bg-amber-50 hover:bg-amber-100 text-amber-900 border border-amber-200 font-bold">Rotacionar Token</button>
    </div>`;

  byId('btnCopyPortalLink')?.addEventListener('click', () => {
    void navigator.clipboard?.writeText(portalUrl);
    alert('Link do Portal do Tomador copiado!');
  });
  byId('btnPrintPortalSticker')?.addEventListener('click', () => window.print());
  byId('btnRotatePortalToken')?.addEventListener('click', () => {
    if (confirm('Rotacionar invalida o QR Code impresso anteriormente neste equipamento. Continuar?')) {
      void renderPortalQrSticker(printerId, true, applyPin);
    }
  });
}

export function openPortalQrModal(printerId: string): void {
  appState.activePortalPrinterId = printerId;
  const pinInput = byId<HTMLInputElement>('portalStickerPinInput');
  if (pinInput) pinInput.value = '1234';
  openModal('portalQrModal');
  void renderPortalQrSticker(printerId, false, false);
}

export function initPrintersView(callbacks: PrintersCallbacks): void {
  printerCallbacks = callbacks;

  byId('btnOpenNewPrinter')?.addEventListener('click', () => {
    populatePrinterFormSelects();
    switchPrinterAddMode('auto');
    openModal('newPrinterModal');
  });
  byId('btnPrtModeAuto')?.addEventListener('click', () => switchPrinterAddMode('auto'));
  byId('btnPrtModeManual')?.addEventListener('click', () => switchPrinterAddMode('manual'));
  byId('printerSearchInput')?.addEventListener('input', () => renderPrintersGrid());
  byId('printerOwnershipFilter')?.addEventListener('change', () => renderPrintersGrid());

  byId('btnDownloadAgentWin')?.addEventListener('click', () => {
    const tok = byId<HTMLSelectElement>('autoClientTokenSelect')?.value ?? '';
    downloadPairedAgent('windows', tok);
  });
  byId('btnDownloadAgentLinux')?.addEventListener('click', () => {
    const tok = byId<HTMLSelectElement>('autoClientTokenSelect')?.value ?? '';
    downloadPairedAgent('linux', tok);
  });
  byId('btnCopyMsiModal')?.addEventListener('click', () => {
    const tok = byId<HTMLSelectElement>('autoClientTokenSelect')?.value ?? '';
    copyMsiSilentCommand(tok);
  });

  byId('btnRegeneratePortalPin')?.addEventListener('click', () => {
    if (appState.activePortalPrinterId) {
      void renderPortalQrSticker(appState.activePortalPrinterId, false, true);
    }
  });

  byId('printersGrid')?.addEventListener('click', e => {
    const btn = (e.target as HTMLElement).closest<HTMLElement>('[data-action]');
    if (!btn) return;
    const action = btn.dataset.action;
    const pid = btn.dataset.printerId ?? '';
    const cid = btn.dataset.clientId ?? '';

    if (action === 'detail-printer' && pid) openPrinterDetailModal(pid);
    else if (action === 'toggle-exempt' && pid) void togglePrinterBillingExempt(pid);
    else if (action === 'portal-qr' && pid) openPortalQrModal(pid);
    else if (action === 'spare-delta' && cid) {
      const col = (btn.dataset.color ?? 'K') as keyof SpareTonersStock;
      const delta = Number(btn.dataset.delta ?? 0);
      if (delta) void adjustClientSpareToner(cid, col, delta);
    } else if (action === 'open-spare' && cid) openSpareTonersModal(cid);
  });

  byId<HTMLFormElement>('formNewPrinter')?.addEventListener('submit', e => {
    e.preventDefault();
    void (async () => {
      if (!appState.profile || !appState.tenant) return;
      const hasPaidPlan =
        appState.tenant.subscription_status === 'active' &&
        appState.tenant.plan_tier !== 'trial';
      const { billable } = getBillablePrintersSummary(appState.printers);

      if (!isMasterAdminUser() && !hasPaidPlan && billable.length >= FREE_TRIAL_PRINTER_LIMIT) {
        closeModal('newPrinterModal');
        printerCallbacks.onOpenPricing();
        alert(`Limite do Teste Gratuito atingido (${FREE_TRIAL_PRINTER_LIMIT} impressoras faturáveis). Ative um plano ou isente máquinas de bancada.`);
        return;
      }

      if (!isMasterAdminUser() && appState.tenant.pioneer_courtesy_active) {
        const minCharge = appState.platformSettings.asaas_min_charge_brl;
        const courtesyLimit = resolveEffectiveCourtesyLimit(
          appState.tenant.max_printers,
          minCharge,
          appState.tenant.plan_tier,
        );
        if (billable.length >= courtesyLimit) {
          closeModal('newPrinterModal');
          printerCallbacks.onOpenPricing();
          alert(`Limite da Cortesia Pioneiros atingido (${courtesyLimit} impressoras faturáveis). Para cadastrar mais máquinas, realize o checkout real no Asaas (>= R$ ${minCharge.toFixed(2)}).`);
          return;
        }
      }

      const client_id = byId<HTMLSelectElement>('prtClientSelect')?.value || null;
      const unit = byId<HTMLInputElement>('prtUnit')?.value.trim() || 'Matriz';
      const department = byId<HTMLInputElement>('prtDept')?.value.trim() || 'Geral';
      const model = byId<HTMLInputElement>('prtModel')?.value.trim() ?? '';
      const serial = byId<HTMLInputElement>('prtSerial')?.value.trim() ?? '';
      const ip = byId<HTMLInputElement>('prtIp')?.value.trim() || null;
      const initial_mono_pages = Math.max(0, Number(byId<HTMLInputElement>('prtInitMono')?.value || 0));
      const mono_pages = Math.max(0, Number(byId<HTMLInputElement>('prtCurMono')?.value || initial_mono_pages));
      const initial_color_pages = Math.max(0, Number(byId<HTMLInputElement>('prtInitColor')?.value || 0));
      const color_pages = Math.max(0, Number(byId<HTMLInputElement>('prtCurColor')?.value || initial_color_pages));

      const { data: inserted, error } = await sb
        .from('printers')
        .insert({
          tenant_id: appState.profile.tenant_id,
          client_id,
          unit,
          department,
          model,
          serial,
          ip,
          initial_mono_pages,
          mono_pages,
          initial_color_pages,
          color_pages,
          initial_total_pages: initial_mono_pages + initial_color_pages,
          total_pages: mono_pages + color_pages,
          is_color_capable: color_pages > 0,
          status: 'online',
          last_seen: new Date().toISOString(),
        })
        .select('*')
        .single();

      if (error || !inserted) {
        alert('Erro ao cadastrar impressora: ' + (error?.message ?? 'erro desconhecido'));
        return;
      }
      const p = inserted as Printer;
      const baseIso = new Date(Date.now() - 60000).toISOString();
      const { error: baseReadErr } = await sb.from('telemetry_readings').insert({
        tenant_id: appState.tenant.id,
        printer_id: p.id,
        client_id: p.client_id ?? null,
        idempotency_key: `${p.id}-base-${baseIso.slice(0, 16).replace(/[:T]/g, '-')}`,
        total_pages: initial_mono_pages + initial_color_pages,
        mono_pages: initial_mono_pages,
        color_pages: initial_color_pages,
        recorded_at: baseIso,
      });
      if (baseReadErr) {
        console.warn('Falha ao registrar leitura inicial da impressora:', baseReadErr.message);
      }

      (e.target as HTMLFormElement).reset();
      closeModal('newPrinterModal');
      await loadTenantData();
      printerCallbacks.onDataChanged();
    })();
  });
}
