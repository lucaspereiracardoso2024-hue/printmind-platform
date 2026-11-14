// Deploy with: supabase functions deploy asaas-webhook --no-verify-jwt
import { createClient } from "https://esm.sh/@supabase/supabase-js@2";

interface AsaasWebhookPayload {
  id?: string;
  event?: string;
  payment?: {
    id?: string;
    subscription?: string;
    customer?: string;
    externalReference?: string;
    value?: number;
    status?: string;
  };
}

function jsonResponse(body: Record<string, unknown>, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json; charset=utf-8" },
  });
}

async function constantTimeEqual(a: string, b: string): Promise<boolean> {
  const enc = new TextEncoder();
  const [hashA, hashB] = await Promise.all([
    crypto.subtle.digest("SHA-256", enc.encode(a)),
    crypto.subtle.digest("SHA-256", enc.encode(b)),
  ]);
  const bytesA = new Uint8Array(hashA);
  const bytesB = new Uint8Array(hashB);
  let diff = 0;
  for (let i = 0; i < bytesA.length; i++) {
    diff |= bytesA[i] ^ bytesB[i];
  }
  return diff === 0;
}

function extractPlanFromExternalRef(ref?: string): "starter" | "pro" | "enterprise" | null {
  if (!ref) return null;
  const match = /:plan:(starter|pro|enterprise)\b/i.exec(ref);
  if (!match) return null;
  return match[1].toLowerCase() as "starter" | "pro" | "enterprise";
}

function extractPrepaidFromExternalRef(ref?: string): number {
  if (!ref) return 0;
  const match = /:prepaid:([0-9]+(?:\.[0-9]+)?)\b/i.exec(ref);
  if (!match) return 0;
  const parsed = Number(match[1]);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : 0;
}

function extractTenantFromExternalRef(ref?: string): string | null {
  if (!ref) return null;
  const match = /tenant:([0-9a-f-]{36})/i.exec(ref);
  return match ? match[1] : null;
}

function extractClosingFromExternalRef(ref?: string): string | null {
  if (!ref) return null;
  const match = /closing:([0-9a-f-]{36})/i.exec(ref);
  return match ? match[1] : null;
}

