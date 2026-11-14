import {
  FREE_TRIAL_PRINTER_LIMIT,
  PLAN_CATALOG,
  computeCourtesyMaxPrinters,
  getBillablePrintersSummary,
  normalizePlanTier,
  quotePlanLocally,
  resolveEffectiveCourtesyLimit,
} from '../domain/billingEngine';
import {
  cancelSaasSubscription,
  createSaasCheckout,
  quoteSaasPlan,
} from '../services/billingService';
import { appState, byId, closeModal, loadTenantData, openModal } from '../state/store';
import { PlanTier } from '../types/models';
import { escapeHtml, formatCurrencyBrl } from '../utils/format';
import { renderQrDataUrl } from '../utils/qrcodeRender';
import { checkTrialPrinterLock, switchTab } from './navigation';

type PlansCallbacks = {
  onDataChanged: () => void;
};

let plansCallbacks: PlansCallbacks = {
  onDataChanged: () => {},
};

let activeSelectedPlan: Exclude<PlanTier, 'trial'> = 'pro';

export function renderPlansView(): void {
  const ten = appState.tenant;
  const settings = appState.platformSettings;
  const slotsRemaining = Math.max(0, settings.pioneer_slots_total - settings.pioneer_slots_used);
  const { billable, exempt } = getBillablePrintersSummary(appState.printers);

  const slotsBadge = byId('pioneerSlotsRemainingBadge');
  if (slotsBadge) {
    slotsBadge.innerText =
      slotsRemaining > 0
        ? `Cortesia Pioneiros: ${slotsRemaining} de ${settings.pioneer_slots_total} vagas restantes`
        : 'Vagas Pioneiros encerradas';
  }

  const currentPlanBox = byId('currentPlanStatusSummary');
  if (currentPlanBox && ten) {
    const st = String(ten.subscription_status ?? 'trialing').toLowerCase();
    const pt = String(ten.plan_tier ?? 'trial').toLowerCase();
    const activeTierLabel =
      pt === 'trial' ? 'Teste Gratuito (até 2 faturáveis)' : PLAN_CATALOG[normalizePlanTier(pt)].name;
    const minCharge = settings.asaas_min_charge_brl;
    const effectiveCourtesyMax = resolveEffectiveCourtesyLimit(ten.max_printers, minCharge, pt);

    currentPlanBox.innerHTML = `
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div>
          <span class="text-[11px] font-bold uppercase text-slate-500 block">Assinatura Atual</span>
          <strong class="text-base font-black text-slate-900">${escapeHtml(activeTierLabel)}</strong>
          <span class="block text-xs text-slate-600 mt-0.5">
            ${billable.length} máquina(s) não isenta(s)${exempt.length > 0 ? ` • ${exempt.length} isenta(s) Fair Billing` : ''} • Status: <strong>${escapeHtml(st)}</strong>
            ${
              ten.pioneer_courtesy_active
                ? ` • <span class="text-emerald-700 font-bold">Cortesia Pioneiros Ativa (teto de ${effectiveCourtesyMax} máq — acima exige checkout real Asaas &gt;= ${formatCurrencyBrl(minCharge)})</span>`
                : ''
            }
          </span>
        </div>
        <div class="flex flex-wrap gap-2">
          ${st === 'active' && pt !== 'trial' ? `<button type="button" id="btnCancelSaasPlan" class="px-3.5 py-2 rounded-xl bg-rose-50 hover:bg-rose-100 text-rose-700 border border-rose-200 text-xs font-bold">Cancelar Assinatura</button>` : ''}
        </div>
      </div>`;

    byId('btnCancelSaasPlan')?.addEventListener('click', () => {
      void (async () => {
        if (!confirm('Deseja cancelar sua assinatura SaaS no Asaas? Sua conta voltará ao limite gratuito de 2 impressoras.')) {
          return;
        }
        const res = await cancelSaasSubscription(ten.id);
        alert(res.message);
        await loadTenantData();
        checkTrialPrinterLock();
        renderPlansView();
        plansCallbacks.onDataChanged();
      })();
    });
  }

  updatePricingSimulation();
}

