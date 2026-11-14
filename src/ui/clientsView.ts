import { getOrCreatePortalToken } from '../services/portalService';
import { sb } from '../services/supabaseClient';
import { appState, byId, closeModal, loadTenantData, openModal } from '../state/store';
import { Client, SpareTonersStock } from '../types/models';
import { escapeHtml, formatCurrencyBrl, formatIntegerBr, isMissingColumnError } from '../utils/format';
import { refreshIcons } from '../utils/icons';
import { renderQrDataUrl } from '../utils/qrcodeRender';
import { centsToBrl, computeClientMetrics } from '../domain/billingEngine';
import { getEffectivePrinterStatus } from '../domain/supplyPresenter';
import { checkTrialPrinterLock } from './navigation';

let activePortalClientId: string | null = null;

const SPARE_TONER_COLORS: Array<{
  key: keyof SpareTonersStock;
  label: string;
  dot: string;
  pill: string;
}> = [
  { key: 'K', label: 'Preto (K)', dot: 'bg-slate-900', pill: 'bg-slate-100 text-slate-800 border-slate-200' },
  { key: 'C', label: 'Ciano (C)', dot: 'bg-cyan-500', pill: 'bg-cyan-50 text-cyan-800 border-cyan-200' },
  { key: 'M', label: 'Magenta (M)', dot: 'bg-pink-500', pill: 'bg-pink-50 text-pink-800 border-pink-200' },
  { key: 'Y', label: 'Amarelo (Y)', dot: 'bg-amber-400', pill: 'bg-amber-50 text-amber-800 border-amber-200' },
];

type ClientUiCallbacks = {
  onOpenInvoice: (clientId: string) => void;
  onSendQuickWhatsApp: (clientId: string) => void;
  onDataChanged: () => void;
};

let clientCallbacks: ClientUiCallbacks = {
  onOpenInvoice: () => {},
  onSendQuickWhatsApp: () => {},
  onDataChanged: () => {},
};

export function normalizeSpareToners(raw: Client['spare_toners']): SpareTonersStock {
  const src = raw && typeof raw === 'object' ? raw : {};
  return {
    K: Math.max(0, Number(src.K ?? 0) || 0),
    C: Math.max(0, Number(src.C ?? 0) || 0),
    M: Math.max(0, Number(src.M ?? 0) || 0),
    Y: Math.max(0, Number(src.Y ?? 0) || 0),
  };
}

export function renderSpareTonersSummaryPills(c: Client): string {
  const sp = normalizeSpareToners(c.spare_toners);
  const items = SPARE_TONER_COLORS.filter(col => sp[col.key] > 0);
  if (items.length === 0) return '';
  return items
    .map(
      col =>
        `<span class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded-md border text-[9px] font-bold mono ${col.pill}">
          <span class="w-1.5 h-1.5 rounded-full ${col.dot}"></span>${col.key}: ${sp[col.key]}
        </span>`,
    )
    .join('');
}

export function renderSpareTonersInlineWidget(c: Client, focusColorKey: keyof SpareTonersStock = 'K'): string {
  const sp = normalizeSpareToners(c.spare_toners);
  const col = SPARE_TONER_COLORS.find(x => x.key === focusColorKey) ?? SPARE_TONER_COLORS[0];
  const qty = sp[col.key];
  const statusCls = qty > 0 ? 'bg-emerald-50 border-emerald-200 text-emerald-900' : 'bg-rose-50 border-rose-200 text-rose-900';
  const statusText = qty > 0 ? `${qty} reserva(s) de ${col.key} no cliente` : `SEM reserva de ${col.key} — enviar novo toner!`;

  return `
    <div class="p-2 rounded-xl border ${statusCls} flex flex-wrap items-center justify-between gap-2 text-[10px]">
      <span class="font-bold flex items-center gap-1.5">
        <span class="w-2 h-2 rounded-full ${col.dot}"></span>${escapeHtml(statusText)}
      </span>
      <div class="flex items-center gap-1">
        ${
          qty > 0
            ? `<button type="button" data-action="spare-delta" data-client-id="${c.id}" data-color="${col.key}" data-delta="-1" class="px-2 py-0.5 rounded-lg bg-white border border-slate-300 font-bold hover:bg-slate-50 transition">-1 Consumido</button>`
            : `<button type="button" data-action="spare-delta" data-client-id="${c.id}" data-color="${col.key}" data-delta="1" class="px-2 py-0.5 rounded-lg bg-slate-900 text-white font-bold hover:bg-slate-800 transition">+1 Enviar</button>`
        }
        <button type="button" data-action="open-spare" data-client-id="${c.id}" class="px-1.5 py-0.5 rounded-lg bg-white/80 border border-slate-200 font-semibold hover:bg-white transition">Estoque</button>
      </div>
    </div>`;
}

