import { createClient } from "https://esm.sh/@supabase/supabase-js@2";

const MODEL_NAME = "gemini-2.5-flash";
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
      lines.push(`- **[${item.priority.toUpperCase()}] ${item.target}**: ${item.action} _(${item.reason})_`);
    }
    lines.push("");
  }

  if (insights.anomalies?.length) {
    lines.push("## Anomalias Detectadas");
    for (const a of insights.anomalies) {
      lines.push(`- **[${a.severity.toUpperCase()}] ${a.type} — ${a.target}**: ${a.detail}`);
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

Deno.serve(async (req: Request): Promise<Response> => {
  if (req.method === "OPTIONS") {
    return new Response("ok", { headers: corsHeaders });
  }

  if (req.method !== "POST") {
    return jsonResponse(
      { status: "error", code: "METHOD_NOT_ALLOWED", message: "Use POST para gerar insights." },
      405,
    );
  }

  const supabaseUrl = Deno.env.get("SUPABASE_URL");
  const supabaseAnonKey = Deno.env.get("SUPABASE_ANON_KEY");
  if (!supabaseUrl || !supabaseAnonKey) {
    return jsonResponse(
      {
        status: "error",
        code: "SUPABASE_ENV_MISSING",
        message: "Variáveis SUPABASE_URL ou SUPABASE_ANON_KEY ausentes no ambiente da Edge Function.",
      },
      500,
    );
  }

  const authHeader = req.headers.get("Authorization");
  if (!authHeader || !authHeader.toLowerCase().startsWith("bearer ")) {
    return jsonResponse(
      {
        status: "error",
        code: "UNAUTHORIZED",
        message: "Cabeçalho Authorization: Bearer <jwt> obrigatório.",
      },
      401,
    );
  }

  const supabase = createClient(supabaseUrl, supabaseAnonKey, {
    global: {
      headers: { Authorization: authHeader },
    },
    auth: {
      persistSession: false,
      autoRefreshToken: false,
    },
  });

  const {
    data: { user },
    error: authError,
  } = await supabase.auth.getUser();

  if (authError || !user) {
    return jsonResponse(
      {
        status: "error",
        code: "INVALID_TOKEN",
        message: "Sessão inválida ou expirada. Faça login novamente.",
      },
      401,
    );
  }

  let requestedTenantId: string | undefined;
  try {
    const rawBody = await req.text();
    if (rawBody.trim().length > 0) {
      const parsed = JSON.parse(rawBody) as { tenant_id?: string };
      if (typeof parsed.tenant_id === "string" && parsed.tenant_id.trim().length > 0) {
        requestedTenantId = parsed.tenant_id.trim();
      }
    }
  } catch {
    return jsonResponse(
      { status: "error", code: "INVALID_JSON", message: "Corpo JSON inválido." },
      400,
    );
  }

  const { data: profile, error: profileError } = await supabase
    .from("profiles")
    .select("tenant_id, role, email")
    .eq("id", user.id)
    .maybeSingle();

  if (profileError) {
    return jsonResponse(
      {
        status: "error",
        code: "PROFILE_LOOKUP_FAILED",
        message: `Erro ao consultar perfil do usuário: ${profileError.message}`,
      },
      500,
    );
  }

  const isMasterAdmin = (user.email ?? "").toLowerCase().trim() === MASTER_ADMIN_EMAIL;
  const targetTenantId = requestedTenantId ?? profile?.tenant_id;

  if (!targetTenantId) {
    return jsonResponse(
      {
        status: "error",
        code: "TENANT_NOT_FOUND",
        message: "Nenhum tenant_id associado ao usuário autenticado.",
      },
      400,
    );
  }

  if (requestedTenantId && !isMasterAdmin && profile?.tenant_id !== requestedTenantId) {
    return jsonResponse(
      {
        status: "error",
        code: "FORBIDDEN",
        message: "Acesso negado ao tenant solicitado.",
      },
      403,
    );
  }

  // 1. Fetch deterministic pre-calculated fleet telemetry & financial context
  const { data: fleetContext, error: rpcError } = await supabase.rpc("get_fleet_ai_context", {
    p_tenant_id: targetTenantId,
  });

  if (rpcError || !fleetContext) {
    return jsonResponse(
      {
        status: "error",
        code: "CONTEXT_RPC_FAILED",
        message: rpcError?.message ?? "Falha ao calcular contexto determinístico da frota.",
      },
      rpcError?.code === "42501" ? 403 : 500,
    );
  }

  // 2. Verify Gemini API secret
  const geminiApiKey = Deno.env.get("GEMINI_API_KEY")?.trim();
  if (!geminiApiKey) {
    return jsonResponse(
      {
        status: "error",
        code: "GEMINI_API_KEY_NOT_CONFIGURED",
        message:
          "O segredo GEMINI_API_KEY não está configurado no Supabase Edge Functions. Configure via `supabase secrets set GEMINI_API_KEY=...` no projeto zbsmzcbcivhzslcnfxmq.",
        context: fleetContext,
      },
      503,
    );
  }

  // 3. Call Gemini 2.5 Flash with strict grounding & structured JSON output
  const systemInstruction = [
    "Você é o analista executivo sênior de Managed Print Services (MPS) do PrintMind.",
    "REGRAS INVIOLÁVEIS:",
    "1. NUNCA invente, arredonde indevidamente, estime por conta própria ou altere números, percentuais, contadores de páginas, números de série, valores em R$ ou nomes de equipamentos/clientes.",
    "2. Baseie-se EXCLUSIVAMENTE nos dados já pré-calculados no JSON de contexto determinístico fornecido.",
    "3. Se uma lista (ex: trocas prematuras, drenagens, impressoras ociosas, rateio por departamento) estiver vazia, informe explicitamente que não há ocorrências registradas no período — jamais fabrique exemplos.",
    "4. Responda em Português do Brasil (PT-BR) claro, objetivo e acionável para o gestor de locação de impressoras.",
  ].join("\n");

  const userPrompt = [
    "Analise o seguinte JSON determinístico do parque de impressoras e produza o diagnóstico estruturado:",
    JSON.stringify(fleetContext),
  ].join("\n\n");

  const geminiUrl = `https://generativelanguage.googleapis.com/v1beta/models/${MODEL_NAME}:generateContent`;
  const geminiResp = await fetch(geminiUrl, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "x-goog-api-key": geminiApiKey,
    },
    body: JSON.stringify({
      systemInstruction: {
        parts: [{ text: systemInstruction }],
      },
      contents: [
        {
          role: "user",
          parts: [{ text: userPrompt }],
        },
      ],
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
          required: [
            "executive_summary",
            "urgent_actions",
            "cost_saving_tips",
            "whatsapp_briefing_ptbr",
            "anomalies",
          ],
        },
      },
    }),
  });

  if (!geminiResp.ok) {
    const errText = await geminiResp.text();
    return jsonResponse(
      {
        status: "error",
        code: "GEMINI_API_ERROR",
        message: `Gemini API retornou HTTP ${geminiResp.status}`,
        details: errText.slice(0, 600),
      },
      502,
    );
  }

  const geminiJson = await geminiResp.json();
  const rawText: string | undefined = geminiJson?.candidates?.[0]?.content?.parts?.[0]?.text;
  if (!rawText) {
    return jsonResponse(
      {
        status: "error",
        code: "GEMINI_EMPTY_RESPONSE",
        message: "A API Gemini não retornou conteúdo textual.",
      },
      502,
    );
  }

  let insights: GeminiInsightsPayload;
  try {
    insights = JSON.parse(stripJsonFences(rawText)) as GeminiInsightsPayload;
  } catch {
    return jsonResponse(
      {
        status: "error",
        code: "GEMINI_INVALID_JSON",
        message: "A resposta do Gemini não pôde ser interpretada como JSON válido.",
        raw_preview: rawText.slice(0, 500),
      },
      502,
    );
  }

  const periodMonth = String((fleetContext as { period_month?: string }).period_month ?? new Date().toISOString().slice(0, 7));
  const summaryMarkdown = buildSummaryMarkdown(insights, periodMonth);

  // 4. Persist auditable insight into public.ai_insights (under caller's RLS)
  const { data: savedRow, error: insertError } = await supabase
    .from("ai_insights")
    .insert({
      tenant_id: targetTenantId,
      period_month: periodMonth,
      model_used: MODEL_NAME,
      deterministic_context: fleetContext,
      insights_json: insights,
      summary_markdown: summaryMarkdown,
    })
    .select("id, created_at")
    .single();

  if (insertError) {
    return jsonResponse({
      status: "success",
      insight_id: null,
      persistence_warning: insertError.message,
      model_used: MODEL_NAME,
      period_month: periodMonth,
      context: fleetContext,
      insights,
      summary_markdown: summaryMarkdown,
    });
  }

  return jsonResponse({
    status: "success",
    insight_id: savedRow.id,
    created_at: savedRow.created_at,
    model_used: MODEL_NAME,
    period_month: periodMonth,
    context: fleetContext,
    insights,
    summary_markdown: summaryMarkdown,
  });
});
