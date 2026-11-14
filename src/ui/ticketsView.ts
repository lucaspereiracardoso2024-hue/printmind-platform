import { updateServiceTicketStatus } from '../services/portalService';
import { appState, byId, loadTenantData } from '../state/store';
import { TicketStatus } from '../types/models';
import { escapeHtml } from '../utils/format';

export function renderTicketsView(): void {
  const tbody = byId('ticketsTableBody');
  const badge = byId('ticketsOpenBadge');
  if (!tbody) return;

  const openCount = appState.serviceTickets.filter(
    t => t.status === 'aberto' || t.status === 'em_atendimento',
  ).length;
  if (badge) {
    badge.innerText = String(openCount);
    badge.classList.toggle('hidden', openCount === 0);
  }

  const filter = byId<HTMLSelectElement>('ticketsStatusFilter')?.value ?? 'all';
  const filtered = appState.serviceTickets.filter(t => filter === 'all' || t.status === filter);

  if (filtered.length === 0) {
    tbody.innerHTML = `<tr><td colspan="6" class="py-8 text-center text-slate-400">Nenhum chamado encontrado. Gere etiquetas QR nas impressoras para os clientes solicitarem suporte ou toner com 1 clique.</td></tr>`;
    return;
  }

  tbody.innerHTML = filtered
    .map(t => {
      const prt = appState.printers.find(p => p.id === t.printer_id);
      const cli = appState.clients.find(c => c.id === t.client_id);
      const dt = new Date(t.created_at).toLocaleString('pt-BR');
      const statusBadge =
        t.status === 'resolvido'
          ? 'bg-emerald-100 text-emerald-800'
          : t.status === 'em_atendimento'
            ? 'bg-blue-100 text-[#0038A8]'
            : t.status === 'cancelado'
              ? 'bg-slate-200 text-slate-600'
              : 'bg-amber-100 text-amber-900';

      return `
        <tr class="hover:bg-slate-50 transition">
          <td class="py-3 px-4 mono font-bold text-[#0038A8]">${escapeHtml(t.protocol_code)}<span class="block text-[10px] text-slate-400">${escapeHtml(dt)}</span></td>
          <td class="py-3 px-4">
            <strong class="text-slate-900 block">${escapeHtml(prt ? prt.model : 'Chamado Geral (Cliente)')}</strong>
            <span class="text-[11px] text-slate-500">${escapeHtml(cli ? cli.name : 'Cliente')}${prt?.serial ? ` • ${escapeHtml(prt.serial)}` : ''}</span>
          </td>
          <td class="py-3 px-4">
            <strong class="text-slate-800 block">${escapeHtml(t.requester_name)}</strong>
            <span class="text-[11px] mono text-slate-500">${escapeHtml(t.requester_phone ?? '')}</span>
          </td>
          <td class="py-3 px-4">
            <span class="px-2 py-0.5 rounded-full text-[10px] font-bold bg-slate-100 text-slate-700 uppercase">${escapeHtml(t.category)}</span>
            <p class="text-[11px] text-slate-600 mt-1">${escapeHtml(t.description)}</p>
            ${t.resolution_note ? `<div class="mt-1 px-2 py-1 rounded-lg bg-emerald-50 border border-emerald-200 text-[10px] text-emerald-900 font-semibold">Nota técnica: ${escapeHtml(t.resolution_note)}</div>` : ''}
          </td>
          <td class="py-3 px-4 text-center">
            <span class="px-2.5 py-0.5 rounded-full text-[10px] font-black uppercase ${statusBadge}">${escapeHtml(t.status)}</span>
          </td>
          <td class="py-3 px-4 text-right whitespace-nowrap space-x-1">
            <button type="button" data-action="ticket-status" data-ticket-id="${t.id}" data-status="em_atendimento" class="px-2 py-1 rounded-xl bg-blue-50 text-[#0038A8] font-bold text-[11px]">Atender</button>
            <button type="button" data-action="ticket-status" data-ticket-id="${t.id}" data-status="resolvido" class="px-2 py-1 rounded-xl bg-emerald-50 text-emerald-800 font-bold text-[11px]">Resolver</button>
          </td>
        </tr>`;
    })
    .join('');
}

export function initTicketsView(): void {
  byId('ticketsStatusFilter')?.addEventListener('change', () => renderTicketsView());
  byId('ticketsTableBody')?.addEventListener('click', e => {
    const btn = (e.target as HTMLElement).closest<HTMLElement>('[data-action="ticket-status"]');
    if (!btn) return;
    const tid = btn.dataset.ticketId ?? '';
    const status = (btn.dataset.status ?? 'em_atendimento') as TicketStatus;
    if (!tid) return;
    void (async () => {
      const res = await updateServiceTicketStatus(tid, status);
      if (!res.ok) {
        alert(res.message ?? 'Erro ao atualizar chamado.');
        return;
      }
      await loadTenantData();
      renderTicketsView();
    })();
  });
}