export function isClientWppReady(clientId: string): boolean {
  return appState.units.some(u => u.client_id === clientId && u.wpp_enabled === true);
}

function getDueWindowScore(closingDay: number | null | undefined, todayDay: number): number | null {
  const cd = Number(closingDay ?? 0);
  if (!Number.isFinite(cd) || cd < 1 || cd > 31) return null;
  const diff = cd - todayDay;
  if (diff >= -1 && diff <= 3) return Math.abs(diff);
  return null;
}

export function renderSpareTonersModalBody(): void {
  const c = appState.clients.find(x => x.id === appState.activeSpareClientId);
  const body = byId('spareTonersBody');
  const title = byId('spareTonersTitle');
  if (!c || !body) return;

  if (title) title.innerText = `Estoque Reserva no Cliente • ${c.name}`;
  const sp = normalizeSpareToners(c.spare_toners);

  body.innerHTML = `
    <p class="text-xs text-slate-600">Controle quantos cartuchos ou frascos reserva ficam em estoque físico no local do cliente.</p>
    <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
      ${SPARE_TONER_COLORS.map(
        col => `
        <div class="p-3.5 rounded-2xl border border-slate-200 bg-slate-50 flex items-center justify-between gap-2">
          <div class="flex items-center gap-2">
            <span class="w-3 h-3 rounded-full ${col.dot}"></span>
            <div>
              <strong class="text-xs text-slate-900 block">${col.label}</strong>
              <span class="text-[11px] mono text-slate-500">${sp[col.key]} unidade(s)</span>
            </div>
          </div>
          <div class="flex items-center gap-1.5">
            <button type="button" data-action="spare-delta" data-client-id="${c.id}" data-color="${col.key}" data-delta="-1" class="w-8 h-8 rounded-xl bg-white border border-slate-300 font-black text-slate-700 hover:bg-slate-100">-</button>
            <span class="w-7 text-center font-black mono text-sm">${sp[col.key]}</span>
            <button type="button" data-action="spare-delta" data-client-id="${c.id}" data-color="${col.key}" data-delta="1" class="w-8 h-8 rounded-xl bg-[#0038A8] text-white font-black hover:bg-blue-800">+</button>
          </div>
        </div>`,
      ).join('')}
    </div>`;
}

export function openSpareTonersModal(clientId: string): void {
  appState.activeSpareClientId = clientId;
  renderSpareTonersModalBody();
  openModal('spareTonersModal');
}

export async function adjustClientSpareToner(
  clientId: string,
  colorKey: keyof SpareTonersStock,
  delta: number,
): Promise<void> {
  const c = appState.clients.find(x => x.id === clientId);
  if (!c) return;
  const current = normalizeSpareToners(c.spare_toners);
  const updated: SpareTonersStock = {
    ...current,
    [colorKey]: Math.max(0, current[colorKey] + delta),
  };
  const { error } = await sb.from('clients').update({ spare_toners: updated }).eq('id', clientId);
  if (error) {
    if (!isMissingColumnError(error)) {
      alert('Erro ao atualizar toner reserva: ' + error.message);
      return;
    }
  }
  c.spare_toners = updated;
  if (!byId('spareTonersModal')?.classList.contains('hidden')) {
    renderSpareTonersModalBody();
  }
  clientCallbacks.onDataChanged();
}

export function openClientAdjustmentModal(clientId: string): void {
  const c = appState.clients.find(x => x.id === clientId);
  if (!c) return;
  const setVal = (id: string, val: string): void => {
    const el = byId<HTMLInputElement | HTMLTextAreaElement>(id);
    if (el) el.value = val;
  };
  setVal('adjClientId', c.id);
  setVal('adjReplacedMono', String(Number(c.replaced_delta_mono ?? 0)));
  setVal('adjReplacedColor', String(Number(c.replaced_delta_color ?? 0)));
  setVal('adjTechDedMono', String(Number(c.tech_deduction_mono ?? 0)));
  setVal('adjTechDedColor', String(Number(c.tech_deduction_color ?? 0)));
  setVal('adjPoolGroup', c.billing_pool_group ?? '');
  setVal('adjNote', c.adjustment_note ?? '');

  const titleEl = byId('adjModalClientTitle');
  if (titleEl) titleEl.innerText = `Ajuste Auditado & Pool • ${c.name}`;

  updateClientAdjustmentPreview();
  openModal('clientAdjustmentModal');
}

