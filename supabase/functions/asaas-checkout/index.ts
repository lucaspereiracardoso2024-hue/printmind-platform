import { createClient, SupabaseClient } from "https://esm.sh/@supabase/supabase-js@2";

const MASTER_ADMIN_EMAIL = "lucaspereiracardoso2024@gmail.com";

const corsHeaders: Record<string, string> = {
  "Access-Control-Allow-Origin": "*",
  "Access-Control-Allow-Headers": "authorization, x-client-info, apikey, content-type",
  "Access-Control-Allow-Methods": "POST, OPTIONS",
};

type PlanTier = "starter" | "pro" | "enterprise";
type BillingType = "PIX" | "CREDIT_CARD" | "BOLETO";

interface CheckoutRequestBody {
  action?: "quote_plan" | "create_saas_checkout" | "cancel_saas_subscription" | "emit_split_charge";
  tenant_id?: string;
  plan_tier?: string;
  billing_type?: string;
  client_id?: string;
  closing_id?: string;
  client_name?: string;
  total_amount?: number;
  due_date?: string;
  description?: string;
}

interface TenantRecord {
  id: string;
  trade_name: string;
  legal_name: string | null;
  cnpj_cpf: string | null;
  email: string;
  phone: string | null;
  plan_tier: string;
  subscription_status: string;
  gateway_customer_id: string | null;
  gateway_subscription_id: string | null;
  baas_wallet_id: string | null;
  baas_onboarding_status: string | null;
  pix_key: string | null;
  is_pioneer_tenant: boolean;
  pioneer_courtesy_active: boolean;
}

interface PlatformSettingsRecord {
  id: string;
  pioneer_slots_total: number;
  pioneer_slots_used: number;
  enforce_min_prepaid_recharge: boolean;
  min_prepaid_recharge_brl: number;
  asaas_min_charge_brl: number;
}

interface AsaasCustomerResponse {
  id: string;
  name?: string;
}

interface AsaasSubscriptionResponse {
  id: string;
  status?: string;
  invoiceUrl?: string;
}

interface AsaasPaymentItem {
  id: string;
  status?: string;
  value?: number;
  invoiceUrl?: string;
  bankSlipUrl?: string;
}

interface AsaasPixQrResponse {
  encodedImage?: string;
  payload?: string;
  expirationDate?: string;
}

const PLAN_RATES: Record<PlanTier, number> = {
  starter: 1.0,
  pro: 4.0,
  enterprise: 6.0,
};

function jsonResponse(body: Record<string, unknown>, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      ...corsHeaders,
      "Content-Type": "application/json; charset=utf-8",
    },
  });
}

function normalizePlanTier(raw?: string): PlanTier {
  const s = (raw ?? "starter").toLowerCase().trim();
  if (s === "enterprise" || s.includes("6")) return "enterprise";
  if (s === "pro" || s.includes("4")) return "pro";
  return "starter";
}

function normalizeBillingType(raw?: string): BillingType {
  const s = (raw ?? "PIX").toUpperCase().trim();
  if (s === "CREDIT_CARD" || s === "BOLETO") return s;
  return "PIX";
}

function cleanDigits(raw: string | null | undefined): string {
  return (raw ?? "").replace(/\D+/g, "");
}

function getAsaasBaseUrl(): string {
  const env = (Deno.env.get("ASAAS_ENV") ?? "production").toLowerCase().trim();
  return env === "sandbox" ? "https://sandbox.asaas.com/api/v3" : "https://api.asaas.com/api/v3";
}

function todayIsoSaoPaulo(addDays = 0): string {
  const now = new Date(Date.now() - 3 * 3600 * 1000 + addDays * 86400 * 1000);
  return now.toISOString().slice(0, 10);
}

