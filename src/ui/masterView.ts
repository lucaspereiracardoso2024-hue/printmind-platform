import { MASTER_ADMIN_EMAIL, sb } from '../services/supabaseClient';
import { appState, byId, isMasterAdminUser, loadPlatformSettings, openModal } from '../state/store';
import { Printer, Tenant } from '../types/models';
import { escapeHtml, formatCurrencyBrl, normalizeDigits } from '../utils/format';
import { refreshIcons } from '../utils/icons';
import { getBillablePrintersSummary, normalizePlanTier, PLAN_CATALOG } from '../domain/billingEngine';

interface TenantRiskAlert {
  severity: 'high' | 'medium' | 'low';
  code: string;
  label: string;
  detail: string;
}

type MasterSubTab = 'tenants' | 'billing' | 'fraud' | 'config';

let tenantRiskMap: Record<string, TenantRiskAlert[]> = {};

function analyzeTenantRisks(tenantsList: Tenant[], printersList: Printer[]): Record<string, TenantRiskAlert[]> {
  const cnpjCounts: Record<string, number> = {};
  const phoneCounts: Record<string, number> = {};

  tenantsList.forEach(t => {
    const doc = normalizeDigits(t.cnpj_cpf);
    const tel = normalizeDigits(t.phone);
    if (doc.length >= 11) cnpjCounts[doc] = (cnpjCounts[doc] ?? 0) + 1;
    if (tel.length >= 8) phoneCounts[tel] = (phoneCounts[tel] ?? 0) + 1;
  });

  const risks: Record<string, TenantRiskAlert[]> = {};
  tenantsList.forEach(t => {
    const alerts: TenantRiskAlert[] = [];
    const tPrinters = printersList.filter(p => p.tenant_id === t.id);
    const { billable } = getBillablePrintersSummary(tPrinters);
    const doc = normalizeDigits(t.cnpj_cpf);
    const tel = normalizeDigits(t.phone);
    const pt = String(t.plan_tier ?? 'trial').toLowerCase();
    const st = String(t.subscription_status ?? 'trialing').toLowerCase();

    if (doc.length >= 11 && cnpjCounts[doc] > 1) {
      alerts.push({
        severity: 'high',
        code: 'DUP_DOC',
        label: 'CNPJ/CPF duplicado',
        detail: `Mesmo documento cadastrado em ${cnpjCounts[doc]} contas.`,
      });
    }
    if (tel.length >= 8 && phoneCounts[tel] > 1) {
      alerts.push({
        severity: 'high',
        code: 'DUP_PHONE',
        label: 'Telefone duplicado',
        detail: `Mesmo telefone cadastrado em ${phoneCounts[tel]} contas.`,
      });
    }
    if ((pt === 'trial' || st === 'trialing') && billable.length > 2) {
      alerts.push({
        severity: 'high',
        code: 'TRIAL_ABUSE',
        label: `Trial excedido (${billable.length} máq)`,
        detail: `Conta gratuita com ${billable.length} máquinas faturáveis (limite 2).`,
      });
    }
    if ((st === 'past_due' || st === 'canceled') && billable.length > 0) {
      alerts.push({
        severity: 'medium',
        code: 'BLOCKED_WITH_MACHINES',
        label: `${st === 'past_due' ? 'Inadimplente' : 'Cancelado'} c/ máquinas`,
        detail: `${billable.length} máquina(s) vinculada(s) em conta ${st}.`,
      });
    }
    if (doc.length < 11) {
      alerts.push({
        severity: 'low',
        code: 'NO_DOC',
        label: 'Sem CNPJ/CPF válido',
        detail: 'Cadastro sem documento fiscal para emissão no Asaas.',
      });
    }
    risks[t.id] = alerts;
  });
  return risks;
}

function getTenantRate(planTier?: string | null): number {
  const s = String(planTier ?? '').toLowerCase();
  if (s === 'trial' || s === '') return 0;
  return PLAN_CATALOG[normalizePlanTier(s)].rate;
}