export function updateClientAdjustmentPreview(): void {
  const cid = byId<HTMLInputElement>('adjClientId')?.value.trim() ?? '';
  const c = appState.clients.find(x => x.id === cid);
  const previewEl = byId('adjPreviewBox');
  if (!c || !previewEl) return;

  const repM = Math.max(0, Number(byId<HTMLInputElement>('adjReplacedMono')?.value ?? 0));
  const repC = Math.max(0, Number(byId<HTMLInputElement>('adjReplacedColor')?.value ?? 0));
  const dedM = Math.max(0, Number(byId<HTMLInputElement>('adjTechDedMono')?.value ?? 0));
  const dedC = Math.max(0, Number(byId<HTMLInputElement>('adjTechDedColor')?.value ?? 0));

  const tempClient: Client = {
    ...c,
    replaced_delta_mono: repM,
    replaced_delta_color: repC,
    tech_deduction_mono: dedM,
    tech_deduction_color: dedC,
  };
  const m = computeClientMetrics(tempClient, appState.printers, appState.telemetryReadings, appState.closings);
  previewEl.innerHTML = `
    <div class="grid grid-cols-2 sm:grid-cols-4 gap-2 text-xs mono">
      <div><span class="text-[10px] text-slate-400 block">Bruto Hardware</span><strong>${formatIntegerBr(m.rawMono)} P&amp;B • ${formatIntegerBr(m.rawColor)} Cor</strong></div>
      <div><span class="text-[10px] text-indigo-600 block">(+) Troca no Mês</span><strong>+${formatIntegerBr(repM)} / +${formatIntegerBr(repC)}</strong></div>
      <div><span class="text-[10px] text-rose-600 block">(-) Abatimento Téc.</span><strong>-${formatIntegerBr(dedM)} / -${formatIntegerBr(dedC)}</strong></div>
      <div><span class="text-[10px] text-[#0038A8] block">Total Faturável</span><strong>${formatCurrencyBrl(m.total)}</strong></div>
    </div>`;
}

export function openClientModalForCreate(): void {
  const editInput = byId<HTMLInputElement>('cliEditId');
  if (editInput) editInput.value = '';
  const form = document.querySelector<HTMLFormElement>('#newClientModal form');
  form?.reset();
  const titleEl = byId('newClientModalTitle');
  if (titleEl) titleEl.innerText = 'Cadastrar Novo Cliente & Contrato';
  const btnEl = byId('cliSubmitBtn');
  if (btnEl) btnEl.innerText = 'Salvar Cliente e Gerar Token do Scanner';
  openModal('newClientModal');
}

export function openClientModalForEdit(clientId: string): void {
  const c = appState.clients.find(x => x.id === clientId);
  if (!c) return;
  const setVal = (id: string, val: string): void => {
    const el = byId<HTMLInputElement>(id);
    if (el) el.value = val;
  };
  setVal('cliEditId', c.id);
  const titleEl = byId('newClientModalTitle');
  if (titleEl) titleEl.innerText = `Editar Cliente • ${c.name}`;
  const btnEl = byId('cliSubmitBtn');
  if (btnEl) btnEl.innerText = 'Salvar Alterações do Cliente';

  setVal('cliName', c.name || '');
  setVal('cliCnpj', c.cnpj || '');
  setVal('cliPhone', c.phone || '');
  setVal('cliClosingDay', String(c.closing_day || 15));
  setVal('cliContract', c.contract_num || '');
  setVal('cliBaseRate', String(Number(c.base_rate || 0)));
  setVal('cliFranMono', String(Number(c.franchise_mono || 0)));
  setVal('cliExtraMono', String(Number(c.extra_mono ?? 0.05)));
  setVal('cliFranColor', String(Number(c.franchise_color || 0)));
  setVal('cliExtraColor', String(Number(c.extra_color ?? 0.1)));
  const corpCheck = byId<HTMLInputElement>('cliIsCorp');
  if (corpCheck) corpCheck.checked = c.is_corporate !== false;
  setVal('cliUnits', Array.isArray(c.units) ? c.units.join(', ') : 'Matriz');
  setVal('cliDepts', Array.isArray(c.departments) ? c.departments.join(', ') : 'Geral');
  setVal('cliBillingPoolGroup', c.billing_pool_group || '');
  setVal('cliTechDeductionMono', String(Number(c.tech_deduction_mono || 0)));
  setVal('cliTechDeductionColor', String(Number(c.tech_deduction_color || 0)));
  setVal('cliReplacedDeltaMono', String(Number(c.replaced_delta_mono || 0)));
  setVal('cliReplacedDeltaColor', String(Number(c.replaced_delta_color || 0)));
  setVal('cliAdjustmentNote', c.adjustment_note || '');
  openModal('newClientModal');
}