export function updatePricingSimulation(): void {
  const ten = appState.tenant;
  const settings = appState.platformSettings;
  const slotsRemaining = Math.max(0, settings.pioneer_slots_total - settings.pioneer_slots_used);

  const simSlider = byId<HTMLInputElement>('simPrintersSlider');
  const simCountEl = byId('simPrintersCount');
  const simRateSel = byId<HTMLSelectElement>('simPlanSelect');
  const simTier = (simRateSel?.value ?? activeSelectedPlan) as Exclude<PlanTier, 'trial'>;

  const billableNow = getBillablePrintersSummary(appState.printers).billable.length;
  const simulatedCount = Math.max(1, Number(simSlider?.value || billableNow || 5));
  if (simCountEl) simCountEl.innerText = `${simulatedCount} impressora(s)`;

  const billableAboveFree = Math.max(0, simulatedCount - FREE_TRIAL_PRINTER_LIMIT);
  const tierRate = PLAN_CATALOG[simTier].rate;
  const courtesyMaxPrinters = computeCourtesyMaxPrinters(settings.asaas_min_charge_brl, tierRate);

  const quote = quotePlanLocally(
    simTier,
    Array.from({ length: billableAboveFree }, (_, i) => ({
      id: String(i),
      tenant_id: 'sim',
      model: 'Sim',
      serial: String(i),
    })),
    Boolean(ten?.is_pioneer_tenant),
    slotsRemaining,
    settings.asaas_min_charge_brl,
    {
      enforceMinPrepaidRecharge: settings.enforce_min_prepaid_recharge,
      minPrepaidRechargeBrl: settings.min_prepaid_recharge_brl,
    },
  );

  const simTotalEl = byId('simEstimatedMonthly');
  if (simTotalEl) {
    simTotalEl.innerText = formatCurrencyBrl(quote.rawAmount);
  }

  const starterNote = byId('simStarterRechargeNote');
  if (starterNote) {
    if (billableAboveFree === 0) {
      starterNote.className = 'p-3 rounded-2xl bg-emerald-50 border border-emerald-200 text-emerald-900 text-xs';
      starterNote.innerHTML = `<strong>Faixa Grátis Permanente (até ${FREE_TRIAL_PRINTER_LIMIT} máquinas = ${formatCurrencyBrl(0)}):</strong> Com ${simulatedCount} impressora(s) não isenta(s), você não paga nada nem consome vaga Pioneiros. A cobrança no Asaas começa apenas a partir da 3ª máquina.`;
    } else if (quote.willUsePioneerCourtesy) {
      starterNote.className = 'p-3 rounded-2xl bg-emerald-50 border border-emerald-200 text-emerald-900 text-xs';
      starterNote.innerHTML = `<strong>Cortesia Pioneiros Elegível (teto: até ${courtesyMaxPrinters} faturáveis neste plano):</strong> As ${FREE_TRIAL_PRINTER_LIMIT} primeiras são grátis + ${billableAboveFree} cobrável(is) = ${formatCurrencyBrl(quote.rawAmount)} (abaixo de ${formatCurrencyBrl(settings.asaas_min_charge_brl)}). Acima de ${courtesyMaxPrinters} máquinas, exige checkout real no Asaas (&gt;= ${formatCurrencyBrl(settings.asaas_min_charge_brl)}).`;
    } else if (quote.willUsePrepaidRecharge) {
      starterNote.className = 'p-3 rounded-2xl bg-indigo-50 border border-indigo-200 text-indigo-950 text-xs';
      starterNote.innerHTML = `<strong>Recarga Pré-Paga Mínima (${formatCurrencyBrl(quote.prepaidRechargeAmountBrl)}):</strong> Como ${billableAboveFree} máq cobrável(is) daria ${formatCurrencyBrl(quote.rawAmount)} (abaixo de R$ ${settings.asaas_min_charge_brl.toFixed(2)}), o checkout gera uma recarga pré-paga única de ${formatCurrencyBrl(quote.prepaidRechargeAmountBrl)} para crédito na sua conta.`;
    } else if (quote.belowMinimumBlocked) {
      starterNote.className = 'p-3 rounded-2xl bg-amber-50 border border-amber-200 text-amber-950 text-xs';
      starterNote.innerHTML = `<strong>Mínimo bancário do Asaas (R$ ${settings.asaas_min_charge_brl.toFixed(2)}):</strong> Cobranças menores que R$ ${settings.asaas_min_charge_brl.toFixed(2)} não são processadas pelo gateway (${FREE_TRIAL_PRINTER_LIMIT} grátis + ${billableAboveFree} cobrável(is) = ${formatCurrencyBrl(quote.rawAmount)}). Adicione mais máquinas ou selecione Gestão (R$ 4/máq) / Completo (R$ 6/máq).`;
    } else {
      starterNote.className = 'p-3 rounded-2xl bg-blue-50 border border-blue-200 text-[#0038A8] text-xs';
      starterNote.innerHTML = `<strong>Faturamento Asaas transparente:</strong> ${FREE_TRIAL_PRINTER_LIMIT} máquinas grátis + ${billableAboveFree} cobrável(is) x ${formatCurrencyBrl(quote.rate)} = ${formatCurrencyBrl(quote.rawAmount)}/mês via checkout real Asaas.`;
    }
  }
}