function getStatusBadgeHtml(t: Tenant): string {
  const st = String(t.subscription_status ?? 'trialing').toLowerCase();
  const pt = String(t.plan_tier ?? 'trial').toLowerCase();
  if (t.pioneer_courtesy_active) {
    return '<span class="px-2.5 py-0.5 rounded-full text-[10px] font-black bg-emerald-100 text-emerald-800 border border-emerald-200">★ PIONEIRO</span>';
  }
  if (st === 'past_due') {
    return '<span class="px-2.5 py-0.5 rounded-full text-[10px] font-black bg-rose-100 text-rose-800 border border-rose-200">⛔ INADIMPLENTE</span>';
  }
  if (st === 'canceled') {
    return '<span class="px-2.5 py-0.5 rounded-full text-[10px] font-black bg-slate-200 text-slate-700">✕ CANCELADO</span>';
  }
  if (st === 'pending_payment') {
    return '<span class="px-2.5 py-0.5 rounded-full text-[10px] font-black bg-amber-100 text-amber-800">⏳ PENDENTE</span>';
  }
  if (st === 'trialing' || pt === 'trial') {
    return '<span class="px-2.5 py-0.5 rounded-full text-[10px] font-black bg-amber-50 text-amber-800 border border-amber-200">● TESTE GRÁTIS</span>';
  }
  return '<span class="px-2.5 py-0.5 rounded-full text-[10px] font-black bg-emerald-100 text-emerald-800">● ATIVO</span>';
}

export function switchMasterSubTab(subTab: MasterSubTab): void {
  const map: Record<MasterSubTab, { pane: string; btn: string }> = {
    tenants: { pane: 'masterSubTabTenants', btn: 'masterSubBtnTenants' },
    billing: { pane: 'masterSubTabBilling', btn: 'masterSubBtnBilling' },
    fraud: { pane: 'masterSubTabFraud', btn: 'masterSubBtnFraud' },
    config: { pane: 'masterSubTabConfig', btn: 'masterSubBtnConfig' },
  };
  (Object.keys(map) as MasterSubTab[]).forEach(k => {
    byId(map[k].pane)?.classList.toggle('hidden', k !== subTab);
    const btn = byId(map[k].btn);
    if (btn) {
      btn.className =
        k === subTab
          ? 'px-4 py-2 rounded-xl bg-[#0038A8] text-white shadow-sm transition flex items-center space-x-1.5 text-xs font-bold'
          : 'px-4 py-2 rounded-xl bg-slate-100 text-slate-700 hover:bg-slate-200 transition flex items-center space-x-1.5 text-xs font-bold';
    }
  });
}

