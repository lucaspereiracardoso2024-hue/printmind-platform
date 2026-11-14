import { createClient } from "https://esm.sh/@supabase/supabase-js@2";

const GEMINI_MODEL = "gemini-2.5-flash";
const GROQ_MODEL = "llama-3.3-70b-versatile";
const OPENROUTER_MODEL = "meta-llama/llama-3.3-70b-instruct:free";
const PROVIDER_TIMEOUT_MS = 7000;
const MASTER_ADMIN_EMAIL = "lucaspereiracardoso2024@gmail.com";

const corsHeaders: Record<string, string> = {
  "Access-Control-Allow-Origin": "*",
  "Access-Control-Allow-Headers": "authorization, x-client-info, apikey, content-type",
  "Access-Control-Allow-Methods": "POST, OPTIONS",
};

interface UrgentAction {
  priority: "alta" | "media" | "baixa";
  target: string;
  action: string;
  reason: string;
}

interface CostSavingTip {
  title: string;
  estimated_impact: string;
  recommendation: string;
}

interface FleetAnomaly {
  type: string;
  severity: "critical" | "warning" | "info";
  target: string;
  detail: string;
}

interface GeminiInsightsPayload {
  executive_summary: string;
  urgent_actions: UrgentAction[];
  cost_saving_tips: CostSavingTip[];
  whatsapp_briefing_ptbr: string;
  anomalies: FleetAnomaly[];
}

interface AnonymizeResult {
  sanitizedContext: Record<string, unknown>;
  safeRestoreMap: Map<string, string>;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      ...corsHeaders,
      "Content-Type": "application/json; charset=utf-8",
    },
  });
}

function stripJsonFences(raw: string): string {
  const trimmed = raw.trim();
  if (trimmed.startsWith("```")) {
    return trimmed
      .replace(/^```(?:json)?\s*/i, "")
      .replace(/\s*```$/i, "")
      .trim();
  }
  return trimmed;
}

function scrubPIIString(input: string): string {
  return input
    .replace(/\b\d{2}\.\d{3}\.\d{3}\/\d{4}-\d{2}\b/g, "[CNPJ_OMITIDO]")
    .replace(/\b\d{3}\.\d{3}\.\d{3}-\d{2}\b/g, "[CPF_OMITIDO]")
    .replace(/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g, "[EMAIL_OMITIDO]")
    .replace(/\b(?:\d{1,3}\.){3}\d{1,3}\b/g, "[IP_OMITIDO]");
}

function pad3(n: number): string {
  return String(n).padStart(3, "0");
}

