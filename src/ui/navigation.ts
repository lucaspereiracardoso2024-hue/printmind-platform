import { AppTabId, ReportsSubTabId, appState, byId, closeModal, isMasterAdminUser, openModal } from '../state/store';
import {
  PLAN_CATALOG,
  getBillablePrintersSummary,
  normalizePlanTier,
  resolveEffectiveCourtesyLimit,
} from '../domain/billingEngine';
import { refreshIcons } from '../utils/icons';

type TabChangeHandler = (tab: AppTabId) => void;
let onTabChangeCallback: TabChangeHandler | null = null;

export function registerTabChangeHandler(cb: TabChangeHandler): void {
  onTabChangeCallback = cb;
}

export function syncMasterAccess(userEmail?: string): void {
  const em = String(userEmail ?? appState.session?.user?.email ?? '').toLowerCase().trim();
  const isMaster = em !== '' && isMasterAdminUser();
  const navMaster = byId('navTabMaster');
  if (navMaster) {
    navMaster.classList.toggle('hidden', !isMaster);
  }
  const masterSec = byId('sectionMaster');
  if (masterSec && !isMaster && appState.currentTab === 'master') {
    switchTab('clients');
  }
}

export function applyOperationModeUI(mode: string): void {
  const isGrafica = mode === 'grafica';
  document.querySelectorAll<HTMLElement>('[data-outsourcing-only="true"]').forEach(el => {
    el.classList.toggle('hidden', isGrafica);
  });
  const reportsLbl = byId('navTabReportsLabel');
  if (reportsLbl) {
    reportsLbl.innerText = isGrafica ? 'Suprimentos & IA' : 'Fechamento & IA';
  }
  const modeSel = byId<HTMLSelectElement>('modeSwitcher');
  if (modeSel) {
    modeSel.value = isGrafica ? 'grafica' : 'outsourcing';
  }
  if (isGrafica) {
    if (appState.currentTab === 'clients' || appState.currentTab === 'tickets') {
      switchTab('grafica');
    }
    if (appState.currentReportsSubTab === 'closing') {
      switchReportsSubTab('supplies');
    }
  }
}

export function switchTab(tabId: AppTabId): void {
  if (tabId === 'master' && !isMasterAdminUser()) {
    tabId = 'clients';
  }
  if (appState.tenant?.operation_mode === 'grafica' && (tabId === 'clients' || tabId === 'tickets')) {
    tabId = 'grafica';
  }
  appState.currentTab = tabId;

  const sections: Record<AppTabId, string> = {
    clients: 'sectionClients',
    reports: 'sectionReports',
    tickets: 'sectionTickets',
    grafica: 'sectionGrafica',
    company: 'sectionCompany',
    plans: 'sectionPlans',
    master: 'sectionMaster',
  };

  (Object.keys(sections) as AppTabId[]).forEach(key => {
    const sec = byId(sections[key]);
    if (sec) sec.classList.toggle('hidden', key !== tabId);
  });

  const navButtons: Record<AppTabId, string> = {
    clients: 'navTabClients',
    reports: 'navTabReports',
    tickets: 'navTabTickets',
    grafica: 'navTabGrafica',
    company: 'navTabCompany',
    plans: 'navTabPlans',
    master: 'navTabMaster',
  };

  (Object.keys(navButtons) as AppTabId[]).forEach(key => {
    const btn = byId(navButtons[key]);
    if (!btn) return;
    if (key === tabId) {
      btn.className =
        'px-3 py-1.5 rounded-xl bg-white text-[#0038A8] font-black shadow-sm transition flex items-center space-x-1.5 text-xs';
    } else if (key === 'master') {
      btn.className =
        'px-3 py-1.5 rounded-xl bg-amber-400/20 text-amber-200 hover:bg-amber-400/30 border border-amber-300/30 transition flex items-center space-x-1.5 text-xs font-bold';
    } else {
      btn.className =
        'px-3 py-1.5 rounded-xl text-white/85 hover:text-white hover:bg-white/10 transition flex items-center space-x-1.5 text-xs font-semibold';
    }
  });

  if (onTabChangeCallback) {
    onTabChangeCallback(tabId);
  }
  refreshIcons();
}