export function renderMasterSubscribersTable(): void {
  const tbody = byId('masterTenantsTableBody');
  if (!tbody) return;
  const search = (byId<HTMLInputElement>('masterSearchInput')?.value ?? '').toLowerCase().trim();
  const filter = (byId<HTMLSelectElement>('masterStatusFilter')?.value ?? 'all').toLowerCase();

  const filtered = appState.allPlatformTenants.filter(t => {
    const st = String(t.subscription_status ?? 'trialing').toLowerCase();
    if (filter !== 'all' && st !== filter) return false;
    if (!search) return true;
    const hay = `${t.trade_name} ${t.email} ${t.cnpj_cpf ?? ''} ${t.city ?? ''}`.toLowerCase();
    return hay.includes(search);
  });

  if (filtered.length === 0) {
    tbody.innerHTML = `<tr><td colspan="7" class="py-8 text-center text-slate-400">Nenhum assinante encontrado.</td></tr>`;
    return;
  }

  tbody.innerHTML = filtered
    .map(t => {
      const prts = appState.allPlatformPrinters.filter(p => p.tenant_id === t.id);
      const { billable, exempt } = getBillablePrintersSummary(prts);
      const rate = getTenantRate(t.plan_tier);
      const monthly = billable.length * rate;
      const highAlerts = (tenantRiskMap[t.id] ?? []).filter(a => a.severity === 'high').length;

      return `
        <tr class="hover:bg-slate-50 transition">
          <td class="py-3 px-4">
            <strong class="text-slate-900 block">${escapeHtml(t.trade_name)}</strong>
            <span class="text-[11px] text-slate-500">${escapeHtml(t.email)}</span>
            <span class="block text-[10px] mono text-slate-400">${escapeHtml(t.cnpj_cpf ?? 'Sem doc')} • ${escapeHtml(t.city ?? '-')}</span>
          </td>
          <td class="py-3 px-4 uppercase font-bold text-xs">${escapeHtml(t.plan_tier)}</td>
          <td class="py-3 px-4 text-center mono font-bold">${billable.length}${exempt.length > 0 ? ` <span class="text-[10px] text-amber-700">(+${exempt.length} is.)</span>` : ''}</td>
          <td class="py-3 px-4 text-right mono font-black">${formatCurrencyBrl(monthly)}</td>
          <td class="py-3 px-4 text-center">${getStatusBadgeHtml(t)}</td>
          <td class="py-3 px-4 text-center">
            ${highAlerts > 0 ? `<span class="px-2 py-0.5 rounded-full text-[10px] font-black bg-rose-100 text-rose-800">${highAlerts} alerta(s)</span>` : '<span class="text-xs text-slate-400">Limpo</span>'}
          </td>
          <td class="py-3 px-4 text-right whitespace-nowrap space-x-1">
            <button type="button" data-action="inspect-tenant" data-tenant-id="${t.id}" class="px-2.5 py-1.5 rounded-xl bg-slate-100 text-slate-700 hover:bg-slate-200 font-bold text-[11px]">Inspecionar</button>
            <button type="button" data-action="toggle-suspend" data-tenant-id="${t.id}" data-next="${t.subscription_status === 'past_due' ? 'active' : 'past_due'}" class="px-2.5 py-1.5 rounded-xl ${t.subscription_status === 'past_due' ? 'bg-emerald-50 text-emerald-700' : 'bg-rose-50 text-rose-700'} font-bold text-[11px]">
              ${t.subscription_status === 'past_due' ? 'Reativar' : 'Suspender'}
            </button>
          </td>
        </tr>`;
    })
    .join('');
}

export function renderMasterBillingView(): void {
  let activeMRR = 0;
  let activeCount = 0;
  let riskMRR = 0;
  let riskCount = 0;
  let trialCount = 0;

  const rows = appState.allPlatformTenants
    .map(t => {
      const prts = appState.allPlatformPrinters.filter(p => p.tenant_id === t.id);
      const { billable } = getBillablePrintersSummary(prts);
      const rate = getTenantRate(t.plan_tier);
      const monthly = billable.length * rate;
      const st = String(t.subscription_status ?? 'trialing').toLowerCase();
      const pt = String(t.plan_tier ?? 'trial').toLowerCase();

      if (st === 'past_due' || st === 'canceled') {
        riskCount++;
        riskMRR += monthly;
      } else if (st === 'active' && pt !== 'trial') {
        activeCount++;
        if (!t.pioneer_courtesy_active) activeMRR += monthly;
      } else {
        trialCount++;
      }

      return `
        <tr class="hover:bg-slate-50">
          <td class="py-2.5 px-4 font-bold text-slate-900">${escapeHtml(t.trade_name)}</td>
          <td class="py-2.5 px-4 uppercase text-xs">${escapeHtml(t.plan_tier)}</td>
          <td class="py-2.5 px-4 text-center mono">${billable.length}</td>
          <td class="py-2.5 px-4 text-right mono">${formatCurrencyBrl(rate)}</td>
          <td class="py-2.5 px-4 text-right mono font-black">${t.pioneer_courtesy_active ? 'R$ 0,00 (Pioneiro)' : formatCurrencyBrl(monthly)}</td>
          <td class="py-2.5 px-4 text-center">${getStatusBadgeHtml(t)}</td>
        </tr>`;
    })
    .join('');

  const setText = (id: string, val: string): void => {
    const el = byId(id);
    if (el) el.innerText = val;
  };
  setText('masterBilActiveMRR', formatCurrencyBrl(activeMRR));
  setText('masterBilActiveCount', `${activeCount} assinante(s)`);
  setText('masterBilRiskMRR', formatCurrencyBrl(riskMRR));
  setText('masterBilRiskCount', `${riskCount} conta(s) em risco`);
  setText('masterBilTrialMRR', 'Trial');
  setText('masterBilTrialCount', `${trialCount} conta(s) gratuitas`);

  const tbody = byId('masterBillingTableBody');
  if (tbody) {
    tbody.innerHTML = rows || `<tr><td colspan="6" class="py-8 text-center text-slate-400">Sem assinantes.</td></tr>`;
  }
}

