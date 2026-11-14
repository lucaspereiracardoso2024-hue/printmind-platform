import { getEffectivePrinterStatus, renderSupplyListHtml } from '../domain/supplyPresenter';
import { createPortalTicketByToken, getPortalDataByToken } from '../services/portalService';
import { byId } from '../state/store';
import { PortalPublicData, PortalPublicPrinter, TicketCategory } from '../types/models';
import { escapeHtml } from '../utils/format';

function ensureNoIndexMeta(): void {
  if (!document.querySelector('meta[name="robots"]')) {
    const meta = document.createElement('meta');
    meta.name = 'robots';
    meta.content = 'noindex,nofollow';
    document.head.appendChild(meta);
  }
  if (!document.querySelector('meta[name="referrer"]')) {
    const ref = document.createElement('meta');
    ref.name = 'referrer';
    ref.content = 'no-referrer';
    document.head.appendChild(ref);
  }
}

function renderRecentTicketsHtml(tickets: NonNullable<PortalPublicData['recent_tickets']>): string {
  if (tickets.length === 0) {
    return `<div class="text-xs text-slate-400">Nenhum chamado recente registrado neste portal.</div>`;
  }
  return tickets
    .map(t => {
      const equipLabel = t.printer_model
        ? `<span class="text-slate-500">• ${escapeHtml(t.printer_model)} (${escapeHtml(t.printer_serial ?? '')})</span>`
        : '';
      return `<div class="p-3 rounded-2xl bg-slate-50 border border-slate-200 flex justify-between items-start gap-2">
        <div>
          <strong class="mono text-[#0038A8]">${escapeHtml(t.protocol_code)}</strong> • <span class="uppercase font-bold">${escapeHtml(t.category)}</span> ${equipLabel}
          <p class="text-slate-600 mt-0.5">${escapeHtml(t.description)}</p>
          ${
            t.resolution_note
              ? `<p class="text-emerald-800 bg-emerald-50 border border-emerald-200 rounded-lg px-2 py-1 mt-1 text-[11px] font-semibold">Nota técnica: ${escapeHtml(t.resolution_note)}</p>`
              : ''
          }
        </div>
        <span class="px-2 py-0.5 rounded-full bg-white border border-slate-200 text-[10px] font-black uppercase shrink-0">${escapeHtml(t.status)}</span>
      </div>`;
    })
    .join('');
}