export async function deleteClient(clientId: string): Promise<void> {
  const c = appState.clients.find(x => x.id === clientId);
  if (!c) return;
  if (!confirm(`Deseja realmente excluir o cliente "${c.name}"?\n\nAs impressoras ligadas a ele ficarão "Avulsas" para preservar o histórico.`)) {
    return;
  }
  const { error } = await sb.from('clients').delete().eq('id', clientId);
  if (error) {
    alert('Erro ao excluir cliente: ' + error.message);
    return;
  }
  await loadTenantData();
  clientCallbacks.onDataChanged();
}

export function renderClientsDashboard(): void {
  const kpiClients = byId('kpiClients');
  const kpiPrinters = byId('kpiPrinters');
  const badgeClients = byId('clientsCountBadge');
  if (kpiClients) kpiClients.innerText = String(appState.clients.length);
  if (kpiPrinters) kpiPrinters.innerText = String(appState.printers.length);
  if (badgeClients) badgeClients.innerText = String(appState.clients.length);

  const offlineCount = appState.printers.filter(p => getEffectivePrinterStatus(p).isOffline).length;
  const onlineCount = Math.max(0, appState.printers.length - offlineCount);
  const exemptCount = appState.printers.filter(p => Boolean(p.billing_exempt)).length;
  const kpiSub = byId('kpiPrintersSub');
  if (kpiSub) {
    kpiSub.innerText =
      appState.printers.length > 0
        ? `${onlineCount} online • ${offlineCount} offline${exemptCount > 0 ? ` • ${exemptCount} isenta(s)` : ''}`
        : 'Rede (Wi-Fi/Cabo) + USB';
  }

  let sumPages = 0;
  let sumBillCents = 0;
  let dueClient: Client | null = null;
  let bestDueScore = 999;
  const todayDay = new Date().getDate();
  appState.activeDueClientId = null;

  const tbody = byId('clientsTableBody');
  if (tbody) {
    if (appState.clients.length === 0) {
      tbody.innerHTML = `
        <tr>
          <td colspan="7" class="py-12 text-center text-slate-400 space-y-3">
            <div class="w-12 h-12 rounded-2xl bg-slate-100 text-slate-400 flex items-center justify-center mx-auto">
              <i data-lucide="inbox" class="w-6 h-6"></i>
            </div>
            <div class="font-bold text-slate-800 text-sm">Nenhum cliente cadastrado ainda</div>
            <p class="text-xs text-slate-500 max-w-md mx-auto">
              Cadastre seu primeiro cliente ou unidade para organizar impressoras, franquias e fechamentos mensais.
            </p>
            <div class="pt-2">
              <button type="button" data-action="create-client" class="px-4 py-2 rounded-xl bg-[#0038A8] hover:bg-blue-800 text-white font-bold text-xs shadow-sm transition">
                + Cadastrar 1º Cliente
              </button>
            </div>
          </td>
        </tr>`;
    } else {
      tbody.innerHTML = appState.clients
        .map(c => {
          const m = computeClientMetrics(c, appState.printers, appState.telemetryReadings, appState.closings);
          sumPages += m.dTotal;
          sumBillCents += m.totalCents;
          const score = getDueWindowScore(c.closing_day, todayDay);
          if (!m.isFrozen && score !== null && score < bestDueScore) {
            bestDueScore = score;
            dueClient = c;
          }

          const badge = m.isFrozen
            ? '<span class="px-2.5 py-0.5 rounded-full text-[10px] font-black bg-blue-100 text-[#0038A8]">● CONGELADO</span>'
            : '<span class="px-2.5 py-0.5 rounded-full text-[10px] font-bold bg-amber-50 text-amber-700">● EM ABERTO</span>';
          const wppBadge = isClientWppReady(c.id)
            ? '<span class="ml-1.5 px-1.5 py-0.5 rounded-full text-[9px] font-bold bg-emerald-100 text-emerald-800">WPP Win11</span>'
            : '';
          const poolBadge = m.poolGroup
            ? `<span class="ml-1.5 px-1.5 py-0.5 rounded-full text-[9px] font-bold bg-indigo-100 text-indigo-800">Pool: ${escapeHtml(m.poolGroup)}</span>`
            : '';
          const hasAdj = m.replacedMono > 0 || m.replacedColor > 0 || m.techDedMono > 0 || m.techDedColor > 0;
          const adjHint = hasAdj
            ? `<div class="text-[9px] text-amber-700 font-semibold">${m.replacedMono + m.replacedColor > 0 ? `+${formatIntegerBr(m.replacedMono + m.replacedColor)} troca ` : ''}${m.techDedMono + m.techDedColor > 0 ? `-${formatIntegerBr(m.techDedMono + m.techDedColor)} téc` : ''}</div>`
            : '';

          return `
            <tr class="hover:bg-slate-50 transition">
              <td class="py-3.5 px-4">
                <strong class="text-slate-900 block text-xs">${escapeHtml(c.name)}${wppBadge}${poolBadge}</strong>
                <span class="text-[11px] text-slate-400 mono block">${escapeHtml(c.cnpj ?? '')} • Corte Dia ${c.closing_day}</span>
                <div class="mt-1 flex flex-wrap items-center gap-1">${renderSpareTonersSummaryPills(c)}</div>
              </td>
              <td class="py-3.5 px-4 text-[11px] text-slate-600">
                <span>${escapeHtml((c.units ?? ['Matriz']).join(', '))}</span><br>
                <span class="text-indigo-600 font-semibold">${escapeHtml((c.departments ?? ['Geral']).join(', '))}</span>
              </td>
              <td class="py-3.5 px-4 text-center font-bold mono">${m.linked.length} máq</td>
              <td class="py-3.5 px-4 text-right mono font-bold">
                ${formatIntegerBr(m.dTotal)} págs
                <div class="text-[10px] text-slate-400">${formatIntegerBr(m.dMono)} P&amp;B • ${formatIntegerBr(m.dColor)} Cor</div>
                ${adjHint}
              </td>
              <td class="py-3.5 px-4 text-right mono font-black text-slate-900 text-sm whitespace-nowrap">
                ${formatCurrencyBrl(m.total)}
              </td>
              <td class="py-3.5 px-4 text-center whitespace-nowrap">${badge}</td>
              <td class="py-3.5 px-4 text-right whitespace-nowrap">
                <div class="inline-flex items-center gap-1">
                  <button type="button" data-action="edit-client" data-client-id="${c.id}" class="px-2 py-1.5 rounded-xl bg-slate-100 hover:bg-slate-200 text-slate-700 font-bold text-[11px] transition">Editar</button>
                  <button type="button" data-action="open-spare" data-client-id="${c.id}" class="px-2 py-1.5 rounded-xl bg-slate-100 hover:bg-slate-200 text-slate-800 font-bold text-[11px] transition">Reserva</button>
                  <button type="button" data-action="open-adj" data-client-id="${c.id}" class="px-2 py-1.5 rounded-xl bg-amber-50 hover:bg-amber-100 text-amber-900 border border-amber-200 font-bold text-[11px] transition">Ajuste</button>
                  <button type="button" data-action="client-portal" data-client-id="${c.id}" class="px-2 py-1.5 rounded-xl bg-indigo-50 hover:bg-indigo-100 text-indigo-900 border border-indigo-200 font-bold text-[11px] transition" title="Portal Completo do Cliente (full_client)">Portal</button>
                  <button type="button" data-action="open-invoice" data-client-id="${c.id}" class="px-2.5 py-1.5 rounded-xl bg-[#0038A8] text-white font-bold text-[11px] hover:bg-blue-800 transition shadow-sm">Espelho A4</button>
                  <button type="button" data-action="quick-wpp" data-client-id="${c.id}" class="p-1.5 rounded-xl bg-emerald-50 text-emerald-700 hover:bg-emerald-100 transition" title="Enviar WhatsApp"><i data-lucide="message-circle" class="w-4 h-4"></i></button>
                  <button type="button" data-action="delete-client" data-client-id="${c.id}" class="p-1.5 rounded-xl bg-rose-50 text-rose-700 hover:bg-rose-100 transition" title="Excluir Cliente"><i data-lucide="trash-2" class="w-4 h-4"></i></button>
                </div>
              </td>
            </tr>`;
        })
        .join('');
    }
  }

  const kpiPages = byId('kpiPagesMonth');
  if (kpiPages) kpiPages.innerText = `${formatIntegerBr(sumPages)} págs`;
  const kpiBilling = byId('kpiBillingMonth');
  if (kpiBilling) kpiBilling.innerText = formatCurrencyBrl(centsToBrl(sumBillCents));

  const dueCard = byId('dueAlertCard');
  if (dueCard) {
    if (dueClient !== null) {
      const targetDue: Client = dueClient;
      appState.activeDueClientId = targetDue.id;
      dueCard.classList.remove('hidden');
      const m = computeClientMetrics(targetDue, appState.printers, appState.telemetryReadings, appState.closings);
      const exactMatch = Number(targetDue.closing_day) === todayDay;
      const dueTitle = byId('dueAlertTitle');
      const dueDesc = byId('dueAlertDesc');
      if (dueTitle) {
        dueTitle.innerText = exactMatch
          ? `● FECHAMENTO DE HOJE • CORTE DIA ${targetDue.closing_day}`
          : `● CORTE PRÓXIMO • DIA ${targetDue.closing_day}`;
      }
      if (dueDesc) {
        dueDesc.innerText = `${targetDue.name}: ${formatIntegerBr(m.dTotal)} págs • ${formatCurrencyBrl(m.total)}.`;
      }
    } else {
      dueCard.classList.add('hidden');
    }
  }

  checkTrialPrinterLock();
  refreshIcons();
}