export function renderMasterFraudAlerts(): void {
  const container = byId('masterFraudAlertsList');
  if (!container) return;

  const entries: Array<{ tenant: Tenant; alert: TenantRiskAlert }> = [];
  appState.allPlatformTenants.forEach(t => {
    (tenantRiskMap[t.id] ?? []).forEach(alert => {
      entries.push({ tenant: t, alert });
    });
  });

  if (entries.length === 0) {
    container.innerHTML = `<div class="p-6 text-center text-xs text-slate-400">Nenhum indício antifraude no momento.</div>`;
    return;
  }

  container.innerHTML = entries
    .map(
      ({ tenant, alert }) => `
      <div class="p-4 rounded-2xl border ${alert.severity === 'high' ? 'border-rose-200 bg-rose-50/40' : 'border-amber-200 bg-amber-50/40'} flex flex-wrap items-center justify-between gap-2 text-xs">
        <div>
          <span class="px-2 py-0.5 rounded-full text-[10px] font-black uppercase ${alert.severity === 'high' ? 'bg-rose-100 text-rose-800' : 'bg-amber-100 text-amber-900'}">${escapeHtml(alert.label)}</span>
          <strong class="ml-2 text-slate-900">${escapeHtml(tenant.trade_name)}</strong>
          <p class="text-slate-600 mt-1">${escapeHtml(alert.detail)}</p>
        </div>
        <button type="button" data-action="inspect-tenant" data-tenant-id="${tenant.id}" class="px-3 py-1.5 rounded-xl bg-white border border-slate-300 font-bold hover:bg-slate-50">Inspecionar</button>
      </div>`,
    )
    .join('');
}