async function asaasFetch<T>(
  apiKey: string,
  path: string,
  init: RequestInit = {},
): Promise<{ ok: boolean; status: number; data: T | null; errorText: string }> {
  const baseUrl = getAsaasBaseUrl();
  const resp = await fetch(`${baseUrl}${path}`, {
    ...init,
    signal: AbortSignal.timeout(12000),
    headers: {
      "Content-Type": "application/json",
      "access_token": apiKey,
      "User-Agent": "PrintMind-SaaS/5.0",
      ...(init.headers ?? {}),
    },
  });
  const text = await resp.text();
  if (!resp.ok) {
    return { ok: false, status: resp.status, data: null, errorText: text.slice(0, 500) };
  }
  try {
    return { ok: true, status: resp.status, data: JSON.parse(text) as T, errorText: "" };
  } catch {
    return { ok: false, status: 502, data: null, errorText: "Invalid JSON from Asaas" };
  }
}

const FREE_TIER_PRINTERS = 2;

interface PrinterCountRow {
  id: string;
  billing_exempt: boolean | null;
  billing_mode: string | null;
}

async function countPrinterTierUsage(
  adminClient: SupabaseClient,
  tenantId: string,
): Promise<{ nonExemptPrinters: number; billablePrinters: number }> {
  const { data, error } = await adminClient
    .from("printers")
    .select("id, billing_exempt, billing_mode")
    .eq("tenant_id", tenantId);

  if (error) {
    throw new Error(`Falha ao contar impressoras faturáveis: ${error.message}`);
  }

  const rows = (data ?? []) as PrinterCountRow[];
  const nonExemptPrinters = rows.filter(row => {
    if (row.billing_exempt) return false;
    const mode = String(row.billing_mode ?? "inherit_client").toLowerCase().trim();
    return mode !== "exempt" && mode !== "billing_exempt";
  }).length;

  return {
    nonExemptPrinters,
    billablePrinters: Math.max(0, nonExemptPrinters - FREE_TIER_PRINTERS),
  };
}

function computeCourtesyMaxPrinters(asaasMinChargeBrl: number, unitPrice: number): number {
  const rateCents = Math.round(Math.max(0, unitPrice) * 100);
  if (rateCents <= 0) return FREE_TIER_PRINTERS;
  const minChargeCents = Math.round(Math.max(0, asaasMinChargeBrl) * 100);
  const maxBillableUnderMin =
    minChargeCents <= 0 ? 0 : Math.floor((minChargeCents - 1) / rateCents);
  return FREE_TIER_PRINTERS + Math.max(0, maxBillableUnderMin);
}

async function getPlatformSettings(adminClient: SupabaseClient): Promise<PlatformSettingsRecord> {
  const { data } = await adminClient
    .from("platform_settings")
    .select("*")
    .eq("id", "default")
    .maybeSingle();

  return {
    id: "default",
    pioneer_slots_total: Number(data?.pioneer_slots_total ?? 15),
    pioneer_slots_used: Number(data?.pioneer_slots_used ?? 0),
    enforce_min_prepaid_recharge: Boolean(data?.enforce_min_prepaid_recharge ?? false),
    min_prepaid_recharge_brl: Number(data?.min_prepaid_recharge_brl ?? 30),
    asaas_min_charge_brl: Number(data?.asaas_min_charge_brl ?? 5),
  };
}