export async function mountPublicPortal(token: string): Promise<void> {
  ensureNoIndexMeta();
  byId('authScreen')?.classList.add('hidden');
  byId('appScreen')?.classList.add('hidden');
  const root = byId('portalScreen');
  if (!root) return;
  root.classList.remove('hidden');

  root.innerHTML = `<div class="max-w-xl mx-auto p-6 mt-10 bg-white rounded-3xl border border-slate-200 shadow-sm text-center text-xs text-slate-500">Validando acesso ao Portal do Tomador...</div>`;

  const res = await getPortalDataByToken(token);
  const scope = res.token_scope ?? (res.client && !res.printer ? 'full_client' : 'printer_ticket_only');
  const printersList: PortalPublicPrinter[] =
    res.printers && res.printers.length > 0
      ? res.printers
      : res.printer
        ? [res.printer]
        : [];

  if (
    res.status !== 'success' ||
    (scope === 'printer_ticket_only' && printersList.length === 0) ||
    (scope === 'full_client' && !res.client)
  ) {
    root.innerHTML = `
      <div class="max-w-xl mx-auto p-6 mt-10 bg-white rounded-3xl border border-rose-200 shadow-sm text-center space-y-3">
        <div class="text-lg font-black text-rose-700">Acesso Inválido ou Revogado</div>
        <p class="text-xs text-slate-600">${escapeHtml(res.message ?? 'Solicite um novo link ou adesivo QR à sua locadora.')}</p>
      </div>`;
    return;
  }

  const isFullClient = scope === 'full_client';
  const singlePrinter = printersList[0] ?? null;
  const tickets = res.recent_tickets ?? [];

  const headerHtml = isFullClient
    ? `
      <div class="bg-gradient-to-r from-[#0038A8] to-blue-800 text-white p-6 rounded-3xl shadow-md space-y-1">
        <div class="text-[11px] font-bold uppercase tracking-wider text-blue-200">Portal do Cliente • ${escapeHtml(res.provider_name ?? 'Assistência')}</div>
        <h1 class="text-xl font-black">${escapeHtml(res.client?.name ?? 'Cliente')}</h1>
        <div class="text-xs text-blue-100 mono">${printersList.length} equipamento(s) monitorado(s)${res.provider_phone ? ` • Suporte: ${escapeHtml(res.provider_phone)}` : ''}</div>
      </div>`
    : `
      <div class="bg-gradient-to-r from-[#0038A8] to-blue-800 text-white p-6 rounded-3xl shadow-md space-y-1">
        <div class="text-[11px] font-bold uppercase tracking-wider text-blue-200">Portal do Equipamento • ${escapeHtml(res.provider_name ?? 'Assistência')}</div>
        <h1 class="text-xl font-black">${escapeHtml(singlePrinter?.model ?? 'Equipamento')}</h1>
        <div class="text-xs text-blue-100 mono">Série: ${escapeHtml(singlePrinter?.serial ?? '')} • ${escapeHtml(singlePrinter?.unit ?? 'Matriz')} (${escapeHtml(singlePrinter?.department ?? 'Geral')})</div>
      </div>`;

  const equipmentSectionHtml = isFullClient
    ? `
      <div class="bg-white p-5 rounded-3xl border border-slate-200 shadow-sm space-y-3">
        <strong class="text-xs uppercase font-black text-slate-700 block">Equipamentos e Suprimentos do Parque</strong>
        ${
          printersList.length === 0
            ? `<div class="p-4 rounded-2xl bg-slate-50 border border-slate-200 text-xs text-slate-500 text-center">Nenhum equipamento vinculado até o momento.</div>`
            : `<div class="grid grid-cols-1 gap-3">
                ${printersList
                  .map(prt => {
                    const st = getEffectivePrinterStatus({
                      id: prt.id,
                      tenant_id: '',
                      model: prt.model,
                      serial: prt.serial,
                      status: prt.status,
                      last_seen: prt.last_seen,
                    });
                    return `
                      <div class="p-4 rounded-2xl bg-slate-50 border border-slate-200 space-y-2.5">
                        <div class="flex flex-wrap items-center justify-between gap-2">
                          <div>
                            <strong class="text-sm font-black text-slate-900">${escapeHtml(prt.model)}</strong>
                            <span class="block text-[11px] text-slate-500 mono">Série: ${escapeHtml(prt.serial)} • ${escapeHtml(prt.unit ?? 'Matriz')} (${escapeHtml(prt.department ?? 'Geral')})</span>
                          </div>
                          <div class="flex items-center gap-2">
                            <span class="px-2 py-0.5 rounded-full text-[10px] font-bold ${st.badgeClass}">${escapeHtml(st.label)}</span>
                            <button type="button" data-select-portal-printer="${escapeHtml(prt.id)}" class="px-2.5 py-1 rounded-xl bg-blue-100 hover:bg-blue-200 text-[#0038A8] font-bold text-[11px]">Abrir Chamado</button>
                          </div>
                        </div>
                        ${renderSupplyListHtml(prt.supplies, false)}
                      </div>`;
                  })
                  .join('')}
              </div>`
        }
      </div>`
    : `
      <div class="bg-white p-5 rounded-3xl border border-slate-200 shadow-sm space-y-3">
        <strong class="text-xs uppercase font-black text-slate-700 block">Status Atual dos Suprimentos</strong>
        ${renderSupplyListHtml(singlePrinter?.supplies ?? [], false)}
      </div>`;

  const printerSelectorHtml = isFullClient
    ? `
      <div>
        <label class="font-bold text-slate-600 block mb-1">Equipamento *</label>
        <select id="portalPrinterSelect" ${printersList.length > 0 ? 'required' : ''} class="w-full px-3 py-2.5 rounded-xl border border-slate-300 bg-white">
          ${
            printersList.length === 0
              ? `<option value="">Chamado Geral (sem equipamento vinculado)</option>`
              : printersList
                  .map(
                    prt =>
                      `<option value="${escapeHtml(prt.id)}">${escapeHtml(prt.model)} (${escapeHtml(prt.serial)}) — ${escapeHtml(prt.unit ?? 'Matriz')} • ${escapeHtml(prt.department ?? 'Geral')}</option>`,
                  )
                  .join('')
          }
        </select>
      </div>`
    : '';

  root.innerHTML = `
    <div class="max-w-3xl mx-auto px-4 py-8 space-y-5">
      ${headerHtml}
      ${equipmentSectionHtml}

      <div id="portalTicketFormCard" class="bg-white p-6 rounded-3xl border border-slate-200 shadow-sm space-y-4">
        <strong class="text-sm font-black text-slate-900 block">Abrir Chamado Técnico ou Pedir Toner</strong>
        <form id="formPublicPortalTicket" class="space-y-3 text-xs">
          ${printerSelectorHtml}
          <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
            <div>
              <label class="font-bold text-slate-600 block mb-1">Seu Nome *</label>
              <input id="portalReqName" required type="text" placeholder="Ex: Maria Souza" class="w-full px-3 py-2.5 rounded-xl border border-slate-300" />
            </div>
            <div>
              <label class="font-bold text-slate-600 block mb-1">WhatsApp / Ramal</label>
              <input id="portalReqPhone" type="text" placeholder="(11) 99999-9999" class="w-full px-3 py-2.5 rounded-xl border border-slate-300" />
            </div>
          </div>
          <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
            <div>
              <label class="font-bold text-slate-600 block mb-1">Tipo de Solicitação</label>
              <select id="portalCategory" class="w-full px-3 py-2.5 rounded-xl border border-slate-300 bg-white">
                <option value="toner">Pedido de Toner / Tinta</option>
                <option value="atolagem">Atolamento de Papel</option>
                <option value="qualidade">Defeito na Qualidade</option>
                <option value="offline">Não Imprime / Offline</option>
                <option value="chamado_tecnico">Suporte Técnico Geral</option>
              </select>
            </div>
            <div>
              <label class="font-bold text-slate-600 block mb-1">${isFullClient ? 'PIN do Portal *' : 'PIN da Etiqueta *'}</label>
              <input id="portalPinInput" required type="password" inputmode="numeric" placeholder="Ex: 1234" class="w-full px-3 py-2.5 rounded-xl border border-slate-300 mono" />
            </div>
          </div>
          <div>
            <label class="font-bold text-slate-600 block mb-1">Descrição *</label>
            <textarea id="portalDescription" required rows="3" placeholder="Descreva o que aconteceu ou qual cor de toner precisa..." class="w-full px-3 py-2.5 rounded-xl border border-slate-300"></textarea>
          </div>
          <div id="portalTicketFeedback" class="hidden p-3 rounded-xl text-xs font-bold"></div>
          <button type="submit" id="btnSubmitPortalTicket" class="w-full py-3 rounded-xl bg-[#0038A8] hover:bg-blue-800 text-white font-black transition">Enviar Solicitação</button>
        </form>
      </div>

      <div class="bg-white p-5 rounded-3xl border border-slate-200 shadow-sm space-y-2.5">
        <strong class="text-xs uppercase font-black text-slate-700 block">${isFullClient ? 'Últimos Chamados do Cliente' : 'Últimos Chamados deste Equipamento'}</strong>
        <div id="portalRecentTicketsList" class="space-y-2 text-xs">
          ${renderRecentTicketsHtml(tickets)}
        </div>
      </div>
    </div>`;

  root.querySelectorAll<HTMLElement>('[data-select-portal-printer]').forEach(btn => {
    btn.addEventListener('click', () => {
      const pid = btn.dataset.selectPortalPrinter ?? '';
      const sel = byId<HTMLSelectElement>('portalPrinterSelect');
      if (sel && pid) {
        sel.value = pid;
      }
      byId('portalTicketFormCard')?.scrollIntoView({ behavior: 'smooth', block: 'center' });
      byId<HTMLInputElement>('portalReqName')?.focus();
    });
  });

  byId<HTMLFormElement>('formPublicPortalTicket')?.addEventListener('submit', e => {
    e.preventDefault();
    void (async () => {
      const btn = byId<HTMLButtonElement>('btnSubmitPortalTicket');
      const fb = byId('portalTicketFeedback');
      if (btn) btn.disabled = true;

      const selectedPrinterId = isFullClient
        ? byId<HTMLSelectElement>('portalPrinterSelect')?.value || null
        : singlePrinter?.id ?? null;
      const requesterName = byId<HTMLInputElement>('portalReqName')?.value.trim() ?? '';
      const requesterPhone = byId<HTMLInputElement>('portalReqPhone')?.value.trim() ?? '';
      const category = (byId<HTMLSelectElement>('portalCategory')?.value ?? 'chamado_tecnico') as TicketCategory;
      const pin = byId<HTMLInputElement>('portalPinInput')?.value.trim() ?? '';
      const description = byId<HTMLTextAreaElement>('portalDescription')?.value.trim() ?? '';

      const ticketRes = await createPortalTicketByToken({
        token,
        pin,
        requesterName,
        requesterPhone,
        category,
        description,
        printerId: selectedPrinterId,
      });

      if (btn) btn.disabled = false;
      if (!fb) return;

      fb.classList.remove('hidden', 'bg-emerald-50', 'text-emerald-800', 'bg-rose-50', 'text-rose-800', 'bg-amber-50', 'text-amber-900');

      if (ticketRes.status === 'success' || ticketRes.status === 'duplicate') {
        fb.classList.add('bg-emerald-50', 'text-emerald-800');
        fb.innerText =
          ticketRes.status === 'duplicate'
            ? `Já existe chamado recente aberto para esta ocorrência (${ticketRes.protocol_code}). A equipe já foi notificada.`
            : `✓ Chamado registrado! Protocolo: ${ticketRes.protocol_code}`;
        byId<HTMLFormElement>('formPublicPortalTicket')?.reset();
        const refreshed = await getPortalDataByToken(token);
        const ticketsListEl = byId('portalRecentTicketsList');
        if (ticketsListEl && refreshed.status === 'success' && refreshed.recent_tickets) {
          ticketsListEl.innerHTML = renderRecentTicketsHtml(refreshed.recent_tickets);
        }
      } else if (ticketRes.status === 'invalid_pin') {
        fb.classList.add('bg-amber-50', 'text-amber-900');
        fb.innerText = ticketRes.message ?? 'PIN incorreto.';
      } else {
        fb.classList.add('bg-rose-50', 'text-rose-800');
        fb.innerText = ticketRes.message ?? 'Não foi possível enviar sua solicitação.';
      }
    })();
  });
}