export async function loadMasterTenants(): Promise<void> {
  if (!isMasterAdminUser()) return;

  await loadPlatformSettings();

  const { data: tenList } = await sb.from('tenants').select('*').order('created_at', { ascending: false });
  appState.allPlatformTenants = ((tenList ?? []) as Tenant[]).filter(t => {
    const em = String(t.email ?? '').toLowerCase().trim();
    const isOwn = appState.tenant && t.id === appState.tenant.id;
    return em !== MASTER_ADMIN_EMAIL && !isOwn;
  });

  const { data: allPrinters } = await sb
    .from('printers')
    .select('id, tenant_id, model, serial, ip, usb_port, status, total_pages, last_seen, billing_exempt, billing_mode');
  const ids = new Set(appState.allPlatformTenants.map(t => t.id));
  appState.allPlatformPrinters = ((allPrinters ?? []) as Printer[]).filter(p => ids.has(p.tenant_id));

  tenantRiskMap = analyzeTenantRisks(appState.allPlatformTenants, appState.allPlatformPrinters);

  let activeMRR = 0;
  let activeCount = 0;
  let blockedCount = 0;
  let totalAlerts = 0;
  let globalBillable = 0;
  let globalExempt = 0;

  appState.allPlatformTenants.forEach(t => {
    const prts = appState.allPlatformPrinters.filter(p => p.tenant_id === t.id);
    const { billable, exempt } = getBillablePrintersSummary(prts);
    globalBillable += billable.length;
    globalExempt += exempt.length;
    const rate = getTenantRate(t.plan_tier);
    const monthly = billable.length * rate;
    const st = String(t.subscription_status ?? 'trialing').toLowerCase();
    const pt = String(t.plan_tier ?? 'trial').toLowerCase();

    if (st === 'past_due' || st === 'canceled') {
      blockedCount++;
    } else if (st === 'active' && pt !== 'trial') {
      activeCount++;
      if (!t.pioneer_courtesy_active) activeMRR += monthly;
    }
    totalAlerts += (tenantRiskMap[t.id] ?? []).filter(a => a.severity !== 'low').length;
  });

  const setText = (id: string, val: string): void => {
    const el = byId(id);
    if (el) el.innerText = val;
  };
  setText('kpiMasterTenants', String(appState.allPlatformTenants.length));
  setText('kpiMasterTenantsSub', `${activeCount} ativos • ${blockedCount} bloqueados`);
  setText('kpiMasterPrinters', String(globalBillable));
  setText('kpiMasterPrintersSub', `${globalBillable} faturáveis • ${globalExempt} isenta(s)`);
  setText('kpiMasterMRR', formatCurrencyBrl(activeMRR));
  setText('kpiMasterRiskCount', String(totalAlerts));
  setText('kpiMasterRiskSub', totalAlerts > 0 ? 'Clique para revisar indícios' : 'Sem indícios críticos');

  const fraudBadge = byId('masterFraudTabBadge');
  if (fraudBadge) {
    fraudBadge.innerText = String(totalAlerts);
    fraudBadge.classList.toggle('hidden', totalAlerts === 0);
  }

  const slotsTotalInput = byId<HTMLInputElement>('masterPioneerSlotsTotalInput');
  const slotsUsedLbl = byId('masterPioneerSlotsUsedLabel');
  if (slotsTotalInput) slotsTotalInput.value = String(appState.platformSettings.pioneer_slots_total);
  if (slotsUsedLbl) {
    slotsUsedLbl.innerText = `${appState.platformSettings.pioneer_slots_used} usadas de ${appState.platformSettings.pioneer_slots_total}`;
  }

  const inviteInp = byId<HTMLInputElement>('masterInviteLink');
  if (inviteInp) {
    inviteInp.value = `${window.location.origin}${window.location.pathname}`;
  }

  renderMasterSubscribersTable();
  renderMasterBillingView();
  renderMasterFraudAlerts();
  refreshIcons();
}

function inspectMasterTenant(tenantId: string): void {
  const t = appState.allPlatformTenants.find(x => x.id === tenantId);
  const body = byId('masterTenantDetailBody');
  const title = byId('masterTenantDetailTitle');
  if (!t || !body) return;

  if (title) title.innerText = `Auditoria • ${t.trade_name}`;
  const prts = appState.allPlatformPrinters.filter(p => p.tenant_id === t.id);
  const alerts = tenantRiskMap[t.id] ?? [];

  body.innerHTML = `
    <div class="space-y-4 text-xs">
      <div class="p-4 rounded-2xl bg-slate-50 border border-slate-200 grid grid-cols-2 gap-2">
        <div><span class="text-slate-400 block">E-mail</span><strong>${escapeHtml(t.email)}</strong></div>
        <div><span class="text-slate-400 block">CNPJ/CPF</span><strong class="mono">${escapeHtml(t.cnpj_cpf ?? '-')}</strong></div>
        <div><span class="text-slate-400 block">Cidade/UF</span><strong>${escapeHtml(t.city ?? '-')}/${escapeHtml(t.state_uf ?? '-')}</strong></div>
        <div><span class="text-slate-400 block">Status</span>${getStatusBadgeHtml(t)}</div>
      </div>
      ${
        alerts.length > 0
          ? `<div class="space-y-1.5">${alerts.map(a => `<div class="p-2.5 rounded-xl bg-rose-50 border border-rose-200 text-rose-800"><strong>${escapeHtml(a.label)}:</strong> ${escapeHtml(a.detail)}</div>`).join('')}</div>`
          : ''
      }
      <div class="space-y-1.5">
        <strong class="text-slate-700 block">Máquinas Monitoradas (${prts.length})</strong>
        ${prts.map(p => `<div class="p-2 rounded-xl border border-slate-200 flex justify-between mono"><span>${escapeHtml(p.model)} (${escapeHtml(p.serial)})</span><span>${p.billing_exempt ? 'ISENTA' : 'FATURÁVEL'}</span></div>`).join('')}
      </div>
    </div>`;
  openModal('masterTenantDetailModal');
}