async function renderClientPortalModal(clientId: string, rotate: boolean, applyPin = false): Promise<void> {
  const c = appState.clients.find(x => x.id === clientId);
  const body = byId('clientPortalModalBody');
  const titleEl = byId('clientPortalModalTitle');
  if (!c || !body) return;
  if (titleEl) titleEl.innerText = `Portal Completo • ${c.name}`;

  const pinInput = byId<HTMLInputElement>('clientPortalPinInput');
  const rawPin = (pinInput?.value ?? '').replace(/[^A-Za-z0-9]/g, '');
  if (applyPin && rawPin.length < 4) {
    alert('O PIN do portal deve conter ao menos 4 caracteres alfanuméricos.');
    return;
  }

  body.innerHTML = `<div class="py-6 text-center text-xs text-slate-500">Gerando / recuperando link permanente do Portal do Cliente...</div>`;
  const res = await getOrCreatePortalToken({
    clientId: c.id,
    scope: 'full_client',
    pin: applyPin ? rawPin : undefined,
    rotate,
    label: `Portal Completo — ${c.name}`,
  });

  if (res.status !== 'success' || !res.token_raw) {
    body.innerHTML = `<div class="p-4 rounded-2xl bg-rose-50 text-rose-700 text-xs font-bold">${escapeHtml(res.message ?? 'Erro ao gerar Portal do Cliente.')}</div>`;
    return;
  }

  const portalUrl = `${window.location.origin}${window.location.pathname}#portal=${res.token_raw}`;
  const qrDataUrl = await renderQrDataUrl(portalUrl, 200);
  const pinHint = applyPin
    ? `PIN atualizado: ${escapeHtml(rawPin)}`
    : res.reused
      ? 'PIN atual mantido (padrão inicial: 1234 ou o PIN já personalizado)'
      : 'PIN inicial do portal: 1234';

  body.innerHTML = `
    <div class="p-4 rounded-3xl border border-indigo-200 bg-indigo-50/40 text-center space-y-2.5">
      <div class="text-[11px] font-black uppercase tracking-wider text-indigo-800">Escopo Completo do Cliente (full_client)</div>
      <div class="text-sm font-black text-slate-900">${escapeHtml(c.name)}</div>
      <img src="${qrDataUrl}" alt="QR Code Portal do Cliente" class="w-40 h-40 mx-auto rounded-xl border border-slate-200 p-1 bg-white" />
      <input id="clientPortalUrlInput" readonly value="${escapeHtml(portalUrl)}" class="w-full px-3 py-2 rounded-xl border border-slate-300 bg-white mono text-[11px] text-slate-700" />
      <div class="inline-block px-3 py-1 rounded-full bg-white border border-slate-200 text-slate-800 mono text-[11px] font-black">${pinHint}</div>
      <div class="text-[10px] text-slate-500">${res.reused ? 'Token permanente reutilizado — links enviados anteriormente continuam ativos.' : 'Novo token de cliente emitido com segurança.'}</div>
    </div>
    <div class="flex flex-wrap gap-2 text-xs">
      <button type="button" id="btnOpenClientPortalUrl" class="flex-1 py-2.5 rounded-xl bg-[#0038A8] hover:bg-blue-800 text-white font-black">Abrir Portal</button>
      <button type="button" id="btnCopyClientPortalUrl" class="flex-1 py-2.5 rounded-xl bg-slate-100 hover:bg-slate-200 text-slate-800 font-bold">Copiar Link</button>
      <button type="button" id="btnRotateClientPortalToken" class="px-3 py-2.5 rounded-xl bg-amber-50 hover:bg-amber-100 text-amber-900 border border-amber-200 font-bold">Rotacionar Token</button>
    </div>`;

  byId('btnOpenClientPortalUrl')?.addEventListener('click', () => {
    window.open(portalUrl, '_blank', 'noopener');
  });
  byId('btnCopyClientPortalUrl')?.addEventListener('click', () => {
    void navigator.clipboard?.writeText(portalUrl);
    alert('Link do Portal do Cliente copiado!');
  });
  byId('btnRotateClientPortalToken')?.addEventListener('click', () => {
    if (confirm('Rotacionar invalida o link anterior do Portal Completo deste cliente (adesivos individuais das máquinas não são afetados). Continuar?')) {
      void renderClientPortalModal(clientId, true, applyPin);
    }
  });
}

