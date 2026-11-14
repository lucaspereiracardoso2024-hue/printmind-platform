import './styles.css';
import { sb } from './services/supabaseClient';
import { appState, byId, closeModal, loadTenantData } from './state/store';
import { refreshIcons } from './utils/icons';
import { initAuthView, onSessionAuthenticated } from './ui/authView';
import { initClientsView, openClientModalForCreate, renderClientsDashboard } from './ui/clientsView';
import { initPrintersView, populatePrinterFormSelects, renderPrintersGrid } from './ui/printersView';
import { initClosingInvoiceView, openClientInvoice, sendQuickWhatsAppClient } from './ui/closingInvoiceView';
import { initReportsView, renderReportsView } from './ui/reportsView';
import { renderGraficaView } from './ui/graficaView';
import { initTicketsView, renderTicketsView } from './ui/ticketsView';
import { initCompanyView, renderCompanyView } from './ui/companyView';
import { initPlansView, openPricingModal, renderPlansView } from './ui/plansView';
import { initMasterView, loadMasterTenants } from './ui/masterView';
import { mountPublicPortal } from './ui/portalView';
import {
  applyOperationModeUI,
  checkFirstTimeUserTour,
  checkTrialPrinterLock,
  finishTourAndStart,
  openOnboardingOnDemand,
  registerTabChangeHandler,
  switchTab,
  switchTourSlide,
  updateInteractiveWalkthrough,
} from './ui/navigation';

function renderAllViews(): void {
  renderClientsDashboard();
  renderPrintersGrid();
  updateInteractiveWalkthrough();
  checkTrialPrinterLock();

  if (appState.currentTab === 'reports') renderReportsView();
  else if (appState.currentTab === 'grafica') renderGraficaView();
  else if (appState.currentTab === 'tickets') renderTicketsView();
  else if (appState.currentTab === 'company') renderCompanyView();
  else if (appState.currentTab === 'plans') renderPlansView();
  else if (appState.currentTab === 'master') void loadMasterTenants();

  refreshIcons();
}

function getPortalTokenFromHash(): string | null {
  const hash = window.location.hash || '';
  if (hash.startsWith('#portal=')) {
    const tok = decodeURIComponent(hash.slice('#portal='.length)).trim();
    return tok || null;
  }
  return null;
}

function bindGlobalUiListeners(): void {
  document.querySelectorAll<HTMLElement>('[data-close-modal]').forEach(btn => {
    btn.addEventListener('click', () => {
      const targetId = btn.dataset.closeModal ?? '';
      if (targetId) closeModal(targetId);
    });
  });

  byId('navTabClients')?.addEventListener('click', () => switchTab('clients'));
  byId('navTabReports')?.addEventListener('click', () => switchTab('reports'));
  byId('navTabTickets')?.addEventListener('click', () => switchTab('tickets'));
  byId('navTabGrafica')?.addEventListener('click', () => switchTab('grafica'));
  byId('navTabCompany')?.addEventListener('click', () => switchTab('company'));
  byId('navTabPlans')?.addEventListener('click', () => switchTab('plans'));
  byId('navTabMaster')?.addEventListener('click', () => switchTab('master'));

  byId('btnOpenOnboarding')?.addEventListener('click', () => openOnboardingOnDemand());
  byId('wtStep1Btn')?.addEventListener('click', () => openClientModalForCreate());
  byId('wtStep2Btn')?.addEventListener('click', () => {
    populatePrinterFormSelects();
    byId('btnOpenNewPrinter')?.click();
  });

  byId<HTMLSelectElement>('modeSwitcher')?.addEventListener('change', e => {
    const mode = (e.target as HTMLSelectElement).value;
    void (async () => {
      if (appState.tenant) {
        const previousMode = appState.tenant.operation_mode || 'outsourcing';
        const { error } = await sb
          .from('tenants')
          .update({ operation_mode: mode })
          .eq('id', appState.tenant.id);
        if (error) {
          applyOperationModeUI(previousMode);
          alert('Não foi possível salvar o modo de operação: ' + error.message);
          return;
        }
        appState.tenant.operation_mode = mode;
      }
      applyOperationModeUI(mode);
      if (mode === 'grafica') switchTab('grafica');
      else switchTab('clients');
    })();
  });

  byId('btnTourStep1')?.addEventListener('click', () => switchTourSlide(2));
  byId('btnTourStep2')?.addEventListener('click', () => switchTourSlide(3));
  byId('btnTourFinish')?.addEventListener('click', () => finishTourAndStart());

  registerTabChangeHandler(tab => {
    if (tab === 'reports') renderReportsView();
    else if (tab === 'grafica') renderGraficaView();
    else if (tab === 'tickets') renderTicketsView();
    else if (tab === 'company') renderCompanyView();
    else if (tab === 'plans') renderPlansView();
    else if (tab === 'master') void loadMasterTenants();
  });
}

async function bootstrap(): Promise<void> {
  const portalToken = getPortalTokenFromHash();
  if (portalToken) {
    await mountPublicPortal(portalToken);
    return;
  }

  window.addEventListener('hashchange', () => {
    const tok = getPortalTokenFromHash();
    if (tok) {
      void mountPublicPortal(tok);
    }
  });

  bindGlobalUiListeners();

  initClientsView({
    onOpenInvoice: cid => openClientInvoice(cid),
    onSendQuickWhatsApp: cid => sendQuickWhatsAppClient(cid),
    onDataChanged: () => renderAllViews(),
  });

  initPrintersView({
    onDataChanged: () => renderAllViews(),
    onOpenPricing: () => openPricingModal(),
  });

  initClosingInvoiceView({
    onDataChanged: () => renderAllViews(),
  });

  initReportsView({
    onOpenInvoice: cid => openClientInvoice(cid),
    onDataChanged: () => renderAllViews(),
  });

  initTicketsView();

  initCompanyView({
    onDataChanged: () => renderAllViews(),
    onAccountDeleted: async () => {
      await sb.auth.signOut();
      window.location.reload();
    },
  });

  initPlansView({
    onDataChanged: () => renderAllViews(),
  });

  initMasterView();

  initAuthView(() => {
    renderAllViews();
    checkFirstTimeUserTour();
    renderTicketsView();
  });

  const { data } = await sb.auth.getSession();
  if (data.session) {
    await onSessionAuthenticated(data.session);
    await loadTenantData();
    renderAllViews();
  }

  refreshIcons();
}

void bootstrap();