export function initMasterView(): void {
  byId('masterSubBtnTenants')?.addEventListener('click', () => switchMasterSubTab('tenants'));
  byId('masterSubBtnBilling')?.addEventListener('click', () => switchMasterSubTab('billing'));
  byId('masterSubBtnFraud')?.addEventListener('click', () => switchMasterSubTab('fraud'));
  byId('masterSubBtnConfig')?.addEventListener('click', () => switchMasterSubTab('config'));

  byId('masterSearchInput')?.addEventListener('input', () => renderMasterSubscribersTable());
  byId('masterStatusFilter')?.addEventListener('change', () => renderMasterSubscribersTable());
  byId('btnRefreshMasterTenants')?.addEventListener('click', () => void loadMasterTenants());

  const handleInspectOrSuspend = (e: Event): void => {
    const btn = (e.target as HTMLElement).closest<HTMLElement>('[data-action]');
    if (!btn) return;
    const tid = btn.dataset.tenantId ?? '';
    if (btn.dataset.action === 'inspect-tenant' && tid) {
      inspectMasterTenant(tid);
    } else if (btn.dataset.action === 'toggle-suspend' && tid) {
      const next = btn.dataset.next ?? 'past_due';
      void (async () => {
        if (!isMasterAdminUser()) return;
        const { error } = await sb.from('tenants').update({ subscription_status: next }).eq('id', tid);
        if (error) {
          alert('Erro ao atualizar status do assinante: ' + error.message);
          return;
        }
        await loadMasterTenants();
      })();
    }
  };

  byId('masterTenantsTableBody')?.addEventListener('click', handleInspectOrSuspend);
  byId('masterFraudAlertsList')?.addEventListener('click', handleInspectOrSuspend);

  byId('btnSavePioneerQuota')?.addEventListener('click', () => {
    void (async () => {
      if (!isMasterAdminUser()) return;
      const val = Math.max(0, Number(byId<HTMLInputElement>('masterPioneerSlotsTotalInput')?.value ?? 100));
      const { error } = await sb
        .from('platform_settings')
        .update({ pioneer_slots_total: val, updated_at: new Date().toISOString() })
        .eq('id', 'default');
      if (error) {
        alert('Erro ao salvar cota Pioneiros: ' + error.message);
        return;
      }
      await loadMasterTenants();
      alert('Cota de Pioneiros atualizada!');
    })();
  });

  byId('btnCopyMasterInviteLink')?.addEventListener('click', () => {
    const link = byId<HTMLInputElement>('masterInviteLink')?.value ?? '';
    void navigator.clipboard?.writeText(link);
    alert('Link de convite copiado!');
  });

  byId('btnExportMasterBillingCSV')?.addEventListener('click', () => {
    const lines = ['Empresa;Email;Plano;Status;Maquinas_Faturaveis'];
    appState.allPlatformTenants.forEach(t => {
      const prts = appState.allPlatformPrinters.filter(p => p.tenant_id === t.id);
      const { billable } = getBillablePrintersSummary(prts);
      lines.push(`"${t.trade_name}";"${t.email}";"${t.plan_tier}";"${t.subscription_status}";${billable.length}`);
    });
    const blob = new Blob(['\uFEFF' + lines.join('\n')], { type: 'text/csv;charset=utf-8;' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = `printmind_master_faturamento.csv`;
    a.click();
  });
}