export function openClientPortalModal(clientId: string): void {
  activePortalClientId = clientId;
  const pinInput = byId<HTMLInputElement>('clientPortalPinInput');
  if (pinInput) pinInput.value = '1234';
  openModal('clientPortalModal');
  void renderClientPortalModal(clientId, false, false);
}

export function initClientsView(callbacks: ClientUiCallbacks): void {
  clientCallbacks = callbacks;

  byId('btnNewClient')?.addEventListener('click', () => openClientModalForCreate());
  byId('btnRegenerateClientPortalPin')?.addEventListener('click', () => {
    if (activePortalClientId) void renderClientPortalModal(activePortalClientId, false, true);
  });
  byId('btnDueQuickInvoice')?.addEventListener('click', () => {
    const tid = appState.activeDueClientId ?? appState.clients[0]?.id;
    if (tid) clientCallbacks.onOpenInvoice(tid);
  });

  byId('clientsTableBody')?.addEventListener('click', e => {
    const btn = (e.target as HTMLElement).closest<HTMLElement>('[data-action]');
    if (!btn) return;
    const action = btn.dataset.action;
    const cid = btn.dataset.clientId ?? '';
    if (action === 'create-client') openClientModalForCreate();
    else if (action === 'edit-client' && cid) openClientModalForEdit(cid);
    else if (action === 'open-spare' && cid) openSpareTonersModal(cid);
    else if (action === 'open-adj' && cid) openClientAdjustmentModal(cid);
    else if (action === 'client-portal' && cid) openClientPortalModal(cid);
    else if (action === 'open-invoice' && cid) clientCallbacks.onOpenInvoice(cid);
    else if (action === 'quick-wpp' && cid) clientCallbacks.onSendQuickWhatsApp(cid);
    else if (action === 'delete-client' && cid) void deleteClient(cid);
  });

  byId('spareTonersBody')?.addEventListener('click', e => {
    const btn = (e.target as HTMLElement).closest<HTMLElement>('[data-action="spare-delta"]');
    if (!btn) return;
    const cid = btn.dataset.clientId ?? '';
    const color = (btn.dataset.color ?? 'K') as keyof SpareTonersStock;
    const delta = Number(btn.dataset.delta ?? 0);
    if (cid && delta) void adjustClientSpareToner(cid, color, delta);
  });

  ['adjReplacedMono', 'adjReplacedColor', 'adjTechDedMono', 'adjTechDedColor'].forEach(id => {
    byId(id)?.addEventListener('input', () => updateClientAdjustmentPreview());
  });

  byId<HTMLFormElement>('formClientAdjustment')?.addEventListener('submit', e => {
    e.preventDefault();
    void (async () => {
      const cid = byId<HTMLInputElement>('adjClientId')?.value.trim() ?? '';
      if (!cid) return;
      const replaced_delta_mono = Math.max(0, Number(byId<HTMLInputElement>('adjReplacedMono')?.value ?? 0));
      const replaced_delta_color = Math.max(0, Number(byId<HTMLInputElement>('adjReplacedColor')?.value ?? 0));
      const tech_deduction_mono = Math.max(0, Number(byId<HTMLInputElement>('adjTechDedMono')?.value ?? 0));
      const tech_deduction_color = Math.max(0, Number(byId<HTMLInputElement>('adjTechDedColor')?.value ?? 0));
      const billing_pool_group = (byId<HTMLInputElement>('adjPoolGroup')?.value ?? '').trim() || null;
      const adjustment_note = (byId<HTMLTextAreaElement>('adjNote')?.value ?? '').trim() || null;

      const { error } = await sb
        .from('clients')
        .update({
          replaced_delta_mono,
          replaced_delta_color,
          tech_deduction_mono,
          tech_deduction_color,
          billing_pool_group,
          adjustment_note,
        })
        .eq('id', cid);

      if (error) {
        alert('Erro ao salvar ajuste auditado: ' + error.message);
        return;
      }
      closeModal('clientAdjustmentModal');
      await loadTenantData();
      clientCallbacks.onDataChanged();
    })();
  });

  byId<HTMLFormElement>('formNewClient')?.addEventListener('submit', e => {
    e.preventDefault();
    void (async () => {
      if (!appState.profile) return;
      const editId = byId<HTMLInputElement>('cliEditId')?.value.trim() ?? '';
      const name = byId<HTMLInputElement>('cliName')?.value.trim() ?? '';
      const cnpj = byId<HTMLInputElement>('cliCnpj')?.value.trim() ?? '';
      const phone = byId<HTMLInputElement>('cliPhone')?.value.trim() ?? '';
      const closing_day = Number(byId<HTMLInputElement>('cliClosingDay')?.value || 15);
      const contract_num = byId<HTMLInputElement>('cliContract')?.value.trim() ?? '';
      const base_rate = Number(byId<HTMLInputElement>('cliBaseRate')?.value || 0);
      const franchise_mono = Number(byId<HTMLInputElement>('cliFranMono')?.value || 0);
      const extra_mono = Number(byId<HTMLInputElement>('cliExtraMono')?.value || 0.05);
      const franchise_color = Number(byId<HTMLInputElement>('cliFranColor')?.value || 0);
      const extra_color = Number(byId<HTMLInputElement>('cliExtraColor')?.value || 0.1);
      const is_corporate = byId<HTMLInputElement>('cliIsCorp')?.checked ?? true;
      const unitNames = (byId<HTMLInputElement>('cliUnits')?.value ?? '')
        .split(',')
        .map(s => s.trim())
        .filter(Boolean);
      const deptNames = (byId<HTMLInputElement>('cliDepts')?.value ?? '')
        .split(',')
        .map(s => s.trim())
        .filter(Boolean);

      const billing_pool_group = (byId<HTMLInputElement>('cliBillingPoolGroup')?.value ?? '').trim() || null;
      const tech_deduction_mono = Math.max(0, Number(byId<HTMLInputElement>('cliTechDeductionMono')?.value || 0));
      const tech_deduction_color = Math.max(0, Number(byId<HTMLInputElement>('cliTechDeductionColor')?.value || 0));
      const replaced_delta_mono = Math.max(0, Number(byId<HTMLInputElement>('cliReplacedDeltaMono')?.value || 0));
      const replaced_delta_color = Math.max(0, Number(byId<HTMLInputElement>('cliReplacedDeltaColor')?.value || 0));
      const adjustment_note = (byId<HTMLInputElement>('cliAdjustmentNote')?.value ?? '').trim() || null;

      const payload = {
        tenant_id: appState.profile.tenant_id,
        name,
        cnpj,
        phone,
        closing_day,
        contract_num,
        base_rate,
        franchise_mono,
        extra_mono,
        franchise_color,
        extra_color,
        is_corporate,
        units: unitNames.length > 0 ? unitNames : ['Matriz'],
        departments: deptNames.length > 0 ? deptNames : ['Geral'],
        billing_pool_group,
        tech_deduction_mono,
        tech_deduction_color,
        replaced_delta_mono,
        replaced_delta_color,
        adjustment_note,
      };

      if (editId) {
        const { error } = await sb.from('clients').update(payload).eq('id', editId);
        if (error) {
          alert('Erro ao atualizar cliente: ' + error.message);
          return;
        }
        if (byId<HTMLInputElement>('cliEditId')) byId<HTMLInputElement>('cliEditId')!.value = '';
        (e.target as HTMLFormElement).reset();
        closeModal('newClientModal');
        await loadTenantData();
        clientCallbacks.onDataChanged();
        return;
      }

      const { data: newCli, error } = await sb.from('clients').insert(payload).select().single();
      if (error || !newCli) {
        alert('Erro ao salvar cliente: ' + (error?.message ?? 'erro desconhecido'));
        return;
      }

      const unitsToCreate = (unitNames.length > 0 ? unitNames : ['Matriz']).map(uName => ({
        tenant_id: appState.profile!.tenant_id,
        client_id: (newCli as Client).id,
        unit_name: uName,
        scan_subnets: ['192.168.1.0/24'],
      }));
      const { error: unitInsertErr } = await sb.from('client_units').insert(unitsToCreate);
      if (unitInsertErr) {
        console.warn('Falha ao criar unidades iniciais do cliente:', unitInsertErr.message);
      }

      (e.target as HTMLFormElement).reset();
      closeModal('newClientModal');
      await loadTenantData();
      clientCallbacks.onDataChanged();
    })();
  });
}
