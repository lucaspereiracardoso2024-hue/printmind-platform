import { AGENT_RELEASE_BASE, APP_VERSION, sb } from '../services/supabaseClient';
import { appState, byId, getActiveUnitPairingToken, loadTenantData } from '../state/store';
import { escapeHtml, formatElapsedSince } from '../utils/format';
import { copyMsiSilentCommand, downloadPairedAgent } from './printersView';
import { checkTrialPrinterLock } from './navigation';

type CompanyCallbacks = {
  onDataChanged: () => void;
  onAccountDeleted: () => Promise<void>;
};

let companyCallbacks: CompanyCallbacks = {
  onDataChanged: () => {},
  onAccountDeleted: async () => {},
};

export function renderCompanyView(): void {
  const ten = appState.tenant;
  const setVal = (id: string, val: string): void => {
    const el = byId<HTMLInputElement>(id);
    if (el) el.value = val;
  };

  if (ten) {
    setVal('compTradeName', ten.trade_name ?? '');
    setVal('compLegalName', ten.legal_name ?? '');
    setVal('compCnpjCpf', ten.cnpj_cpf ?? '');
    setVal('compPhone', ten.phone ?? '');
    setVal('compCity', ten.city ?? '');
    setVal('compStateUf', ten.state_uf ?? '');
    setVal('compPixKey', ten.pix_key ?? '');
    setVal('compWalletId', ten.baas_wallet_id ?? '');

    const isGrafica = ten.operation_mode === 'grafica';
    const cityInput = byId<HTMLInputElement>('compCity');
    const pixInput = byId<HTMLInputElement>('compPixKey');
    if (cityInput) cityInput.required = false;
    if (pixInput) pixInput.required = false;
    if (isGrafica && cityInput) {
      cityInput.placeholder = 'Ex: Curitiba (opcional)';
    } else if (cityInput) {
      cityInput.placeholder = 'Ex: Curitiba';
    }
  }

  renderUnitsTokensList();
}

export function renderUnitsTokensList(): void {
  const msiPrev = byId('msiSilentCommandPreview');
  const activeTok = getActiveUnitPairingToken() || 'SEU_TOKEN_AQUI';
  if (msiPrev) {
    msiPrev.innerText = `msiexec /i PrintMindAgentSetup-v${APP_VERSION}-x64.msi PAIRINGTOKEN="${activeTok}" /quiet`;
  }

  const winLink = byId<HTMLAnchorElement>('btnDirectAgentWinDownload');
  const msiLink = byId<HTMLAnchorElement>('btnDirectMsiDownload');
  if (winLink) winLink.href = `${AGENT_RELEASE_BASE}/printmind-agent.exe`;
  if (msiLink) msiLink.href = `${AGENT_RELEASE_BASE}/PrintMindAgentSetup-v${APP_VERSION}-x64.msi`;

  const container = byId('unitsTokensList');
  if (!container) return;

  if (appState.units.length === 0) {
    container.innerHTML = `<div class="p-4 rounded-2xl bg-slate-50 text-slate-500 text-center text-xs">Cadastre seu primeiro cliente para gerar automaticamente o Token de Pareamento do Scanner v${APP_VERSION}.</div>`;
    return;
  }

  container.innerHTML = appState.units
    .map(u => {
      const cli = appState.clients.find(c => c.id === u.client_id);
      const tok = u.pairing_token ?? '';
      const hb = u.last_heartbeat
        ? formatElapsedSince(Date.now() - new Date(u.last_heartbeat).getTime())
        : 'Nunca conectado';
      return `
        <div class="p-4 rounded-2xl border border-slate-200 bg-slate-50 flex flex-wrap items-center justify-between gap-3 text-xs">
          <div class="space-y-1">
            <div class="flex items-center gap-2">
              <strong class="text-slate-900">${escapeHtml(cli ? cli.name : 'Conta')} • ${escapeHtml(u.unit_name)}</strong>
              <span class="px-2 py-0.5 rounded-full text-[10px] font-bold bg-blue-100 text-[#0038A8] mono">v${escapeHtml(u.agent_version ?? APP_VERSION)}</span>
              ${u.wpp_enabled ? '<span class="px-2 py-0.5 rounded-full text-[10px] font-bold bg-emerald-100 text-emerald-800">WPP Win11</span>' : ''}
            </div>
            <div class="text-[11px] mono text-slate-500">Token: <span class="text-slate-800 font-bold">${escapeHtml(tok)}</span> • Heartbeat: ${escapeHtml(hb)}</div>
          </div>
          <div class="flex flex-wrap items-center gap-1.5">
            <button type="button" data-action="copy-token" data-token="${escapeHtml(tok)}" class="px-2.5 py-1.5 rounded-xl bg-white border border-slate-300 text-slate-700 font-bold hover:bg-slate-100">Copiar Token</button>
            <button type="button" data-action="download-win" data-token="${escapeHtml(tok)}" class="px-2.5 py-1.5 rounded-xl bg-[#0038A8] text-white font-bold hover:bg-blue-800">Baixar EXE Pareado</button>
            <button type="button" data-action="copy-msi" data-token="${escapeHtml(tok)}" class="px-2.5 py-1.5 rounded-xl bg-slate-900 text-white font-bold hover:bg-slate-800">Comando MSI</button>
          </div>
        </div>`;
    })
    .join('');
}