Deno.serve(async (req: Request): Promise<Response> => {
  if (req.method !== "POST") {
    return jsonResponse({ status: "error", message: "Method not allowed" }, 405);
  }

  const expectedWebhookToken = Deno.env.get("ASAAS_WEBHOOK_TOKEN")?.trim() ?? "";
  if (!expectedWebhookToken) {
    return jsonResponse({ status: "error", message: "ASAAS_WEBHOOK_TOKEN not configured" }, 503);
  }

  const receivedToken = (req.headers.get("asaas-access-token") ?? "").trim();
  if (!receivedToken || !(await constantTimeEqual(receivedToken, expectedWebhookToken))) {
    return jsonResponse({ status: "error", message: "Unauthorized webhook token" }, 401);
  }

  const supabaseUrl = Deno.env.get("SUPABASE_URL");
  const serviceRoleKey = Deno.env.get("SUPABASE_SERVICE_ROLE_KEY");
  if (!supabaseUrl || !serviceRoleKey) {
    return jsonResponse({ status: "error", message: "Missing Supabase service credentials" }, 500);
  }

  let body: AsaasWebhookPayload;
  try {
    body = (await req.json()) as AsaasWebhookPayload;
  } catch {
    return jsonResponse({ status: "error", message: "Invalid JSON payload" }, 400);
  }

  const eventType = (body.event ?? "").trim().toUpperCase();
  const payment = body.payment ?? {};
  const paymentId = (payment.id ?? "").trim();
  const subscriptionId = (payment.subscription ?? "").trim();
  const customerId = (payment.customer ?? "").trim();
  const externalRef = (payment.externalReference ?? "").trim();

  if (!eventType) {
    return jsonResponse({ status: "ignored", message: "Missing event type" }, 200);
  }

  const eventId = (body.id ?? `${eventType}:${paymentId || subscriptionId || Date.now()}`).trim();
  const adminClient = createClient(supabaseUrl, serviceRoleKey, {
    auth: { persistSession: false, autoRefreshToken: false },
  });

  // Resolve tenant_id via externalReference -> subscription_id -> customer_id
  let tenantId = extractTenantFromExternalRef(externalRef);
  if (!tenantId && subscriptionId) {
    const { data } = await adminClient
      .from("tenants")
      .select("id")
      .eq("gateway_subscription_id", subscriptionId)
      .maybeSingle();
    tenantId = (data?.id as string | undefined) ?? null;
  }
  if (!tenantId && customerId) {
    const { data } = await adminClient
      .from("tenants")
      .select("id")
      .eq("gateway_customer_id", customerId)
      .maybeSingle();
    tenantId = (data?.id as string | undefined) ?? null;
  }

  // Atomic idempotency insert (unique violation 23505 => already processed)
  const { error: logErr } = await adminClient.from("payment_webhook_events").insert({
    provider: "asaas",
    event_id: eventId,
    event_type: eventType,
    payment_id: paymentId || null,
    subscription_id: subscriptionId || null,
    tenant_id: tenantId,
    payload: body,
  });

  if (logErr) {
    if (logErr.code === "23505") {
      return jsonResponse({ status: "already_processed", event_id: eventId }, 200);
    }
    return jsonResponse({ status: "error", message: `Webhook log error: ${logErr.message}` }, 500);
  }

  // B2B2C End-client closing payment confirmation
  const closingId = extractClosingFromExternalRef(externalRef);
  if (closingId && (eventType === "PAYMENT_CONFIRMED" || eventType === "PAYMENT_RECEIVED")) {
    const { error: closeErr } = await adminClient
      .from("closings")
      .update({ status: "pago", updated_at: new Date().toISOString() })
      .eq("id", closingId);
    if (closeErr) {
      return jsonResponse({ status: "error", message: closeErr.message }, 500);
    }
    return jsonResponse({ status: "processed", scope: "closing", closing_id: closingId }, 200);
  }

  // Also check by asaas_payment_id if externalRef didn't include closing:
  if (paymentId && (eventType === "PAYMENT_CONFIRMED" || eventType === "PAYMENT_RECEIVED")) {
    const { data: paidClosing, error: findCloseErr } = await adminClient
      .from("closings")
      .select("id")
      .eq("asaas_payment_id", paymentId)
      .maybeSingle();
    if (findCloseErr) {
      return jsonResponse({ status: "error", message: findCloseErr.message }, 500);
    }
    if (paidClosing?.id) {
      const { error: closeErr } = await adminClient
        .from("closings")
        .update({ status: "pago", updated_at: new Date().toISOString() })
        .eq("id", paidClosing.id);
      if (closeErr) {
        return jsonResponse({ status: "error", message: closeErr.message }, 500);
      }
      return jsonResponse({ status: "processed", scope: "closing", closing_id: paidClosing.id }, 200);
    }
  }

  if (!tenantId) {
    return jsonResponse({ status: "ignored", message: "No matching tenant found for event", event_id: eventId }, 200);
  }

  const parsedPlan = extractPlanFromExternalRef(externalRef);
  const prepaidRefAmount = extractPrepaidFromExternalRef(externalRef);

  if (eventType === "PAYMENT_CONFIRMED" || eventType === "PAYMENT_RECEIVED") {
    const updates: Record<string, unknown> = {
      subscription_status: "active",
      pioneer_courtesy_active: false,
      max_printers: 10000,
      updated_at: new Date().toISOString(),
    };
    if (parsedPlan) {
      updates.plan_tier = parsedPlan;
    }
    if (subscriptionId) {
      updates.gateway_subscription_id = subscriptionId;
    }
    if (customerId) {
      updates.gateway_customer_id = customerId;
    }
    if (prepaidRefAmount > 0) {
      const creditToAdd = Number((Number(payment.value ?? prepaidRefAmount) || prepaidRefAmount).toFixed(2));
      const { data: currTenant } = await adminClient
        .from("tenants")
        .select("prepaid_credit_balance")
        .eq("id", tenantId)
        .maybeSingle();
      const prevBal = Number(currTenant?.prepaid_credit_balance ?? 0);
      updates.prepaid_credit_balance = Number((prevBal + creditToAdd).toFixed(2));
    }

    const { error: updErr } = await adminClient.from("tenants").update(updates).eq("id", tenantId);
    if (updErr) {
      return jsonResponse({ status: "error", message: updErr.message }, 500);
    }
    return jsonResponse({ status: "processed", tenant_id: tenantId, subscription_status: "active" }, 200);
  }

  if (eventType === "PAYMENT_OVERDUE") {
    const { error: overdueErr } = await adminClient
      .from("tenants")
      .update({ subscription_status: "past_due", updated_at: new Date().toISOString() })
      .eq("id", tenantId);
    if (overdueErr) {
      return jsonResponse({ status: "error", message: overdueErr.message }, 500);
    }
    return jsonResponse({ status: "processed", tenant_id: tenantId, subscription_status: "past_due" }, 200);
  }

  if (
    eventType === "PAYMENT_REFUNDED" ||
    eventType === "SUBSCRIPTION_DELETED" ||
    eventType === "SUBSCRIPTION_INACTIVATED"
  ) {
    const { error: cancelErr } = await adminClient
      .from("tenants")
      .update({
        subscription_status: "canceled",
        plan_tier: "trial",
        max_printers: 2,
        pioneer_courtesy_active: false,
        updated_at: new Date().toISOString(),
      })
      .eq("id", tenantId);
    if (cancelErr) {
      return jsonResponse({ status: "error", message: cancelErr.message }, 500);
    }
    return jsonResponse({ status: "processed", tenant_id: tenantId, subscription_status: "canceled" }, 200);
  }

  return jsonResponse({ status: "ignored", event_type: eventType }, 200);
});