function anonymizeFleetContext(raw: Record<string, unknown>): AnonymizeResult {
  const forwardMap = new Map<string, string>();
  const safeRestoreMap = new Map<string, string>();
  const counters = {
    tenant: 0,
    client: 0,
    unit: 0,
    dept: 0,
    printer: 0,
    serial: 0,
    user: 0,
    machine: 0,
    ip: 0,
  };

  function getOrAssign(
    kind: keyof typeof counters,
    prefix: string,
    rawVal: unknown,
    restoreToTenantUI: boolean,
  ): unknown {
    if (typeof rawVal !== "string") return rawVal;
    const trimmed = rawVal.trim();
    if (!trimmed || trimmed === "Geral" || trimmed === "Sistema") return trimmed;
    const mapKey = `${kind}:${trimmed.toLowerCase()}`;
    const existing = forwardMap.get(mapKey);
    if (existing) return existing;
    counters[kind] += 1;
    const token = `${prefix}_${pad3(counters[kind])}`;
    forwardMap.set(mapKey, token);
    if (restoreToTenantUI) {
      safeRestoreMap.set(token, trimmed);
    }
    return token;
  }

  function walk(node: unknown, parentKey = ""): unknown {
    if (Array.isArray(node)) {
      return node.map((item) => walk(item, parentKey));
    }
    if (node !== null && typeof node === "object") {
      const src = node as Record<string, unknown>;
      const out: Record<string, unknown> = {};
      for (const [k, v] of Object.entries(src)) {
        const kl = k.toLowerCase();
        if (kl === "tenant_name" || kl === "trade_name" || kl === "legal_name") {
          out[k] = getOrAssign("tenant", "Empresa", v, true);
        } else if (kl === "client_name" || kl === "billing_pool_group") {
          out[k] = getOrAssign("client", "Cliente", v, true);
        } else if (kl === "unit" || kl === "unit_name") {
          out[k] = getOrAssign("unit", "Unidade", v, true);
        } else if (kl === "department") {
          out[k] = getOrAssign("dept", "Setor", v, true);
        } else if (kl === "printer_model" || (kl === "model" && parentKey !== "host_info")) {
          out[k] = getOrAssign("printer", "Impressora", v, true);
        } else if (kl === "serial" || kl === "serial_number") {
          out[k] = getOrAssign("serial", "SN", v, false);
        } else if (kl === "user_name") {
          out[k] = getOrAssign("user", "Usuario", v, false);
        } else if (kl === "machine_name" || kl === "hostname" || kl === "agent_hostname") {
          out[k] = getOrAssign("machine", "Estacao", v, false);
        } else if (kl === "ip" || kl === "ip_address" || kl === "printer_ip") {
          out[k] = getOrAssign("ip", "IP", v, false);
        } else if (kl === "cnpj" || kl === "cnpj_cpf" || kl === "email" || kl === "phone" || kl === "pix_key") {
          out[k] = "[OMITIDO]";
        } else {
          out[k] = walk(v, k);
        }
      }
      return out;
    }
    if (typeof node === "string") {
      return scrubPIIString(node);
    }
    return node;
  }

  const sanitizedContext = walk(raw) as Record<string, unknown>;
  return { sanitizedContext, safeRestoreMap };
}

function restoreText(text: string, entries: Array<[string, string]>): string {
  let out = text;
  for (const [token, original] of entries) {
    if (!token || !original) continue;
    const re = new RegExp(`\\b${token}\\b`, "g");
    out = out.replace(re, original);
  }
  return out;
}

function deanonimizeInsights(
  insights: GeminiInsightsPayload,
  restoreMap: Map<string, string>,
): GeminiInsightsPayload {
  // Sort descending by pseudonym key length so prefix collisions (e.g. Cliente_001 vs Cliente_0010) never happen
  const sortedEntries = Array.from(restoreMap.entries()).sort((a, b) => b[0].length - a[0].length);
  return {
    executive_summary: restoreText(insights.executive_summary ?? "", sortedEntries),
    urgent_actions: (insights.urgent_actions ?? []).map((a) => ({
      priority: a.priority,
      target: restoreText(a.target ?? "", sortedEntries),
      action: restoreText(a.action ?? "", sortedEntries),
      reason: restoreText(a.reason ?? "", sortedEntries),
    })),
    cost_saving_tips: (insights.cost_saving_tips ?? []).map((tip) => ({
      title: restoreText(tip.title ?? "", sortedEntries),
      estimated_impact: restoreText(tip.estimated_impact ?? "", sortedEntries),
      recommendation: restoreText(tip.recommendation ?? "", sortedEntries),
    })),
    whatsapp_briefing_ptbr: restoreText(insights.whatsapp_briefing_ptbr ?? "", sortedEntries),
    anomalies: (insights.anomalies ?? []).map((an) => ({
      type: restoreText(an.type ?? "", sortedEntries),
      severity: an.severity,
      target: restoreText(an.target ?? "", sortedEntries),
      detail: restoreText(an.detail ?? "", sortedEntries),
    })),
  };
}

function validateInsightsPayload(obj: unknown): GeminiInsightsPayload {
  const p = obj as Partial<GeminiInsightsPayload>;
  if (!p || typeof p.executive_summary !== "string") {
    throw new Error("Payload IA sem executive_summary.");
  }
  return {
    executive_summary: p.executive_summary,
    urgent_actions: Array.isArray(p.urgent_actions) ? p.urgent_actions : [],
    cost_saving_tips: Array.isArray(p.cost_saving_tips) ? p.cost_saving_tips : [],
    whatsapp_briefing_ptbr: typeof p.whatsapp_briefing_ptbr === "string" ? p.whatsapp_briefing_ptbr : "",
    anomalies: Array.isArray(p.anomalies) ? p.anomalies : [],
  };
}