export function switchReportsSubTab(sub: ReportsSubTabId): void {
  if (appState.tenant?.operation_mode === 'grafica' && sub === 'closing') {
    sub = 'supplies';
  }
  appState.currentReportsSubTab = sub;
  const subMap: Record<ReportsSubTabId, { pane: string; btn: string }> = {
    closing: { pane: 'reportsSubClosing', btn: 'repSubBtnClosing' },
    supplies: { pane: 'reportsSubSupplies', btn: 'repSubBtnSupplies' },
    ai: { pane: 'reportsSubAi', btn: 'repSubBtnAi' },
  };
  (Object.keys(subMap) as ReportsSubTabId[]).forEach(k => {
    const p = byId(subMap[k].pane);
    const b = byId(subMap[k].btn);
    if (p) p.classList.toggle('hidden', k !== sub);
    if (b) {
      b.className =
        k === sub
          ? 'px-4 py-2 rounded-xl bg-[#0038A8] text-white text-xs font-bold shadow-sm transition'
          : 'px-4 py-2 rounded-xl bg-slate-100 text-slate-700 hover:bg-slate-200 text-xs font-bold transition';
    }
  });
  refreshIcons();
}

export function checkTrialPrinterLock(): void {
  const ten = appState.tenant;
  if (!ten) return;

  const isMaster = isMasterAdminUser();
  const status = String(ten.subscription_status ?? 'trialing').toLowerCase();
  const tier = String(ten.plan_tier ?? 'trial').toLowerCase();

  const { billable, exempt } = getBillablePrintersSummary(appState.printers);
  const minCharge = appState.platformSettings.asaas_min_charge_brl;
  const courtesyLimit = resolveEffectiveCourtesyLimit(ten.max_printers, minCharge, tier);
  const courtesyExceeded = Boolean(ten.pioneer_courtesy_active && billable.length > courtesyLimit);

  const overlay = byId('suspendedTenantOverlay');
  if (overlay) {
    const blocked = !isMaster && (status === 'past_due' || status === 'canceled' || courtesyExceeded);
    overlay.classList.toggle('hidden', !blocked);
    const title = byId('suspendedOverlayTitle');
    const desc = byId('suspendedOverlayDesc');
    if (title && desc && blocked) {
      if (courtesyExceeded) {
        title.innerText = 'Teto da Cortesia Pioneiros Atingido';
        desc.innerText = `Seu parque atingiu ${billable.length} máquinas não isentas (limite da cortesia: ${courtesyLimit}). Faça o upgrade com checkout real no Asaas (>= R$ ${minCharge.toFixed(2)}) na aba Planos.`;
      } else if (status === 'past_due') {
        title.innerText = 'Assinatura com Pagamento Pendente';
        desc.innerText =
          'Detectamos uma pendência financeira no Asaas. Regularize sua fatura na aba Planos para desbloquear o parque.';
      } else {
        title.innerText = 'Assinatura Cancelada';
        desc.innerText =
          'Sua conta retornou ao limite gratuito de até 2 impressoras faturáveis. Reative um plano quando precisar.';
      }
    }
  }

  const headerPill = byId('tenantPlanBadge');
  if (headerPill) {
    if (ten.pioneer_courtesy_active) {
      headerPill.innerText = courtesyExceeded
        ? `Cortesia Excedida (${billable.length}/${courtesyLimit} • Upgrade Asaas >= R$ ${minCharge.toFixed(2)})`
        : `Cortesia Pioneiros (${billable.length}/${courtesyLimit} máq)`;
      headerPill.className = courtesyExceeded
        ? 'px-2.5 py-1 rounded-full text-[11px] font-black bg-rose-500/30 text-white border border-rose-300/60'
        : 'px-2.5 py-1 rounded-full text-[11px] font-black bg-emerald-400/20 text-emerald-100 border border-emerald-300/40';
    } else if (status === 'active' && tier !== 'trial') {
      const planName = PLAN_CATALOG[normalizePlanTier(tier)].name.split('—')[0].trim();
      headerPill.innerText = `${planName} • ${billable.length} faturáveis${exempt.length > 0 ? ` (+${exempt.length} isentas)` : ''}`;
      headerPill.className =
        'px-2.5 py-1 rounded-full text-[11px] font-black bg-white/15 text-white border border-white/25';
    } else if (status === 'pending_payment') {
      headerPill.innerText = 'Pagamento PIX/Asaas Aguardando Confirmação';
      headerPill.className =
        'px-2.5 py-1 rounded-full text-[11px] font-black bg-amber-400/25 text-amber-100 border border-amber-300/50';
    } else {
      headerPill.innerText = `Teste Grátis (${billable.length}/2 faturáveis)`;
      headerPill.className =
        'px-2.5 py-1 rounded-full text-[11px] font-black bg-amber-400/20 text-amber-100 border border-amber-300/35';
    }
  }

  const fbBadge = byId('fairBillingBadge');
  if (fbBadge) {
    const { billable, exempt } = getBillablePrintersSummary(appState.printers);
    fbBadge.innerText =
      exempt.length > 0
        ? `Fair Billing Ativo: ${billable.length} cobrável(is) • ${exempt.length} isenta(s)`
        : `Fair Billing Ativo: ${billable.length} impressora(s) cobrável(is)`;
  }
}