export async function startPlanCheckout(
  planTier: Exclude<PlanTier, 'trial'>,
  billingType: 'PIX' | 'BOLETO' | 'CREDIT_CARD' = 'PIX',
): Promise<void> {
  const ten = appState.tenant;
  if (!ten) return;
  activeSelectedPlan = planTier;

  const feedbackBox = byId('checkoutResultModalBody');
  if (feedbackBox) {
    feedbackBox.innerHTML = `<div class="py-8 text-center text-xs text-slate-500">Consultando cotação real e iniciando checkout seguro no Asaas...</div>`;
  }
  openModal('checkoutResultModal');

  try {
    const quote = await quoteSaasPlan(ten.id, planTier);
    const checkout = await createSaasCheckout({ tenantId: ten.id, planTier, billingType });

    if (!feedbackBox) return;

    if (checkout.status === 'free_tier' || checkout.status === 'pioneer_courtesy') {
      const heading =
        checkout.status === 'free_tier'
          ? '✓ Faixa Grátis Permanente (R$ 0,00)'
          : '✓ Cortesia Pioneiros Ativada!';
      feedbackBox.innerHTML = `
        <div class="p-5 rounded-2xl bg-emerald-50 border border-emerald-200 text-emerald-950 space-y-2 text-xs">
          <strong class="text-sm font-black block">${heading}</strong>
          <p>${escapeHtml(checkout.message ?? '')}</p>
        </div>`;
      await loadTenantData();
      checkTrialPrinterLock();
      renderPlansView();
      plansCallbacks.onDataChanged();
      return;
    }

    if (checkout.status === 'error') {
      const needsDoc = checkout.code === 'CNPJ_CPF_REQUIRED';
      feedbackBox.innerHTML = `
        <div class="p-5 rounded-2xl bg-rose-50 border border-rose-200 text-rose-900 space-y-3 text-xs">
          <strong class="text-sm font-black block">Não foi possível iniciar a cobrança</strong>
          <p>${escapeHtml(checkout.message ?? 'Verifique os dados da empresa.')}</p>
          ${
            needsDoc
              ? `<button type="button" id="btnCheckoutGoCompany" class="px-4 py-2 rounded-xl bg-slate-900 text-white font-bold">Ir para Minha Empresa</button>`
              : ''
          }
        </div>`;
      byId('btnCheckoutGoCompany')?.addEventListener('click', () => {
        closeModal('checkoutResultModal');
        switchTab('company');
      });
      return;
    }

    // Pending payment: NEVER falsely activate plan before asaas-webhook confirms payment!
    const qrSrc = checkout.pix_qr_base64
      ? `data:image/png;base64,${checkout.pix_qr_base64}`
      : checkout.pix_payload
        ? await renderQrDataUrl(checkout.pix_payload, 190)
        : '';

    const checkoutTitle = checkout.is_prepaid_recharge
      ? `Recarga Pré-Paga Gerada — Aguardando Pagamento (${formatCurrencyBrl(checkout.charged_amount ?? quote.charged_amount ?? quote.raw_amount)})`
      : `Assinatura Gerada — Aguardando Pagamento (${formatCurrencyBrl(checkout.charged_amount ?? quote.raw_amount)})`;
    feedbackBox.innerHTML = `
      <div class="space-y-4 text-xs text-slate-700">
        <div class="p-4 rounded-2xl bg-blue-50 border border-blue-200 space-y-1">
          <strong class="text-sm font-black text-[#0038A8] block">${checkoutTitle}</strong>
          <p>Seu plano <strong>${escapeHtml(PLAN_CATALOG[planTier].name)}</strong> para ${quote.billable_printers} máquina(s) será liberado automaticamente assim que o webhook do Asaas confirmar o pagamento.</p>
        </div>
        ${qrSrc ? `<div class="text-center"><img src="${qrSrc}" alt="QR Code PIX Asaas" class="w-44 h-44 mx-auto rounded-2xl border border-slate-200 p-2 bg-white" /></div>` : ''}
        ${
          checkout.pix_payload
            ? `<div class="space-y-1.5">
                <label class="text-[11px] font-bold text-slate-600 block">PIX Copia e Cola Oficial Asaas</label>
                <div class="flex gap-2">
                  <input id="checkoutPixCopyInput" readonly value="${escapeHtml(checkout.pix_payload)}" class="flex-1 px-3 py-2 rounded-xl border border-slate-300 bg-slate-50 mono text-[11px]" />
                  <button type="button" id="btnCopyCheckoutPix" class="px-3.5 py-2 rounded-xl bg-[#0038A8] text-white font-bold">Copiar</button>
                </div>
              </div>`
            : ''
        }
        ${
          checkout.invoice_url
            ? `<a href="${escapeHtml(checkout.invoice_url)}" target="_blank" rel="noopener" class="block w-full py-2.5 rounded-xl bg-slate-900 hover:bg-slate-800 text-white text-center font-black">Abrir Fatura Completa no Asaas</a>`
            : ''
        }
      </div>`;

    byId('btnCopyCheckoutPix')?.addEventListener('click', () => {
      const val = byId<HTMLInputElement>('checkoutPixCopyInput')?.value ?? '';
      void navigator.clipboard?.writeText(val);
      alert('Código PIX Asaas copiado!');
    });

    await loadTenantData();
    checkTrialPrinterLock();
    renderPlansView();
  } catch (err) {
    if (feedbackBox) {
      feedbackBox.innerHTML = `<div class="p-4 rounded-2xl bg-rose-50 border border-rose-200 text-rose-800 text-xs font-bold">${escapeHtml(err instanceof Error ? err.message : String(err))}</div>`;
    }
  }
}

export function openPricingModal(): void {
  renderPlansView();
  openModal('pricingModal');
}

export function initPlansView(callbacks: PlansCallbacks): void {
  plansCallbacks = callbacks;

  byId('simPrintersSlider')?.addEventListener('input', () => updatePricingSimulation());
  byId('simPlanSelect')?.addEventListener('change', () => updatePricingSimulation());

  document.querySelectorAll<HTMLElement>('[data-select-plan]').forEach(btn => {
    btn.addEventListener('click', () => {
      const tier = (btn.dataset.selectPlan ?? 'pro') as Exclude<PlanTier, 'trial'>;
      const bType = (byId<HTMLSelectElement>('checkoutBillingTypeSelect')?.value ?? 'PIX') as
        | 'PIX'
        | 'BOLETO'
        | 'CREDIT_CARD';
      closeModal('pricingModal');
      void startPlanCheckout(tier, bType);
    });
  });

  byId('btnOverlayGoPlans')?.addEventListener('click', () => switchTab('plans'));
  byId('btnUpgradeFromBanner')?.addEventListener('click', () => openPricingModal());
}