function buildSummaryMarkdown(insights: GeminiInsightsPayload, periodMonth: string): string {
  const lines: string[] = [];
  lines.push(`# PrintMind IA Briefing (${periodMonth})`);
  lines.push("");
  lines.push("## Resumo Executivo");
  lines.push(insights.executive_summary || "Sem resumo disponível.");
  lines.push("");

  if (insights.urgent_actions?.length) {
    lines.push("## Ações Urgentes");
    for (const item of insights.urgent_actions) {
      lines.push(`- **[${String(item.priority || "media").toUpperCase()}] ${item.target}**: ${item.action} _(${item.reason})_`);
    }
    lines.push("");
  }

  if (insights.anomalies?.length) {
    lines.push("## Anomalias Detectadas");
    for (const a of insights.anomalies) {
      lines.push(`- **[${String(a.severity || "info").toUpperCase()}] ${a.type} — ${a.target}**: ${a.detail}`);
    }
    lines.push("");
  }

  if (insights.cost_saving_tips?.length) {
    lines.push("## Oportunidades de Economia");
    for (const tip of insights.cost_saving_tips) {
      lines.push(`- **${tip.title}** (${tip.estimated_impact}): ${tip.recommendation}`);
    }
    lines.push("");
  }

  if (insights.whatsapp_briefing_ptbr) {
    lines.push("## Briefing WhatsApp (PT-BR)");
    lines.push(insights.whatsapp_briefing_ptbr);
  }

  return lines.join("\n");
}

async function callGeminiProvider(
  apiKey: string,
  systemInstruction: string,
  userPrompt: string,
): Promise<{ insights: GeminiInsightsPayload; modelUsed: string }> {
  const geminiUrl = `https://generativelanguage.googleapis.com/v1beta/models/${GEMINI_MODEL}:generateContent`;
  const resp = await fetch(geminiUrl, {
    method: "POST",
    signal: AbortSignal.timeout(PROVIDER_TIMEOUT_MS),
    headers: {
      "Content-Type": "application/json",
      "x-goog-api-key": apiKey,
    },
    body: JSON.stringify({
      systemInstruction: { parts: [{ text: systemInstruction }] },
      contents: [{ role: "user", parts: [{ text: userPrompt }] }],
      generationConfig: {
        temperature: 0.2,
        responseMimeType: "application/json",
        responseSchema: {
          type: "OBJECT",
          properties: {
            executive_summary: { type: "STRING" },
            urgent_actions: {
              type: "ARRAY",
              items: {
                type: "OBJECT",
                properties: {
                  priority: { type: "STRING", enum: ["alta", "media", "baixa"] },
                  target: { type: "STRING" },
                  action: { type: "STRING" },
                  reason: { type: "STRING" },
                },
                required: ["priority", "target", "action", "reason"],
              },
            },
            cost_saving_tips: {
              type: "ARRAY",
              items: {
                type: "OBJECT",
                properties: {
                  title: { type: "STRING" },
                  estimated_impact: { type: "STRING" },
                  recommendation: { type: "STRING" },
                },
                required: ["title", "estimated_impact", "recommendation"],
              },
            },
            whatsapp_briefing_ptbr: { type: "STRING" },
            anomalies: {
              type: "ARRAY",
              items: {
                type: "OBJECT",
                properties: {
                  type: { type: "STRING" },
                  severity: { type: "STRING", enum: ["critical", "warning", "info"] },
                  target: { type: "STRING" },
                  detail: { type: "STRING" },
                },
                required: ["type", "severity", "target", "detail"],
              },
            },
          },
          required: ["executive_summary", "urgent_actions", "cost_saving_tips", "whatsapp_briefing_ptbr", "anomalies"],
        },
      },
    }),
  });

  if (!resp.ok) {
    throw new Error(`Gemini HTTP ${resp.status}`);
  }
  const data = (await resp.json()) as { candidates?: Array<{ content?: { parts?: Array<{ text?: string }> } }> };
  const rawText = data?.candidates?.[0]?.content?.parts?.[0]?.text;
  if (!rawText) {
    throw new Error("Gemini empty response");
  }
  return {
    insights: validateInsightsPayload(JSON.parse(stripJsonFences(rawText))),
    modelUsed: `gemini:${GEMINI_MODEL}`,
  };
}