Deno.serve(async (req: Request): Promise<Response> => {
  if (req.method === "OPTIONS") {
    return new Response("ok", { headers: corsHeaders });
  }
  if (req.method !== "POST") {
    return jsonResponse({ status: "error", code: "METHOD_NOT_ALLOWED", message: "Use POST." }, 405);
  }

  const supabaseUrl = Deno.env.get("SUPABASE_URL");
  const supabaseAnonKey = Deno.env.get("SUPABASE_ANON_KEY");
  const serviceRoleKey = Deno.env.get("SUPABASE_SERVICE_ROLE_KEY");
  if (!supabaseUrl || !supabaseAnonKey || !serviceRoleKey) {
    return jsonResponse(
      { status: "error", code: "ENV_MISSING", message: "Configuração do Supabase ausente." },
      500,
    );
  }

  const authHeader = req.headers.get("Authorization");
  if (!authHeader || !authHeader.toLowerCase().startsWith("bearer ")) {
    return jsonResponse(
      { status: "error", code: "UNAUTHORIZED", message: "Authorization Bearer JWT obrigatório." },
      401,
    );
  }

  const userClient = createClient(supabaseUrl, supabaseAnonKey, {
    global: { headers: { Authorization: authHeader } },
    auth: { persistSession: false, autoRefreshToken: false },
  });

  const { data: authData, error: authErr } = await userClient.auth.getUser();
  if (authErr || !authData.user) {
    return jsonResponse({ status: "error", code: "INVALID_JWT", message: "Sessão inválida." }, 401);
  }

  let body: CheckoutRequestBody;
  try {
    body = (await req.json()) as CheckoutRequestBody;
  } catch {
    return jsonResponse({ status: "error", code: "INVALID_JSON", message: "JSON inválido." }, 400);
  }

  const adminClient = createClient(supabaseUrl, serviceRoleKey, {
    auth: { persistSession: false, autoRefreshToken: false },
  });

  const isMasterAdmin = (authData.user.email ?? "").toLowerCase().trim() === MASTER_ADMIN_EMAIL;
  const { data: profile } = await adminClient
    .from("profiles")
    .select("tenant_id, role")
    .eq("id", authData.user.id)
    .maybeSingle();

  const targetTenantId = body.tenant_id?.trim() || (profile?.tenant_id as string | undefined);
  if (!targetTenantId) {
    return jsonResponse({ status: "error", code: "NO_TENANT", message: "Tenant não identificado." }, 400);
  }
  if (!isMasterAdmin && profile?.tenant_id !== targetTenantId) {
    return jsonResponse({ status: "error", code: "FORBIDDEN", message: "Acesso negado ao tenant." }, 403);
  }

  const { data: tenantData, error: tenantErr } = await adminClient
    .from("tenants")
    .select("id, trade_name, legal_name, cnpj_cpf, email, phone, plan_tier, subscription_status, gateway_customer_id, gateway_subscription_id, baas_wallet_id, baas_onboarding_status, pix_key, is_pioneer_tenant, pioneer_courtesy_active")
    .eq("id", targetTenantId)
    .single();

  if (tenantErr || !tenantData) {
    return jsonResponse({ status: "error", code: "TENANT_NOT_FOUND", message: "Empresa não encontrada." }, 404);
  }
  const tenant = tenantData as TenantRecord;
  const settings = await getPlatformSettings(adminClient);
  const action = body.action ?? "create_saas_checkout";

  if (action === "quote_plan") {
    const planTier = normalizePlanTier(body.plan_tier);
    const rate = PLAN_RATES[planTier];
    const { nonExemptPrinters, billablePrinters } = await countPrinterTierUsage(adminClient, tenant.id);
    const courtesyMaxPrinters = computeCourtesyMaxPrinters(settings.asaas_min_charge_brl, rate);
    const rawAmountCents = Math.round(billablePrinters * Math.round(rate * 100));
    const minChargeCents = Math.round(settings.asaas_min_charge_brl * 100);
    const rawAmount = rawAmountCents / 100;
    const slotsRemaining = Math.max(0, settings.pioneer_slots_total - settings.pioneer_slots_used);
    const pioneerEligible = tenant.is_pioneer_tenant || slotsRemaining > 0;
    const isFreeTier = billablePrinters === 0;
    const belowMin = billablePrinters > 0 && rawAmountCents < minChargeCents;
    const usePrepaid =
      belowMin &&
      !pioneerEligible &&
      settings.enforce_min_prepaid_recharge &&
      Math.round(settings.min_prepaid_recharge_brl * 100) >= minChargeCents;
    const chargedAmount = isFreeTier
      ? 0
      : belowMin && pioneerEligible
        ? 0
        : usePrepaid
          ? Number(settings.min_prepaid_recharge_brl.toFixed(2))
          : rawAmount;

    return jsonResponse({
      status: "success",
      plan_tier: planTier,
      rate_per_printer: rate,
      non_exempt_printers: nonExemptPrinters,
      free_tier_printers: FREE_TIER_PRINTERS,
      billable_printers: billablePrinters,
      courtesy_max_printers: courtesyMaxPrinters,
      raw_amount: rawAmount,
      charged_amount: chargedAmount,
      asaas_min_charge_brl: settings.asaas_min_charge_brl,
      enforce_min_prepaid_recharge: settings.enforce_min_prepaid_recharge,
      min_prepaid_recharge_brl: settings.min_prepaid_recharge_brl,
      pioneer_slots_remaining: slotsRemaining,
      pioneer_eligible: pioneerEligible,
      will_use_pioneer_courtesy: belowMin && pioneerEligible,
      will_use_prepaid_recharge: usePrepaid,
      prepaid_recharge_amount_brl: usePrepaid ? chargedAmount : 0,
      below_minimum_blocked: belowMin && !pioneerEligible && !usePrepaid,
    });
  }

  if (action === "cancel_saas_subscription") {
    const asaasKey = Deno.env.get("ASAAS_API_KEY")?.trim() ?? "";
    if (tenant.gateway_subscription_id && asaasKey) {
      const delSubRes = await asaasFetch(asaasKey, `/subscriptions/${tenant.gateway_subscription_id}`, {
        method: "DELETE",
      });
      if (!delSubRes.ok) {
        console.warn(`Asaas cancel subscription warning (${delSubRes.status}): ${delSubRes.errorText}`);
      }
    }

    const { error: cancelErr } = await adminClient
      .from("tenants")
      .update({
        subscription_status: "canceled",
        plan_tier: "trial",
        max_printers: 2,
        pioneer_courtesy_active: false,
        gateway_subscription_id: null,
        updated_at: new Date().toISOString(),
      })
      .eq("id", tenant.id);

    if (cancelErr) {
      return jsonResponse({ status: "error", code: "CANCEL_FAILED", message: cancelErr.message }, 500);
    }

    return jsonResponse({
      status: "canceled",
      message: "Assinatura cancelada com sucesso no gateway e conta retornada ao modo gratuito (até 2 máquinas).",
    });
  }

  if (action === "emit_split_charge") {
    const walletId = (tenant.baas_wallet_id ?? "").trim();
    if (tenant.baas_onboarding_status !== "approved" || !walletId) {
      return jsonResponse(
        {
          status: "error",
          code: "BAAS_NOT_APPROVED",
          message: "Conclua e aprove sua subconta/carteira Asaas BaaS em 'Minha Empresa' antes de emitir cobranças com split.",
        },
        400,
      );
    }
    const asaasKey = Deno.env.get("ASAAS_API_KEY")?.trim() ?? "";
    if (!asaasKey) {
      return jsonResponse(
        { status: "error", code: "ASAAS_NOT_CONFIGURED", message: "Chave ASAAS_API_KEY não configurada no servidor." },
        503,
      );
    }
    const totalAmount = Number((body.total_amount ?? 0).toFixed(2));
    if (totalAmount < settings.asaas_min_charge_brl) {
      return jsonResponse(
        {
          status: "error",
          code: "BELOW_ASAAS_MINIMUM",
          message: `O Asaas exige cobrança mínima de R$ ${settings.asaas_min_charge_brl.toFixed(2)}. Valor atual: R$ ${totalAmount.toFixed(2)}.`,
        },
        400,
      );
    }
    if (!body.client_id) {
      return jsonResponse({ status: "error", code: "CLIENT_REQUIRED", message: "client_id obrigatório." }, 400);
    }

    const { data: endClient, error: clientErr } = await adminClient
      .from("clients")
      .select("id, name, cnpj, email_billing, phone, baas_customer_id")
      .eq("id", body.client_id)
      .eq("tenant_id", tenant.id)
      .single();

    if (clientErr || !endClient) {
      return jsonResponse({ status: "error", code: "CLIENT_NOT_FOUND", message: "Cliente não encontrado." }, 404);
    }

    let asaasClientId = String(endClient.baas_customer_id ?? "").trim();
    if (!asaasClientId) {
      const cnpjDigits = cleanDigits(endClient.cnpj);
      if (cnpjDigits.length !== 11 && cnpjDigits.length !== 14) {
        return jsonResponse(
          { status: "error", code: "CLIENT_DOCUMENT_REQUIRED", message: "Cadastre um CPF ou CNPJ válido para este cliente antes de emitir boleto/PIX via Asaas." },
          400,
        );
      }
      const createdCust = await asaasFetch<AsaasCustomerResponse>(asaasKey, "/customers", {
        method: "POST",
        body: JSON.stringify({
          name: endClient.name,
          cpfCnpj: cnpjDigits,
          email: endClient.email_billing || tenant.email,
          mobilePhone: cleanDigits(endClient.phone) || undefined,
          externalReference: `client:${endClient.id}`,
        }),
      });
      if (!createdCust.ok || !createdCust.data?.id) {
        return jsonResponse(
          { status: "error", code: "ASAAS_CUSTOMER_ERROR", message: "Erro ao registrar cliente no Asaas.", details: createdCust.errorText },
          502,
        );
      }
      asaasClientId = createdCust.data.id;
      const { error: custLinkErr } = await adminClient
        .from("clients")
        .update({ baas_customer_id: asaasClientId })
        .eq("id", endClient.id);
      if (custLinkErr) {
        console.warn(`Failed caching baas_customer_id for client ${endClient.id}: ${custLinkErr.message}`);
      }
    }

    const paymentBody: Record<string, unknown> = {
      customer: asaasClientId,
      billingType: normalizeBillingType(body.billing_type),
      value: totalAmount,
      dueDate: body.due_date || todayIsoSaoPaulo(5),
      description: body.description || `Fatura de Outsourcing PrintMind — ${endClient.name}`,
      externalReference: body.closing_id ? `closing:${body.closing_id}:tenant:${tenant.id}` : `client:${endClient.id}:tenant:${tenant.id}`,
      split: [
        {
          walletId,
          percentualValue: 98.5,
        },
      ],
    };

    const paymentRes = await asaasFetch<AsaasPaymentItem>(asaasKey, "/payments", {
      method: "POST",
      body: JSON.stringify(paymentBody),
    });
    if (!paymentRes.ok || !paymentRes.data?.id) {
      return jsonResponse(
        { status: "error", code: "ASAAS_PAYMENT_ERROR", message: "Falha ao criar cobrança no Asaas.", details: paymentRes.errorText },
        502,
      );
    }

    const pixRes = await asaasFetch<AsaasPixQrResponse>(asaasKey, `/payments/${paymentRes.data.id}/pixQrCode`);
    if (body.closing_id) {
      const { error: closingUpdErr } = await adminClient
        .from("closings")
        .update({
          asaas_payment_id: paymentRes.data.id,
          invoice_url: paymentRes.data.invoiceUrl ?? paymentRes.data.bankSlipUrl ?? null,
          pix_payload: pixRes.data?.payload ?? null,
          status: "faturado",
          updated_at: new Date().toISOString(),
        })
        .eq("id", body.closing_id)
        .eq("tenant_id", tenant.id);
      if (closingUpdErr) {
        console.error(`Failed linking Asaas payment ${paymentRes.data.id} to closing ${body.closing_id}: ${closingUpdErr.message}`);
      }
    }

    return jsonResponse({
      status: "created",
      payment_id: paymentRes.data.id,
      invoice_url: paymentRes.data.invoiceUrl ?? paymentRes.data.bankSlipUrl ?? null,
      pix_payload: pixRes.data?.payload ?? null,
      pix_qr_base64: pixRes.data?.encodedImage ?? null,
      charged_amount: totalAmount,
    });
  }

  // Default action: create_saas_checkout
  const planTier = normalizePlanTier(body.plan_tier);
  const billingType = normalizeBillingType(body.billing_type);
  const rate = PLAN_RATES[planTier];
  const { nonExemptPrinters, billablePrinters } = await countPrinterTierUsage(adminClient, tenant.id);
  const courtesyMaxPrinters = computeCourtesyMaxPrinters(settings.asaas_min_charge_brl, rate);
  const rawAmountCents = Math.round(billablePrinters * Math.round(rate * 100));
  const minChargeCents = Math.round(settings.asaas_min_charge_brl * 100);
  const rawAmount = rawAmountCents / 100;
  const slotsRemaining = Math.max(0, settings.pioneer_slots_total - settings.pioneer_slots_used);
  const pioneerEligible = tenant.is_pioneer_tenant || slotsRemaining > 0;

  if (billablePrinters === 0) {
    return jsonResponse({
      status: "free_tier",
      plan_tier: "trial",
      non_exempt_printers: nonExemptPrinters,
      billable_printers: 0,
      max_printers: FREE_TIER_PRINTERS,
      courtesy_max_printers: courtesyMaxPrinters,
      raw_amount: 0,
      charged_amount: 0,
      message: `Você possui ${nonExemptPrinters} máquina(s) não isenta(s), dentro da faixa grátis permanente (até ${FREE_TIER_PRINTERS} impressoras = R$ 0,00). Cadastre a 3ª máquina para contratar um plano pago ou cortesia Pioneiros.`,
    });
  }

  const belowMin = rawAmountCents < minChargeCents;
  const usePrepaidRecharge =
    belowMin &&
    !pioneerEligible &&
    settings.enforce_min_prepaid_recharge &&
    Math.round(settings.min_prepaid_recharge_brl * 100) >= minChargeCents;

  if (belowMin && !usePrepaidRecharge) {
    if (pioneerEligible) {
      if (!tenant.is_pioneer_tenant) {
        const { error: slotErr } = await adminClient
          .from("platform_settings")
          .update({ pioneer_slots_used: settings.pioneer_slots_used + 1, updated_at: new Date().toISOString() })
          .eq("id", "default");
        if (slotErr) {
          return jsonResponse(
            { status: "error", code: "PIONEER_SLOT_UPDATE_FAILED", message: slotErr.message },
            500,
          );
        }
      }
      const { error: courtesyErr } = await adminClient
        .from("tenants")
        .update({
          plan_tier: planTier,
          subscription_status: "active",
          max_printers: courtesyMaxPrinters,
          is_pioneer_tenant: true,
          pioneer_courtesy_active: true,
          updated_at: new Date().toISOString(),
        })
        .eq("id", tenant.id);

      if (courtesyErr) {
        return jsonResponse(
          { status: "error", code: "TENANT_UPDATE_FAILED", message: courtesyErr.message },
          500,
        );
      }

      const maxBillableInCourtesy = Math.max(0, courtesyMaxPrinters - FREE_TIER_PRINTERS);
      return jsonResponse({
        status: "pioneer_courtesy",
        plan_tier: planTier,
        non_exempt_printers: nonExemptPrinters,
        billable_printers: billablePrinters,
        max_printers: courtesyMaxPrinters,
        courtesy_max_printers: courtesyMaxPrinters,
        raw_amount: rawAmount,
        charged_amount: 0,
        message: `Cortesia Pioneiros ativada no plano ${planTier.toUpperCase()} até ${courtesyMaxPrinters} máquinas (${FREE_TIER_PRINTERS} grátis + até ${maxBillableInCourtesy} cobrável(is) = R$ ${rawAmount.toFixed(2)} < R$ ${settings.asaas_min_charge_brl.toFixed(2)}). Ao ultrapassar ${courtesyMaxPrinters} máquinas, será necessário checkout real no Asaas (>= R$ ${settings.asaas_min_charge_brl.toFixed(2)}).`,
      });
    }

    return jsonResponse(
      {
        status: "error",
        code: "BELOW_ASAAS_MINIMUM",
        raw_amount: rawAmount,
        min_charge_brl: settings.asaas_min_charge_brl,
        non_exempt_printers: nonExemptPrinters,
        billable_printers: billablePrinters,
        message: `O gateway Asaas não processa cobranças abaixo de R$ ${settings.asaas_min_charge_brl.toFixed(2)} (seu plano atual daria R$ ${rawAmount.toFixed(2)} para ${billablePrinters} máquina(s) cobrável(is) além das ${FREE_TIER_PRINTERS} grátis). Selecione o plano Gestão/Completo ou conecte mais impressoras.`,
      },
      400,
    );
  }

  const asaasKey = Deno.env.get("ASAAS_API_KEY")?.trim() ?? "";
  if (!asaasKey) {
    return jsonResponse(
      {
        status: "error",
        code: "ASAAS_NOT_CONFIGURED",
        message: "O segredo ASAAS_API_KEY não está configurado nas Supabase Edge Functions.",
      },
      503,
    );
  }

  let gatewayCustomerId = (tenant.gateway_customer_id ?? "").trim();
  if (!gatewayCustomerId) {
    const docDigits = cleanDigits(tenant.cnpj_cpf);
    if (docDigits.length !== 11 && docDigits.length !== 14) {
      return jsonResponse(
        {
          status: "error",
          code: "CNPJ_CPF_REQUIRED",
          message: "Cadastre o CNPJ ou CPF válido em 'Minha Empresa' antes de gerar a assinatura no Asaas.",
        },
        400,
      );
    }

    const custRes = await asaasFetch<AsaasCustomerResponse>(asaasKey, "/customers", {
      method: "POST",
      body: JSON.stringify({
        name: tenant.legal_name || tenant.trade_name,
        cpfCnpj: docDigits,
        email: tenant.email,
        mobilePhone: cleanDigits(tenant.phone) || undefined,
        externalReference: `tenant:${tenant.id}`,
      }),
    });
    if (!custRes.ok || !custRes.data?.id) {
      return jsonResponse(
        { status: "error", code: "ASAAS_CUSTOMER_ERROR", message: "Erro ao criar cliente no Asaas.", details: custRes.errorText },
        502,
      );
    }
    gatewayCustomerId = custRes.data.id;
  }

  // Cancel superseded subscription if switching plans cleanly
  if (tenant.gateway_subscription_id) {
    const prevCancelRes = await asaasFetch(asaasKey, `/subscriptions/${tenant.gateway_subscription_id}`, {
      method: "DELETE",
    });
    if (!prevCancelRes.ok) {
      console.warn(`Previous Asaas subscription delete warning (${prevCancelRes.status}): ${prevCancelRes.errorText}`);
    }
  }

  if (usePrepaidRecharge) {
    const rechargeAmount = Number(settings.min_prepaid_recharge_brl.toFixed(2));
    const externalRef = `tenant:${tenant.id}:plan:${planTier}:prepaid:${rechargeAmount.toFixed(2)}`;
    const payRes = await asaasFetch<AsaasPaymentItem>(asaasKey, "/payments", {
      method: "POST",
      body: JSON.stringify({
        customer: gatewayCustomerId,
        billingType,
        value: rechargeAmount,
        dueDate: todayIsoSaoPaulo(1),
        description: `PrintMind SaaS — Recarga Pré-Paga Mínima (Plano ${planTier.toUpperCase()})`,
        externalReference: externalRef,
      }),
    });
    if (!payRes.ok || !payRes.data?.id) {
      return jsonResponse(
        { status: "error", code: "ASAAS_PAYMENT_ERROR", message: "Falha ao gerar recarga pré-paga no Asaas.", details: payRes.errorText },
        502,
      );
    }

    let pixData: AsaasPixQrResponse | null = null;
    if (billingType === "PIX") {
      const qrRes = await asaasFetch<AsaasPixQrResponse>(asaasKey, `/payments/${payRes.data.id}/pixQrCode`);
      if (qrRes.ok) {
        pixData = qrRes.data;
      }
    }

    const { error: updateErr } = await adminClient
      .from("tenants")
      .update({
        gateway_customer_id: gatewayCustomerId,
        gateway_subscription_id: null,
        subscription_status: "pending_payment",
        pioneer_courtesy_active: false,
        updated_at: new Date().toISOString(),
      })
      .eq("id", tenant.id);

    if (updateErr) {
      return jsonResponse({ status: "error", code: "TENANT_UPDATE_FAILED", message: updateErr.message }, 500);
    }

    return jsonResponse({
      status: "pending_payment",
      plan_tier: planTier,
      billable_printers: billablePrinters,
      raw_amount: rawAmount,
      charged_amount: rechargeAmount,
      is_prepaid_recharge: true,
      subscription_id: null,
      payment_id: payRes.data.id,
      invoice_url: payRes.data.invoiceUrl ?? payRes.data.bankSlipUrl ?? null,
      pix_payload: pixData?.payload ?? null,
      pix_qr_base64: pixData?.encodedImage ?? null,
      message: `Recarga pré-paga mínima de R$ ${rechargeAmount.toFixed(2)} gerada! O saldo ficará na sua conta e o plano será ativado após confirmação do Asaas.`,
    });
  }

  const externalRef = `tenant:${tenant.id}:plan:${planTier}`;
  const subRes = await asaasFetch<AsaasSubscriptionResponse>(asaasKey, "/subscriptions", {
    method: "POST",
    body: JSON.stringify({
      customer: gatewayCustomerId,
      billingType,
      value: rawAmount,
      nextDueDate: todayIsoSaoPaulo(0),
      cycle: "MONTHLY",
      description: `PrintMind SaaS — Plano ${planTier.toUpperCase()} (${billablePrinters} impressora(s) faturável(is))`,
      externalReference: externalRef,
    }),
  });

  if (!subRes.ok || !subRes.data?.id) {
    return jsonResponse(
      { status: "error", code: "ASAAS_SUBSCRIPTION_ERROR", message: "Falha ao criar assinatura no Asaas.", details: subRes.errorText },
      502,
    );
  }

  const subId = subRes.data.id;
  const listRes = await asaasFetch<{ data?: AsaasPaymentItem[] }>(
    asaasKey,
    `/subscriptions/${subId}/payments?limit=1`,
  );
  const firstPayment = listRes.data?.data?.[0];

  let pixData: AsaasPixQrResponse | null = null;
  if (firstPayment?.id && billingType === "PIX") {
    const qrRes = await asaasFetch<AsaasPixQrResponse>(asaasKey, `/payments/${firstPayment.id}/pixQrCode`);
    if (qrRes.ok) {
      pixData = qrRes.data;
    }
  }

  // Keep subscription in pending_payment until asaas-webhook receives PAYMENT_CONFIRMED / PAYMENT_RECEIVED.
  const { error: updateErr } = await adminClient
    .from("tenants")
    .update({
      gateway_customer_id: gatewayCustomerId,
      gateway_subscription_id: subId,
      subscription_status: "pending_payment",
      pioneer_courtesy_active: false,
      updated_at: new Date().toISOString(),
    })
    .eq("id", tenant.id);

  if (updateErr) {
    return jsonResponse({ status: "error", code: "TENANT_UPDATE_FAILED", message: updateErr.message }, 500);
  }

  return jsonResponse({
    status: "pending_payment",
    plan_tier: planTier,
    billable_printers: billablePrinters,
    charged_amount: rawAmount,
    subscription_id: subId,
    payment_id: firstPayment?.id ?? null,
    invoice_url: firstPayment?.invoiceUrl ?? firstPayment?.bankSlipUrl ?? null,
    pix_payload: pixData?.payload ?? null,
    pix_qr_base64: pixData?.encodedImage ?? null,
    message: "Cobrança gerada! Seu plano será ativado automaticamente assim que o pagamento for confirmado pelo Asaas.",
  });
});