export function initCompanyView(callbacks: CompanyCallbacks): void {
  companyCallbacks = callbacks;

  byId<HTMLFormElement>('formCompanyProfile')?.addEventListener('submit', e => {
    e.preventDefault();
    void (async () => {
      const ten = appState.tenant;
      if (!ten) return;
      const isGrafica = ten.operation_mode === 'grafica';

      const trade_name = byId<HTMLInputElement>('compTradeName')?.value.trim() ?? '';
      const legal_name = byId<HTMLInputElement>('compLegalName')?.value.trim() || null;
      const cnpj_cpf = byId<HTMLInputElement>('compCnpjCpf')?.value.trim() || null;
      const phone = byId<HTMLInputElement>('compPhone')?.value.trim() || null;
      const city = byId<HTMLInputElement>('compCity')?.value.trim() || null;
      const state_uf = (byId<HTMLInputElement>('compStateUf')?.value ?? '').trim().toUpperCase().slice(0, 2) || null;
      const pix_key = isGrafica ? ten.pix_key ?? null : byId<HTMLInputElement>('compPixKey')?.value.trim() || null;
      const baas_wallet_id = isGrafica
        ? ten.baas_wallet_id ?? null
        : byId<HTMLInputElement>('compWalletId')?.value.trim() || null;

      if (!trade_name) {
        alert('Informe o Nome Fantasia da empresa.');
        return;
      }

      const { error } = await sb
        .from('tenants')
        .update({
          trade_name,
          legal_name,
          cnpj_cpf,
          phone,
          city,
          state_uf,
          pix_key,
          baas_wallet_id,
          updated_at: new Date().toISOString(),
        })
        .eq('id', ten.id);

      const feedback = byId('companySaveFeedback');
      if (error) {
        if (feedback) {
          feedback.classList.remove('hidden', 'text-emerald-700');
          feedback.classList.add('text-rose-700');
          feedback.innerText = 'Erro ao salvar dados: ' + error.message;
        } else {
          alert('Erro ao salvar empresa: ' + error.message);
        }
        return;
      }

      await loadTenantData();
      const tenNameEl = byId('headerTenantName');
      if (tenNameEl && appState.tenant) tenNameEl.innerText = appState.tenant.trade_name;
      checkTrialPrinterLock();
      companyCallbacks.onDataChanged();

      if (feedback) {
        feedback.classList.remove('hidden', 'text-rose-700');
        feedback.classList.add('text-emerald-700');
        feedback.innerText = isGrafica
          ? '✓ Dados da empresa salvos com sucesso!'
          : '✓ Dados da empresa e PIX EMVCo salvos com sucesso!';
      }
    })();
  });

  byId('btnCopyMsiGlobal')?.addEventListener('click', () => copyMsiSilentCommand());
  byId('unitsTokensList')?.addEventListener('click', e => {
    const btn = (e.target as HTMLElement).closest<HTMLElement>('[data-action]');
    if (!btn) return;
    const action = btn.dataset.action;
    const tok = btn.dataset.token ?? '';
    if (action === 'copy-token') {
      void navigator.clipboard?.writeText(tok);
      alert('Token copiado!');
    } else if (action === 'download-win') {
      downloadPairedAgent('windows', tok);
    } else if (action === 'copy-msi') {
      copyMsiSilentCommand(tok);
    }
  });

  byId('btnDeleteTenantAccount')?.addEventListener('click', () => {
    void (async () => {
      const ok1 = confirm(
        'Tem certeza que deseja excluir sua conta PrintMind permanentemente?\nTodos os clientes, leituras e tokens serão apagados (LGPD).',
      );
      if (!ok1) return;
      const typed = prompt('Digite EXCLUIR para confirmar:');
      if (typed !== 'EXCLUIR') return;

      const { error } = await sb.rpc('delete_user_account');
      if (error) {
        alert('Erro ao excluir conta: ' + error.message);
        return;
      }
      alert('Conta e dados excluídos conforme LGPD.');
      await companyCallbacks.onAccountDeleted();
    })();
  });
}