async function callOpenAICompatibleProvider(
  endpoint: string,
  apiKey: string,
  model: string,
  providerLabel: string,
  systemInstruction: string,
  userPrompt: string,
): Promise<{ insights: GeminiInsightsPayload; modelUsed: string }> {
  const resp = await fetch(endpoint, {
    method: "POST",
    signal: AbortSignal.timeout(PROVIDER_TIMEOUT_MS),
    headers: {
      "Content-Type": "application/json",
      "Authorization": `Bearer ${apiKey}`,
    },
    body: JSON.stringify({
      model,
      temperature: 0.2,
      response_format: { type: "json_object" },
      messages: [
        {
          role: "system",
          content: `${systemInstruction}\nResponda APENAS um JSON válido com as chaves exatas: executive_summary (string), urgent_actions (array de {priority,target,action,reason}), cost_saving_tips (array de {title,estimated_impact,recommendation}), whatsapp_briefing_ptbr (string), anomalies (array de {type,severity,target,detail}).`,
        },
        { role: "user", content: userPrompt },
      ],
    }),
  });
  if (!resp.ok) {
    throw new Error(`${providerLabel} HTTP ${resp.status}`);
  }
  const data = (await resp.json()) as { choices?: Array<{ message?: { content?: string } }> };
  const rawText = data?.choices?.[0]?.message?.content;
  if (!rawText) {
    throw new Error(`${providerLabel} empty response`);
  }
  return {
    insights: validateInsightsPayload(JSON.parse(stripJsonFences(rawText))),
    modelUsed: `${providerLabel}:${model}`,
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
    return jsonResponse({ status: "error", code: "SUPABASE_ENV_MISSING", message: "Configuração do Supabase ausente." }, 500);
  }

  const authHeader = req.headers.get("Authorization");
  if (!authHeader || !authHeader.toLowerCase().startsWith("bearer ")) {
    return jsonResponse({ status: "error", code: "UNAUTHORIZED", message: "Authorization Bearer obrigatório." }, 401);
  }

  const supabase = createClient(supabaseUrl, supabaseAnonKey, {
    global: { headers: { Authorization: authHeader } },
    auth: { persistSession: false, autoRefreshToken: false },
  });

  const { data: { user }, error: authError } = await supabase.auth.getUser();
  if (authError || !user) {
    return jsonResponse({ status: "error", code: "INVALID_TOKEN", message: "Sessão expirada." }, 401);
  }

  let requestedTenantId: string | undefined;
  let forceRefresh = false;
  try {
    const rawBody = await req.text();
    if (rawBody.trim().length > 0) {
      const parsed = JSON.parse(rawBody) as { tenant_id?: string; force_refresh?: boolean };
      if (typeof parsed.tenant_id === "string" && parsed.tenant_id.trim().length > 0) {
        requestedTenantId = parsed.tenant_id.trim();
      }
      forceRefresh = Boolean(parsed.force_refresh);
    }
  } catch {
    return jsonResponse({ status: "error", code: "INVALID_JSON", message: "Corpo JSON inválido." }, 400);
  }

  const { data: profile, error: profileError } = await supabase
    .from("profiles")
    .select("tenant_id, role, email")
    .eq("id", user.id)
    .maybeSingle();

  if (profileError) {
    return jsonResponse({ status: "error", code: "PROFILE_LOOKUP_FAILED", message: profileError.message }, 500);
  }

  const isMasterAdmin = (user.email ?? "").toLowerCase().trim() === MASTER_ADMIN_EMAIL;
  const targetTenantId = requestedTenantId ?? (profile?.tenant_id as string | undefined);
  if (!targetTenantId) {
    return jsonResponse({ status: "error", code: "TENANT_NOT_FOUND", message: "Nenhum tenant_id associado." }, 400);
  }
  if (requestedTenantId && !isMasterAdmin && profile?.tenant_id !== requestedTenantId) {
    return jsonResponse({ status: "error", code: "FORBIDDEN", message: "Acesso negado ao tenant solicitado." }, 403);
  }

  const adminClient = createClient(supabaseUrl, serviceRoleKey, {
    auth: { persistSession: false, autoRefreshToken: false },
  });

  // Plan quota & rate-limit guard (max 3 calls per 24h for non-Master tenants; 6h cache hit)
  const { data: tenantInfo } = await adminClient
    .from("tenants")
    .select("plan_tier, subscription_status, trial_ends_at, pioneer_courtesy_active")
    .eq("id", targetTenantId)
    .maybeSingle();

  if (!isMasterAdmin && tenantInfo) {
    const tier = String(tenantInfo.plan_tier ?? "trial");
    const status = String(tenantInfo.subscription_status ?? "trialing");
    const trialExpired = tier === "trial" && tenantInfo.trial_ends_at && new Date(String(tenantInfo.trial_ends_at)).getTime() < Date.now();
    if (trialExpired || (tier === "starter" && !tenantInfo.pioneer_courtesy_active) || status === "past_due") {
      return jsonResponse(
        {
          status: "error",
          code: "PLAN_UPGRADE_REQUIRED",
          message: "Análises executivas com IA estão disponíveis nos planos Gestão/Completo ou durante o Teste Gratuito ativo.",
        },
        403,
      );
    }

    if (!forceRefresh) {
      const sixHoursAgo = new Date(Date.now() - 6 * 3600 * 1000).toISOString();
      const { data: cached } = await adminClient
        .from("ai_insights")
        .select("*")
        .eq("tenant_id", targetTenantId)
        .gte("created_at", sixHoursAgo)
        .order("created_at", { ascending: false })
        .limit(1)
        .maybeSingle();

      if (cached) {
        return jsonResponse({
          status: "success",
          cached: true,
          insight_id: cached.id,
          created_at: cached.created_at,
          model_used: cached.model_used,
          period_month: cached.period_month,
          context: cached.deterministic_context,
          insights: cached.insights_json,
          summary_markdown: cached.summary_markdown,
        });
      }
    }

    const dayAgo = new Date(Date.now() - 24 * 3600 * 1000).toISOString();
    const { count: dailyCount } = await adminClient
      .from("ai_insights")
      .select("id", { count: "exact", head: true })
      .eq("tenant_id", targetTenantId)
      .gte("created_at", dayAgo);

    if ((dailyCount ?? 0) >= 3) {
      return jsonResponse(
        {
          status: "error",
          code: "DAILY_AI_QUOTA_EXCEEDED",
          message: "Limite diário de 3 análises de IA atingido para sua conta hoje. Tente novamente em 24 horas.",
        },
        429,
      );
    }
  }

  // 1. Fetch deterministic fleet context via RPC
  const { data: fleetContextRaw, error: rpcError } = await supabase.rpc("get_fleet_ai_context", {
    p_tenant_id: targetTenantId,
  });

  if (rpcError || !fleetContextRaw) {
    return jsonResponse(
      {
        status: "error",
        code: "CONTEXT_RPC_FAILED",
        message: rpcError?.message ?? "Falha ao calcular contexto determinístico da frota.",
      },
      rpcError?.code === "42501" ? 403 : 500,
    );
  }

  const fleetContext = fleetContextRaw as Record<string, unknown>;
  const { sanitizedContext, safeRestoreMap } = anonymizeFleetContext(fleetContext);

  const systemInstruction = [
    "Você é o analista executivo sênior de Managed Print Services (MPS) do PrintMind.",
    "REGRAS INVIOLÁVEIS:",
    "1. NUNCA invente, arredonde indevidamente, estime por conta própria ou altere números, percentuais, contadores de páginas, identificadores (Cliente_001, Impressora_001, Usuario_001) ou valores em R$.",
    "2. Use exatamente os pseudônimos fornecidos no JSON (ex: Cliente_001, Impressora_001, Setor_001) sem modificá-los.",
    "3. Baseie-se EXCLUSIVAMENTE nos dados já pré-calculados no JSON de contexto determinístico fornecido.",
    "4. Responda em Português do Brasil (PT-BR) claro, objetivo e acionável para o gestor de locação de impressoras.",
  ].join("\n");

  const userPrompt = [
    "Analise o seguinte JSON determinístico e pseudonimizado do parque de impressoras e produza o diagnóstico estruturado:",
    JSON.stringify(sanitizedContext),
  ].join("\n\n");

  // 2. Multi-provider cascade with short 7s timeout per provider (Gemini -> Groq -> OpenRouter)
  const geminiApiKey = Deno.env.get("GEMINI_API_KEY")?.trim() ?? "";
  const groqApiKey = Deno.env.get("GROQ_API_KEY")?.trim() ?? "";
  const openRouterKey = Deno.env.get("OPENROUTER_API_KEY")?.trim() ?? "";

  if (!geminiApiKey && !groqApiKey && !openRouterKey) {
    return jsonResponse(
      {
        status: "error",
        code: "AI_PROVIDERS_NOT_CONFIGURED",
        message: "Nenhum provedor de IA (GEMINI_API_KEY, GROQ_API_KEY ou OPENROUTER_API_KEY) está configurado nas Edge Functions.",
        context: fleetContext,
      },
      503,
    );
  }

  let providerResult: { insights: GeminiInsightsPayload; modelUsed: string } | null = null;
  const providerErrors: string[] = [];

  if (geminiApiKey) {
    try {
      providerResult = await callGeminiProvider(geminiApiKey, systemInstruction, userPrompt);
    } catch (err) {
      providerErrors.push(err instanceof Error ? err.message : String(err));
    }
  }

  if (!providerResult && groqApiKey) {
    try {
      providerResult = await callOpenAICompatibleProvider(
        "https://api.groq.com/openai/v1/chat/completions",
        groqApiKey,
        GROQ_MODEL,
        "groq",
        systemInstruction,
        userPrompt,
      );
    } catch (err) {
      providerErrors.push(err instanceof Error ? err.message : String(err));
    }
  }

  if (!providerResult && openRouterKey) {
    try {
      providerResult = await callOpenAICompatibleProvider(
        "https://openrouter.ai/api/v1/chat/completions",
        openRouterKey,
        OPENROUTER_MODEL,
        "openrouter",
        systemInstruction,
        userPrompt,
      );
    } catch (err) {
      providerErrors.push(err instanceof Error ? err.message : String(err));
    }
  }

  if (!providerResult) {
    return jsonResponse(
      {
        status: "error",
        code: "ALL_AI_PROVIDERS_FAILED",
        message: "Todos os provedores de IA falharam ou excederam o tempo limite.",
        details: providerErrors.join(" | "),
      },
      502,
    );
  }

  // 3. Re-hydrate business labels (Cliente_001 -> Client name) while keeping PII masked
  const restoredInsights = deanonimizeInsights(providerResult.insights, safeRestoreMap);
  const periodMonth = String(fleetContext.period_month ?? new Date().toISOString().slice(0, 7));
  const summaryMarkdown = buildSummaryMarkdown(restoredInsights, periodMonth);

  const { data: savedRow, error: insertError } = await supabase
    .from("ai_insights")
    .insert({
      tenant_id: targetTenantId,
      period_month: periodMonth,
      model_used: providerResult.modelUsed,
      deterministic_context: fleetContext,
      insights_json: restoredInsights,
      summary_markdown: summaryMarkdown,
    })
    .select("id, created_at")
    .single();

  return jsonResponse({
    status: "success",
    cached: false,
    insight_id: savedRow?.id ?? null,
    created_at: savedRow?.created_at ?? new Date().toISOString(),
    persistence_warning: insertError?.message ?? undefined,
    model_used: providerResult.modelUsed,
    period_month: periodMonth,
    context: fleetContext,
    insights: restoredInsights,
    summary_markdown: summaryMarkdown,
  });
});