export function updateInteractiveWalkthrough(): void {
  const card = byId('walkthroughCard');
  const s1Card = byId('wtStep1Card');
  const s2Card = byId('wtStep2Card');
  if (!card || !s1Card || !s2Card) return;

  const hasClients = appState.clients.length > 0;
  const hasPrinters = appState.printers.length > 0;
  if (hasClients && hasPrinters && !appState.forceShowOnboarding) {
    card.classList.add('hidden');
    return;
  }
  card.classList.remove('hidden');

  const badge = byId('wtStepBadge');
  const desc = byId('wtDesc');
  const indicator = byId('wtStatusIndicator');
  const s1Pill = byId('wtStep1Pill');
  const s1Btn = byId<HTMLButtonElement>('wtStep1Btn');
  const s2Pill = byId('wtStep2Pill');
  const s2Btn = byId<HTMLButtonElement>('wtStep2Btn');

  if (!hasClients) {
    if (badge) badge.innerText = 'Passo 1 de 2';
    if (desc) desc.innerText = 'Comece cadastrando seu primeiro cliente ou unidade para organizar impressoras e fechamentos.';
    if (indicator) indicator.innerText = 'Aguardando 1º Cliente';
    if (s1Pill) s1Pill.innerText = 'COMECE AQUI';
    if (s1Btn) s1Btn.disabled = false;
    if (s2Pill) s2Pill.innerText = 'Aguardando Passo 1';
    if (s2Btn) s2Btn.disabled = true;
  } else if (!hasPrinters) {
    if (badge) badge.innerText = 'Passo 2 de 2';
    if (desc) desc.innerText = 'Cliente cadastrado! Conecte suas impressoras usando o Scanner v4.2.0 ou cadastro manual.';
    if (indicator) indicator.innerText = 'Aguardando Impressora';
    if (s1Pill) s1Pill.innerText = '✓ Concluído';
    if (s2Pill) s2Pill.innerText = 'PRÓXIMO PASSO';
    if (s2Btn) s2Btn.disabled = false;
  } else {
    if (badge) badge.innerText = '✓ Setup Concluído';
    if (desc) desc.innerText = 'Seu parque está monitorado. Você pode fechar este guia ou revisar o passo a passo quando quiser.';
    if (indicator) indicator.innerText = 'Operação Normal';
    if (s1Pill) s1Pill.innerText = `${appState.clients.length} cliente(s)`;
    if (s2Pill) s2Pill.innerText = `${appState.printers.length} impressora(s)`;
    if (s2Btn) s2Btn.disabled = false;
  }
}

export function openOnboardingOnDemand(): void {
  appState.forceShowOnboarding = !appState.forceShowOnboarding;
  const onboardingCard = byId('onboardingCard');
  if (onboardingCard) {
    onboardingCard.classList.toggle('hidden', !appState.forceShowOnboarding);
  }
  updateInteractiveWalkthrough();
  if (appState.forceShowOnboarding) {
    openModal('tourModal');
  }
}

export function checkFirstTimeUserTour(): void {
  const userId = appState.session?.user?.id ?? 'anon';
  const key = `printmind_tour_seen_${userId}`;
  if (appState.clients.length === 0 && appState.printers.length === 0 && !window.localStorage.getItem(key)) {
    openModal('tourModal');
  }
}

export function switchTourSlide(slideNum: number): void {
  [1, 2, 3].forEach(n => {
    const slide = byId(`tourSlide${n}`);
    const dot = byId(`tourDot${n}`);
    if (slide) slide.classList.toggle('hidden', n !== slideNum);
    if (dot) {
      dot.className =
        n === slideNum ? 'w-6 h-2 rounded-full bg-[#0038A8] transition-all' : 'w-2 h-2 rounded-full bg-slate-300 transition-all';
    }
  });
}

export function finishTourAndStart(): void {
  const userId = appState.session?.user?.id ?? 'anon';
  window.localStorage.setItem(`printmind_tour_seen_${userId}`, '1');
  closeModal('tourModal');
}
