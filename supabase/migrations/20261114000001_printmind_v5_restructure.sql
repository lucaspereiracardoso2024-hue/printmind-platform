-- ============================================================================
-- PRINTMIND V5.0 - ARCHITECTURE RESTRUCTURE MIGRATION
-- Safe, idempotent migration extending v4.1 (20261113000002_printmind_v41_gaps.sql)
-- Covers:
--   1. public.platform_settings (Pioneer slots <= 15, Asaas minimum charge rules)
--   2. public.tenants extensions + billing column protection trigger (auth.role()/auth.jwt())
--   3. public.printers per-machine contract overrides & odometer reset detection
--   4. public.client_units Windows/EXE/MSI OTA update metadata
--   5. public.closings & public.closing_printer_items immutable snapshot + lock trigger
--   6. public.sync_agent_telemetry v5.0 (state_only NULL levels, color/mono baseline adjustment)
--   7. public.portal_access_tokens & public.service_tickets + hardened SECURITY DEFINER RPCs
--   8. public.payment_webhook_events (Asaas idempotency)
-- ============================================================================

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ----------------------------------------------------------------------------
-- 1. PLATFORM SETTINGS (Pioneiros <= 15 & Regras Comerciais SaaS / Asaas)
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS public.platform_settings (
  id TEXT PRIMARY KEY DEFAULT 'default',
  pioneer_slots_total INT NOT NULL DEFAULT 15,
  pioneer_slots_used INT NOT NULL DEFAULT 0,
  enforce_min_prepaid_recharge BOOLEAN NOT NULL DEFAULT FALSE,
  min_prepaid_recharge_brl NUMERIC(10, 2) NOT NULL DEFAULT 30.00,
  asaas_min_charge_brl NUMERIC(10, 2) NOT NULL DEFAULT 5.00,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO public.platform_settings (
  id, pioneer_slots_total, pioneer_slots_used, enforce_min_prepaid_recharge, min_prepaid_recharge_brl, asaas_min_charge_brl
)
VALUES ('default', 15, 0, FALSE, 30.00, 5.00)
ON CONFLICT (id) DO NOTHING;

ALTER TABLE public.platform_settings ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS "Authenticated users can read platform_settings" ON public.platform_settings;
CREATE POLICY "Authenticated users can read platform_settings"
  ON public.platform_settings
  FOR SELECT
  TO authenticated
  USING (TRUE);

DROP POLICY IF EXISTS "Master admin can update platform_settings" ON public.platform_settings;
CREATE POLICY "Master admin can update platform_settings"
  ON public.platform_settings
  FOR ALL
  TO authenticated
  USING (lower(COALESCE(auth.jwt()->>'email', '')) = 'lucaspereiracardoso2024@gmail.com')
  WITH CHECK (lower(COALESCE(auth.jwt()->>'email', '')) = 'lucaspereiracardoso2024@gmail.com');

-- ----------------------------------------------------------------------------
-- 2. TENANTS: Cidade/UF (PIX EMVCo real), Pioneiros, Trial & Proteção Financeira
-- ----------------------------------------------------------------------------
ALTER TABLE public.tenants ADD COLUMN IF NOT EXISTS city VARCHAR(80);
ALTER TABLE public.tenants ADD COLUMN IF NOT EXISTS state_uf VARCHAR(2);
ALTER TABLE public.tenants ADD COLUMN IF NOT EXISTS is_pioneer_tenant BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE public.tenants ADD COLUMN IF NOT EXISTS pioneer_courtesy_active BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE public.tenants ADD COLUMN IF NOT EXISTS trial_ends_at TIMESTAMPTZ;

ALTER TABLE public.tenants DROP CONSTRAINT IF EXISTS tenants_subscription_status_check;
ALTER TABLE public.tenants
  ADD CONSTRAINT tenants_subscription_status_check
  CHECK (subscription_status::text = ANY (ARRAY['active', 'past_due', 'canceled', 'trialing', 'pending_payment']));

ALTER TABLE public.tenants DROP CONSTRAINT IF EXISTS tenants_operation_mode_check;
ALTER TABLE public.tenants
  ADD CONSTRAINT tenants_operation_mode_check
  CHECK (operation_mode::text = ANY (ARRAY['outsourcing', 'locacao', 'grafica', 'hibrido']));

-- Trigger: block client-side self-upgrade / tampering of plan, status, or balances via PostgREST.
-- Uses auth.role() and auth.jwt() with safe service_role/postgres fallbacks so webhooks and migrations work.
CREATE OR REPLACE FUNCTION public.protect_tenant_billing_columns()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
  v_jwt jsonb;
  v_role text;
  v_email text;
  v_is_privileged boolean := FALSE;
BEGIN
  v_jwt := COALESCE(auth.jwt(), '{}'::jsonb);
  v_role := COALESCE(
    NULLIF(auth.role(), ''),
    NULLIF(v_jwt->>'role', ''),
    NULLIF(current_setting('request.jwt.claim.role', true), ''),
    ''
  );
  v_email := lower(TRIM(COALESCE(v_jwt->>'email', '')));

  IF v_role = 'service_role'
     OR current_user IN ('postgres', 'supabase_admin', 'service_role')
     OR EXISTS (SELECT 1 FROM pg_roles WHERE rolname = current_user AND rolsuper)
     OR v_email = 'lucaspereiracardoso2024@gmail.com' THEN
    v_is_privileged := TRUE;
  END IF;

  IF NOT v_is_privileged THEN
    IF NEW.plan_tier IS DISTINCT FROM OLD.plan_tier
       OR NEW.subscription_status IS DISTINCT FROM OLD.subscription_status
       OR NEW.max_printers IS DISTINCT FROM OLD.max_printers
       OR NEW.prepaid_credit_balance IS DISTINCT FROM OLD.prepaid_credit_balance
       OR NEW.is_pioneer_tenant IS DISTINCT FROM OLD.is_pioneer_tenant
       OR NEW.pioneer_courtesy_active IS DISTINCT FROM OLD.pioneer_courtesy_active
       OR NEW.gateway_customer_id IS DISTINCT FROM OLD.gateway_customer_id
       OR NEW.gateway_subscription_id IS DISTINCT FROM OLD.gateway_subscription_id
       OR NEW.baas_onboarding_status IS DISTINCT FROM OLD.baas_onboarding_status
       OR NEW.trial_ends_at IS DISTINCT FROM OLD.trial_ends_at THEN
      RAISE EXCEPTION 'Operação não autorizada: alterações de plano, status financeiro ou saldo devem passar pelo checkout/webhook Asaas.'
        USING ERRCODE = '42501';
    END IF;
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_protect_tenant_billing_columns ON public.tenants;
CREATE TRIGGER trg_protect_tenant_billing_columns
  BEFORE UPDATE ON public.tenants
  FOR EACH ROW
  EXECUTE FUNCTION public.protect_tenant_billing_columns();

-- Clamp any legacy pioneer_courtesy_active tenants whose max_printers exceeded the < R$ 5,00 courtesy cap (e.g. 500)
DO $$
DECLARE
  v_min_cents INT := 500;
BEGIN
  SELECT GREATEST(0, ROUND(COALESCE(asaas_min_charge_brl, 5.00) * 100))::INT
  INTO v_min_cents
  FROM public.platform_settings
  WHERE id = 'default'
  LIMIT 1;

  v_min_cents := COALESCE(v_min_cents, 500);

  UPDATE public.tenants
  SET max_printers = CASE lower(COALESCE(plan_tier, 'trial'))
    WHEN 'starter' THEN 2 + GREATEST(0, FLOOR(GREATEST(0, v_min_cents - 1)::NUMERIC / 100)::INT)
    WHEN 'pro' THEN 2 + GREATEST(0, FLOOR(GREATEST(0, v_min_cents - 1)::NUMERIC / 400)::INT)
    WHEN 'enterprise' THEN 2 + GREATEST(0, FLOOR(GREATEST(0, v_min_cents - 1)::NUMERIC / 600)::INT)
    ELSE 2
  END
  WHERE pioneer_courtesy_active = TRUE
    AND max_printers > CASE lower(COALESCE(plan_tier, 'trial'))
      WHEN 'starter' THEN 2 + GREATEST(0, FLOOR(GREATEST(0, v_min_cents - 1)::NUMERIC / 100)::INT)
      WHEN 'pro' THEN 2 + GREATEST(0, FLOOR(GREATEST(0, v_min_cents - 1)::NUMERIC / 400)::INT)
      WHEN 'enterprise' THEN 2 + GREATEST(0, FLOOR(GREATEST(0, v_min_cents - 1)::NUMERIC / 600)::INT)
      ELSE 2
    END;
END $$;

-- ----------------------------------------------------------------------------
-- 3. PRINTERS: Contrato por Máquina & Detecção de Reset Físico de Odômetro
-- ----------------------------------------------------------------------------
ALTER TABLE public.printers ADD COLUMN IF NOT EXISTS billing_mode VARCHAR(30) NOT NULL DEFAULT 'inherit_client';
ALTER TABLE public.printers DROP CONSTRAINT IF EXISTS printers_billing_mode_check;
ALTER TABLE public.printers
  ADD CONSTRAINT printers_billing_mode_check
  CHECK (billing_mode::text = ANY (ARRAY['inherit_client', 'fixed_only', 'fixed_plus_franchise', 'per_page_only', 'exempt']));

ALTER TABLE public.printers ADD COLUMN IF NOT EXISTS contract_base_rate NUMERIC(12, 2);
ALTER TABLE public.printers ADD COLUMN IF NOT EXISTS contract_franchise_mono INT;
ALTER TABLE public.printers ADD COLUMN IF NOT EXISTS contract_extra_mono NUMERIC(10, 4);
ALTER TABLE public.printers ADD COLUMN IF NOT EXISTS contract_franchise_color INT;
ALTER TABLE public.printers ADD COLUMN IF NOT EXISTS contract_extra_color NUMERIC(10, 4);
ALTER TABLE public.printers ADD COLUMN IF NOT EXISTS counter_reset_detected BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE public.printers ADD COLUMN IF NOT EXISTS last_reset_at TIMESTAMPTZ;
ALTER TABLE public.printers ADD COLUMN IF NOT EXISTS replaced_delta_mono BIGINT NOT NULL DEFAULT 0;
ALTER TABLE public.printers ADD COLUMN IF NOT EXISTS replaced_delta_color BIGINT NOT NULL DEFAULT 0;

-- ----------------------------------------------------------------------------
-- 4. CLIENT UNITS: OTA Windows MSI/EXE Update Columns
-- ----------------------------------------------------------------------------
ALTER TABLE public.client_units ADD COLUMN IF NOT EXISTS update_msi_url TEXT;
ALTER TABLE public.client_units ADD COLUMN IF NOT EXISTS update_sha256 VARCHAR(128);
ALTER TABLE public.client_units ADD COLUMN IF NOT EXISTS update_exe_url TEXT;
ALTER TABLE public.client_units ADD COLUMN IF NOT EXISTS update_exe_sha256 VARCHAR(128);
ALTER TABLE public.client_units ADD COLUMN IF NOT EXISTS update_version VARCHAR(30);

-- ----------------------------------------------------------------------------
-- 5. CLOSINGS & CLOSING PRINTER ITEMS (Snapshot Financeiro Imutável)
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS public.closings (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id UUID NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,
  client_id UUID NOT NULL REFERENCES public.clients(id) ON DELETE CASCADE,
  status VARCHAR(30) NOT NULL DEFAULT 'aberto',
  total_pages BIGINT NOT NULL DEFAULT 0,
  total_amount NUMERIC(12, 2) NOT NULL DEFAULT 0.00,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE public.closings ADD COLUMN IF NOT EXISTS period_month VARCHAR(7);
ALTER TABLE public.closings ADD COLUMN IF NOT EXISTS client_name_snapshot VARCHAR(150);
ALTER TABLE public.closings ADD COLUMN IF NOT EXISTS client_cnpj_snapshot VARCHAR(30);
ALTER TABLE public.closings ADD COLUMN IF NOT EXISTS pricing_rules_snapshot JSONB NOT NULL DEFAULT '{}'::JSONB;
ALTER TABLE public.closings ADD COLUMN IF NOT EXISTS printer_items_snapshot JSONB NOT NULL DEFAULT '[]'::JSONB;
ALTER TABLE public.closings ADD COLUMN IF NOT EXISTS due_date DATE;
ALTER TABLE public.closings ADD COLUMN IF NOT EXISTS snapshot_frozen BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE public.closings ADD COLUMN IF NOT EXISTS frozen_at TIMESTAMPTZ;
ALTER TABLE public.closings ADD COLUMN IF NOT EXISTS snapshot_hash VARCHAR(64);
ALTER TABLE public.closings ADD COLUMN IF NOT EXISTS pix_payload TEXT;
ALTER TABLE public.closings ADD COLUMN IF NOT EXISTS asaas_payment_id VARCHAR(120);
ALTER TABLE public.closings ADD COLUMN IF NOT EXISTS invoice_url TEXT;

CREATE INDEX IF NOT EXISTS idx_closings_tenant_period
  ON public.closings(tenant_id, period_month);

CREATE TABLE IF NOT EXISTS public.closing_printer_items (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id UUID NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,
  closing_id UUID NOT NULL REFERENCES public.closings(id) ON DELETE CASCADE,
  printer_id UUID REFERENCES public.printers(id) ON DELETE SET NULL,
  model_snapshot VARCHAR(150) NOT NULL,
  serial_snapshot VARCHAR(150) NOT NULL,
  unit_snapshot VARCHAR(120),
  department_snapshot VARCHAR(120),
  initial_mono BIGINT NOT NULL DEFAULT 0,
  final_mono BIGINT NOT NULL DEFAULT 0,
  delta_mono BIGINT NOT NULL DEFAULT 0,
  initial_color BIGINT NOT NULL DEFAULT 0,
  final_color BIGINT NOT NULL DEFAULT 0,
  delta_color BIGINT NOT NULL DEFAULT 0,
  applied_billing_mode VARCHAR(30) NOT NULL DEFAULT 'inherit_client',
  subtotal_brl NUMERIC(12, 2) NOT NULL DEFAULT 0.00,
  is_exempt BOOLEAN NOT NULL DEFAULT FALSE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_closing_printer_items_closing
  ON public.closing_printer_items(closing_id);

ALTER TABLE public.closing_printer_items ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS "Tenant users can manage closing_printer_items" ON public.closing_printer_items;
CREATE POLICY "Tenant users can manage closing_printer_items"
  ON public.closing_printer_items
  FOR ALL
  TO authenticated
  USING (
    lower(COALESCE(auth.jwt()->>'email', '')) = 'lucaspereiracardoso2024@gmail.com'
    OR tenant_id IN (SELECT tenant_id FROM public.profiles WHERE id = auth.uid())
  )
  WITH CHECK (
    lower(COALESCE(auth.jwt()->>'email', '')) = 'lucaspereiracardoso2024@gmail.com'
    OR tenant_id IN (SELECT tenant_id FROM public.profiles WHERE id = auth.uid())
  );

-- Immutability trigger: once frozen, financial items, totals, rules and hash cannot change.
CREATE OR REPLACE FUNCTION public.prevent_frozen_closing_mutation()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
  v_jwt jsonb;
  v_role text;
BEGIN
  v_jwt := COALESCE(auth.jwt(), '{}'::jsonb);
  v_role := COALESCE(
    NULLIF(auth.role(), ''),
    NULLIF(v_jwt->>'role', ''),
    NULLIF(current_setting('request.jwt.claim.role', true), ''),
    ''
  );

  IF v_role = 'service_role' OR current_user IN ('postgres', 'supabase_admin', 'service_role') THEN
    IF TG_OP = 'DELETE' THEN
      RETURN OLD;
    END IF;
    RETURN NEW;
  END IF;

  IF TG_OP = 'DELETE' THEN
    IF COALESCE(OLD.snapshot_frozen, FALSE) OR OLD.status IN ('congelado', 'faturado', 'pago') THEN
      RAISE EXCEPTION 'Fechamento congelado/faturado é imutável e não pode ser excluído.'
        USING ERRCODE = '42501';
    END IF;
    RETURN OLD;
  END IF;

  IF COALESCE(OLD.snapshot_frozen, FALSE) OR OLD.status IN ('congelado', 'faturado', 'pago') THEN
    IF NEW.total_pages IS DISTINCT FROM OLD.total_pages
       OR NEW.total_amount IS DISTINCT FROM OLD.total_amount
       OR NEW.client_id IS DISTINCT FROM OLD.client_id
       OR NEW.period_month IS DISTINCT FROM OLD.period_month
       OR NEW.client_name_snapshot IS DISTINCT FROM OLD.client_name_snapshot
       OR NEW.client_cnpj_snapshot IS DISTINCT FROM OLD.client_cnpj_snapshot
       OR NEW.pricing_rules_snapshot IS DISTINCT FROM OLD.pricing_rules_snapshot
       OR NEW.printer_items_snapshot IS DISTINCT FROM OLD.printer_items_snapshot
       OR NEW.snapshot_hash IS DISTINCT FROM OLD.snapshot_hash
       OR (OLD.snapshot_frozen = TRUE AND NEW.snapshot_frozen = FALSE) THEN
      RAISE EXCEPTION 'Fechamento congelado é imutável: não é permitido alterar valores, itens ou regras históricas.'
        USING ERRCODE = '42501';
    END IF;
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_prevent_frozen_closing_mutation ON public.closings;
CREATE TRIGGER trg_prevent_frozen_closing_mutation
  BEFORE UPDATE OR DELETE ON public.closings
  FOR EACH ROW
  EXECUTE FUNCTION public.prevent_frozen_closing_mutation();

CREATE OR REPLACE FUNCTION public.prevent_frozen_closing_items_mutation()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
  v_frozen boolean := FALSE;
BEGIN
  IF current_user IN ('postgres', 'supabase_admin', 'service_role')
     OR COALESCE(auth.role(), '') = 'service_role' THEN
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
  END IF;

  SELECT COALESCE(snapshot_frozen, FALSE) OR status IN ('congelado', 'faturado', 'pago')
  INTO v_frozen
  FROM public.closings
  WHERE id = OLD.closing_id;

  IF COALESCE(v_frozen, FALSE) THEN
    RAISE EXCEPTION 'Itens de fechamento congelado são imutáveis.'
      USING ERRCODE = '42501';
  END IF;

  IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_prevent_frozen_closing_items_mutation ON public.closing_printer_items;
CREATE TRIGGER trg_prevent_frozen_closing_items_mutation
  BEFORE UPDATE OR DELETE ON public.closing_printer_items
  FOR EACH ROW
  EXECUTE FUNCTION public.prevent_frozen_closing_items_mutation();

-- ----------------------------------------------------------------------------
-- 6. RPC: public.sync_agent_telemetry (v5.0)
--    Fixes:
--    - state_only supplies store NULL numeric levels & track ok/low/empty alerts
--    - mono vs color never defaults mono=total when color > 0
--    - existing printer receiving color > 0 for first time adjusts initial_color_pages
--      and initial_mono_pages so historical color pages are not billed retroactively
--    - flags counter_reset_detected safely without losing odometer
--    - returns Windows MSI/EXE OTA update directives alongside Linux OTA
-- ----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION public.sync_agent_telemetry(
  p_pairing_token text,
  p_payload jsonb
)
RETURNS jsonb
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
  v_unit RECORD;
  v_dev jsonb;
  v_prt RECORD;
  v_prev_reading RECORD;
  v_serial text;
  v_device_uuid text;
  v_model text;
  v_vendor text;
  v_vendor_pen int;
  v_ip text;
  v_usb text;
  v_mac text;
  v_source text;
  v_odometer_source text;
  v_status text;
  v_total bigint;
  v_mono bigint;
  v_color bigint;
  v_raw_mono_txt text;
  v_new_init_mono bigint;
  v_new_init_color bigint;
  v_counter_reset boolean;
  v_duplex bigint;
  v_scan bigint;
  v_label bigint;
  v_print_len bigint;
  v_prev_total bigint;
  v_delta_pages bigint;
  v_is_color_capable boolean;
  v_has_cmy_supply boolean := false;
  v_wpp_at_risk boolean;
  v_error_flags text[];
  v_supplies jsonb;
  v_meta jsonb;
  v_prev_meta jsonb;
  v_meta_key text;
  v_synced int := 0;
  v_jobs_synced int := 0;
  v_prt_id uuid;
  v_k smallint := null;
  v_c smallint := null;
  v_m smallint := null;
  v_y smallint := null;
  v_sup_elem jsonb;
  v_sup_name_raw text;
  v_sup_name_lc text;
  v_sup_color_field text;
  v_sup_color_norm text;
  v_sup_class_raw text;
  v_sup_class text;
  v_level_state text;
  v_meas_mode text;
  v_supply_state text;
  v_sup_pct int;
  v_raw_level int;
  v_prev_pct int;
  v_sup_serial text;
  v_prev_serial text;
  v_waste_brl numeric(10, 2);
  v_host_name text;
  v_host_os text;
  v_agent_ver text;
  v_wpp_enabled boolean;
  v_wpp_policy_state int;
  v_idem_key text;
  v_now_iso text;
  v_job jsonb;
  v_job_printer_id uuid;
  v_job_color_capable boolean;
  v_job_serial text;
  v_job_uuid text;
  v_job_ip text;
  v_job_usb text;
  v_job_period text;
  v_job_user text;
  v_job_machine text;
  v_job_dept text;
  v_job_source text;
  v_job_total bigint;
  v_job_mono bigint;
  v_job_color bigint;
  v_job_duplex bigint;
  v_job_count int;
  v_job_is_delta boolean;
BEGIN
  v_host_name := SUBSTRING(COALESCE(NULLIF(TRIM(p_payload->'host_info'->>'hostname'), ''), 'Host') FROM 1 FOR 100);
  v_host_os := SUBSTRING(NULLIF(TRIM(COALESCE(p_payload->'host_info'->>'os', p_payload->>'host_os', '')), '') FROM 1 FOR 80);
  v_agent_ver := SUBSTRING(COALESCE(NULLIF(TRIM(p_payload->>'agent_version'), ''), '4.2.0') FROM 1 FOR 20);
  v_wpp_enabled := CASE
    WHEN lower(COALESCE(p_payload->'wpp_readiness'->>'wpp_enabled', p_payload->'host_info'->>'wpp_enabled', p_payload->>'wpp_enabled', '')) IN ('true', 't', '1') THEN true
    WHEN lower(COALESCE(p_payload->'wpp_readiness'->>'wpp_enabled', p_payload->'host_info'->>'wpp_enabled', p_payload->>'wpp_enabled', '')) IN ('false', 'f', '0') THEN false
    ELSE NULL
  END;
  v_wpp_policy_state := CASE
    WHEN COALESCE(p_payload->'wpp_readiness'->>'wpp_policy_state', p_payload->'host_info'->>'wpp_policy_state', p_payload->>'wpp_policy_state', '') ~ '^-?[0-9]+$'
      THEN (COALESCE(p_payload->'wpp_readiness'->>'wpp_policy_state', p_payload->'host_info'->>'wpp_policy_state', p_payload->>'wpp_policy_state'))::int
    ELSE NULL
  END;
  v_now_iso := to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"');

  -- 1. Locate Unit / Client by pairing token
  SELECT * INTO v_unit
  FROM public.client_units
  WHERE pairing_token = p_pairing_token
  LIMIT 1;

  IF NOT FOUND THEN
    RETURN jsonb_build_object(
      'status', 'error',
      'message', 'Token de pareamento inválido ou não encontrado'
    );
  END IF;

  UPDATE public.client_units
  SET
    agent_status = 'online',
    agent_last_seen = now(),
    agent_hostname = v_host_name,
    agent_version = v_agent_ver,
    host_os = COALESCE(v_host_os, host_os),
    wpp_enabled = COALESCE(v_wpp_enabled, wpp_enabled),
    wpp_policy_state = COALESCE(v_wpp_policy_state, wpp_policy_state)
  WHERE id = v_unit.id
  RETURNING * INTO v_unit;

  -- 2. Process reported devices
  IF p_payload->'devices' IS NOT NULL AND jsonb_typeof(p_payload->'devices') = 'array' AND jsonb_array_length(p_payload->'devices') > 0 THEN
    FOR v_dev IN SELECT * FROM jsonb_array_elements(p_payload->'devices') LOOP
      v_serial := SUBSTRING(COALESCE(NULLIF(TRIM(v_dev->>'serial_number'), ''), NULLIF(TRIM(v_dev->>'serial'), '')) FROM 1 FOR 150);
      v_device_uuid := NULLIF(TRIM(v_dev->>'device_uuid'), '');
      v_ip := SUBSTRING(COALESCE(NULLIF(TRIM(v_dev->>'ip_address'), ''), NULLIF(TRIM(v_dev->>'ip'), ''), '0.0.0.0') FROM 1 FOR 45);
      v_usb := SUBSTRING(COALESCE(v_dev->>'usb_port', '') FROM 1 FOR 150);
      v_mac := SUBSTRING(COALESCE(v_dev->>'mac_address', '') FROM 1 FOR 100);
      v_model := SUBSTRING(COALESCE(NULLIF(TRIM(v_dev->>'model'), ''), 'Impressora Autônoma') FROM 1 FOR 150);
      v_vendor := SUBSTRING(COALESCE(NULLIF(TRIM(v_dev->>'vendor'), ''), 'Genérico') FROM 1 FOR 60);
      v_vendor_pen := CASE WHEN COALESCE(v_dev->>'vendor_pen', '') ~ '^[0-9]+$' THEN (v_dev->>'vendor_pen')::int ELSE NULL END;
      v_source := SUBSTRING(COALESCE(NULLIF(TRIM(v_dev->>'discovery_source'), ''), 'snmp_v2c') FROM 1 FOR 50);
      v_odometer_source := SUBSTRING(NULLIF(TRIM(COALESCE(v_dev->>'odometer_source', '')), '') FROM 1 FOR 40);

      v_total := GREATEST(0, COALESCE((v_dev->'counters'->>'lifetime_engine_total')::bigint, (v_dev->>'total_pages')::bigint, 0));
      v_color := GREATEST(0, COALESCE((v_dev->'counters'->>'color_total')::bigint, (v_dev->>'color_pages')::bigint, 0));
      v_raw_mono_txt := NULLIF(TRIM(COALESCE(v_dev->'counters'->>'mono_total', v_dev->>'mono_pages', '')), '');
      IF v_raw_mono_txt IS NOT NULL AND v_raw_mono_txt ~ '^-?[0-9]+$' THEN
        v_mono := GREATEST(0, v_raw_mono_txt::bigint);
        -- Guard against legacy SNMP payload setting mono_total = total while color_total > 0
        IF v_color > 0 AND v_mono = v_total AND v_total >= v_color THEN
          v_mono := GREATEST(0, v_total - v_color);
        END IF;
      ELSE
        v_mono := GREATEST(0, v_total - v_color);
      END IF;
      IF v_total < (v_mono + v_color) THEN
        v_total := v_mono + v_color;
      END IF;

      v_duplex := GREATEST(0, COALESCE((v_dev->'counters'->>'duplex_sheets')::bigint, (v_dev->>'duplex_sheets')::bigint, 0));
      v_scan := GREATEST(0, COALESCE((v_dev->'counters'->>'scan_total')::bigint, (v_dev->>'scan_pages')::bigint, 0));
      v_label := GREATEST(0, COALESCE((v_dev->'counters'->>'label_count')::bigint, (v_dev->>'label_count')::bigint, 0));
      v_print_len := GREATEST(0, COALESCE(
        (v_dev->'counters'->>'print_length_cm')::bigint,
        (v_dev->>'print_length_cm')::bigint,
        CASE WHEN COALESCE(v_dev->'counters'->>'print_length_mm', '') ~ '^[0-9]+$' THEN ((v_dev->'counters'->>'print_length_mm')::bigint / 10) ELSE NULL END,
        0
      ));

      v_supplies := CASE WHEN jsonb_typeof(v_dev->'supplies') = 'array' THEN v_dev->'supplies' ELSE '[]'::jsonb END;
      v_error_flags := COALESCE(
        (SELECT array_agg(elem) FROM jsonb_array_elements_text(
          CASE
            WHEN jsonb_typeof(v_dev->'error_flags') = 'array' THEN v_dev->'error_flags'
            WHEN jsonb_typeof(v_dev->'status_reasons') = 'array' THEN v_dev->'status_reasons'
            ELSE '[]'::jsonb
          END
        ) AS elem),
        '{}'::text[]
      );

      v_status := lower(TRIM(COALESCE(v_dev->>'status', 'online')));
      IF v_status NOT IN ('online', 'offline', 'error', 'maintenance') THEN
        IF cardinality(v_error_flags) > 0 THEN
          v_status := 'error';
        ELSE
          v_status := 'online';
        END IF;
      END IF;

      -- Find existing printer in tenant
      SELECT * INTO v_prt
      FROM public.printers
      WHERE tenant_id = v_unit.tenant_id
        AND (
          (v_device_uuid IS NOT NULL AND device_uuid = v_device_uuid)
          OR (v_serial IS NOT NULL AND v_serial <> '' AND serial = v_serial)
          OR (ip = v_ip AND v_ip <> '0.0.0.0' AND v_ip <> '127.0.0.1')
          OR (v_usb <> '' AND usb_port = v_usb)
        )
      ORDER BY
        CASE
          WHEN v_device_uuid IS NOT NULL AND device_uuid = v_device_uuid THEN 1
          WHEN v_serial IS NOT NULL AND v_serial <> '' AND serial = v_serial THEN 2
          ELSE 3
        END
      LIMIT 1;

      -- First pass over supplies: parse CMYK numeric percentages strictly (NULL when state_only or unknown)
      -- while also detecting CMY color capability for state_only tanks.
      v_k := null; v_c := null; v_m := null; v_y := null;
      v_has_cmy_supply := false;
      IF jsonb_array_length(v_supplies) > 0 THEN
        FOR v_sup_elem IN SELECT * FROM jsonb_array_elements(v_supplies) LOOP
          v_sup_color_field := lower(TRIM(COALESCE(v_sup_elem->>'color', '')));
          v_sup_name_raw := COALESCE(
            NULLIF(TRIM(v_sup_elem->>'name'), ''),
            NULLIF(TRIM(v_sup_elem->>'description'), ''),
            NULLIF(v_sup_color_field, ''),
            'Suprimento'
          );
          v_sup_name_lc := lower(v_sup_name_raw);
          v_sup_class_raw := lower(TRIM(COALESCE(v_sup_elem->>'supply_class', v_sup_elem->>'class', '')));

          IF v_sup_class_raw IN ('waste', '4', 'receptacle', 'waste_box')
             OR v_sup_color_field IN ('waste', 'residuo', 'maintenance', 'manutencao')
             OR v_sup_name_lc ~ '(waste|residuo|resíduo|manuten[çc][ãa]o|maintenance box|almofada|descarte|lixeira)' THEN
            v_sup_class := 'waste';
          ELSE
            v_sup_class := 'consumed';
          END IF;

          v_level_state := lower(TRIM(COALESCE(NULLIF(v_sup_elem->>'level_state', ''), 'exact')));
          v_meas_mode := lower(TRIM(COALESCE(NULLIF(v_sup_elem->>'measurement_mode', ''), CASE WHEN v_level_state = 'state_only' THEN 'state_only' ELSE 'percentage' END)));
          v_raw_level := CASE
            WHEN COALESCE(v_sup_elem->>'level', '') ~ '^-?[0-9]+$' THEN (v_sup_elem->>'level')::int
            ELSE NULL
          END;
          IF v_meas_mode <> 'state_only'
             AND v_level_state = 'exact'
             AND (v_sup_elem->>'percent') IS NOT NULL
             AND (v_sup_elem->>'percent') <> 'null'
             AND (v_sup_elem->>'percent') ~ '^-?[0-9]+$'
             AND (v_raw_level IS NULL OR v_raw_level >= 0) THEN
            v_sup_pct := (v_sup_elem->>'percent')::int;
            IF v_sup_pct < 0 OR v_sup_pct > 100 THEN
              v_sup_pct := NULL;
            END IF;
          ELSE
            v_sup_pct := NULL;
          END IF;

          IF v_sup_class = 'consumed' THEN
            IF v_sup_color_field IN ('black', 'preto', 'preta', 'k', 'bk', 'pb', 'p&b') THEN
              v_sup_color_norm := 'black';
            ELSIF v_sup_color_field IN ('cyan', 'ciano', 'c') THEN
              v_sup_color_norm := 'cyan';
            ELSIF v_sup_color_field IN ('magenta', 'm') THEN
              v_sup_color_norm := 'magenta';
            ELSIF v_sup_color_field IN ('yellow', 'amarelo', 'amarela', 'y') THEN
              v_sup_color_norm := 'yellow';
            ELSIF v_sup_name_lc ~ '(^|[^a-z])(black|preto|preta|pb|p&b|bk|k)([^a-z]|$)' THEN
              v_sup_color_norm := 'black';
            ELSIF v_sup_name_lc ~ '(^|[^a-z])(cyan|ciano|c)([^a-z]|$)' THEN
              v_sup_color_norm := 'cyan';
            ELSIF v_sup_name_lc ~ '(^|[^a-z])(magenta|m)([^a-z]|$)' THEN
              v_sup_color_norm := 'magenta';
            ELSIF v_sup_name_lc ~ '(^|[^a-z])(yellow|amarelo|amarela|y)([^a-z]|$)' THEN
              v_sup_color_norm := 'yellow';
            ELSE
              v_sup_color_norm := 'other';
            END IF;

            IF v_sup_color_norm IN ('cyan', 'magenta', 'yellow') THEN
              v_has_cmy_supply := TRUE;
            END IF;

            IF v_sup_pct IS NOT NULL THEN
              IF v_sup_color_norm = 'black' AND v_k IS NULL THEN
                v_k := v_sup_pct::smallint;
              ELSIF v_sup_color_norm = 'cyan' AND v_c IS NULL THEN
                v_c := v_sup_pct::smallint;
              ELSIF v_sup_color_norm = 'magenta' AND v_m IS NULL THEN
                v_m := v_sup_pct::smallint;
              ELSIF v_sup_color_norm = 'yellow' AND v_y IS NULL THEN
                v_y := v_sup_pct::smallint;
              END IF;
            END IF;
          END IF;
        END LOOP;
      END IF;

      -- Physical color capability check (includes state_only CMY tanks reported via color code/name)
      IF (v_dev->>'is_color_capable') IS NOT NULL AND lower(v_dev->>'is_color_capable') IN ('true', 'false', 't', 'f') THEN
        v_is_color_capable := (v_dev->>'is_color_capable')::boolean;
      ELSIF v_prt.id IS NOT NULL THEN
        v_is_color_capable := COALESCE(v_prt.is_color_capable, false) OR (v_color > 0) OR v_has_cmy_supply OR (v_c IS NOT NULL) OR (v_m IS NOT NULL) OR (v_y IS NOT NULL);
      ELSE
        v_is_color_capable := (v_color > 0) OR v_has_cmy_supply OR (v_c IS NOT NULL) OR (v_m IS NOT NULL) OR (v_y IS NOT NULL);
      END IF;

      IF NOT v_is_color_capable THEN
        v_color := 0;
        v_mono := GREATEST(v_mono, v_total);
      END IF;

      IF (v_dev->>'wpp_at_risk') IS NOT NULL AND lower(v_dev->>'wpp_at_risk') IN ('true', 'false', 't', 'f') THEN
        v_wpp_at_risk := (v_dev->>'wpp_at_risk')::boolean;
      ELSIF jsonb_typeof(p_payload->'wpp_readiness'->'impacted_printers') = 'array'
            AND EXISTS (
              SELECT 1
              FROM jsonb_array_elements_text(p_payload->'wpp_readiness'->'impacted_printers') AS imp
              WHERE lower(imp) = lower(v_model)
                 OR (v_serial IS NOT NULL AND v_serial <> '' AND lower(imp) LIKE '%' || lower(v_serial) || '%')
            ) THEN
        v_wpp_at_risk := true;
      ELSIF v_prt.id IS NOT NULL THEN
        v_wpp_at_risk := COALESCE(v_prt.wpp_at_risk, false);
      ELSE
        v_wpp_at_risk := false;
      END IF;

      IF v_prt.id IS NOT NULL THEN
        v_prev_total := COALESCE(v_prt.total_pages, v_total);
        v_meta := COALESCE(v_prt.supply_alerts_meta, '{}'::jsonb);

        -- Adjustment #1: If an existing printer receives color_pages > 0 for the first time
        -- (previously old color_pages = 0), align initial_color_pages and initial_mono_pages
        -- so pre-existing historical color impressions are never billed retroactively while keeping
        -- total billable mono growth intact.
        v_new_init_mono := COALESCE(v_prt.initial_mono_pages, 0);
        v_new_init_color := COALESCE(v_prt.initial_color_pages, 0);
        IF COALESCE(v_prt.color_pages, 0) = 0 AND v_color > 0 AND v_is_color_capable THEN
          v_new_init_color := v_color;
          v_new_init_mono := GREATEST(0, v_mono - GREATEST(0, v_total - COALESCE(v_prt.initial_total_pages, v_total)));
        END IF;

        v_counter_reset := (v_total > 0 AND (v_total + 10) < COALESCE(v_prt.total_pages, 0));

        UPDATE public.printers SET
          device_uuid = COALESCE(v_device_uuid, device_uuid),
          mac_address = CASE WHEN v_mac <> '' THEN v_mac ELSE mac_address END,
          vendor_pen = COALESCE(v_vendor_pen, vendor_pen),
          vendor = CASE WHEN vendor IS NULL OR vendor = '' OR vendor = 'Genérico' THEN v_vendor ELSE vendor END,
          is_color_capable = v_is_color_capable,
          wpp_at_risk = v_wpp_at_risk,
          initial_mono_pages = v_new_init_mono,
          initial_color_pages = v_new_init_color,
          total_pages = GREATEST(COALESCE(total_pages, 0), v_total),
          mono_pages = GREATEST(
            CASE
              WHEN COALESCE(v_prt.color_pages, 0) = 0 AND v_color > 0 AND v_is_color_capable THEN v_mono
              ELSE COALESCE(mono_pages, 0)
            END,
            v_mono
          ),
          color_pages = CASE WHEN v_is_color_capable THEN GREATEST(COALESCE(color_pages, 0), v_color) ELSE 0 END,
          counter_reset_detected = CASE WHEN v_counter_reset THEN TRUE ELSE counter_reset_detected END,
          last_reset_at = CASE WHEN v_counter_reset THEN now() ELSE last_reset_at END,
          duplex_sheets = GREATEST(COALESCE(duplex_sheets, 0), v_duplex),
          scan_pages = GREATEST(COALESCE(scan_pages, 0), v_scan),
          label_count = GREATEST(COALESCE(label_count, 0), v_label),
          print_length_cm = GREATEST(COALESCE(print_length_cm, 0), v_print_len),
          status = v_status,
          error_flags = v_error_flags,
          supplies = CASE WHEN jsonb_array_length(v_supplies) > 0 THEN v_supplies ELSE supplies END,
          discovery_source = v_source,
          odometer_source = COALESCE(v_odometer_source, odometer_source),
          last_seen = now(),
          updated_at = now(),
          model = CASE WHEN model IS NULL OR model = '' OR model = 'Genérica' THEN v_model ELSE model END
        WHERE id = v_prt.id;
        v_prt_id := v_prt.id;

        IF v_counter_reset AND NOT EXISTS (
          SELECT 1 FROM public.supply_alerts
          WHERE printer_id = v_prt_id AND alert_type = 'counter_reset' AND resolved = false
        ) THEN
          INSERT INTO public.supply_alerts (
            tenant_id, printer_id, client_id, alert_type, severity, supply_name,
            previous_level_pct, new_level_pct, estimated_waste_brl, message, resolved, created_at
          ) VALUES (
            v_unit.tenant_id, v_prt_id, v_unit.client_id, 'counter_reset', 'warning', 'Odômetro Físico',
            NULL, NULL, 0,
            'Reset físico de odômetro detectado (' || COALESCE(v_prt.total_pages, 0)::text || ' -> ' || v_total::text || '). Revise o baseline ou registre troca de placa no faturamento.',
            false, now()
          );
        END IF;
      ELSE
        v_prev_total := v_total;
        v_meta := '{}'::jsonb;

        INSERT INTO public.printers (
          tenant_id, client_id, unit_id, unit, department, model, serial, ip, usb_port, mac_address,
          device_uuid, vendor_pen, vendor,
          initial_mono_pages, mono_pages, initial_color_pages, color_pages,
          initial_total_pages, total_pages, duplex_sheets, scan_pages, label_count, print_length_cm,
          is_color_capable, wpp_at_risk, status, error_flags, supplies, discovery_source, odometer_source,
          last_seen, updated_at
        ) VALUES (
          v_unit.tenant_id, v_unit.client_id, v_unit.id, v_unit.unit_name, 'Geral',
          v_model, COALESCE(v_serial, 'SN-' || substr(md5(random()::text), 1, 8)), v_ip, v_usb, v_mac,
          v_device_uuid, v_vendor_pen, v_vendor,
          v_mono, v_mono, v_color, v_color,
          v_total, v_total, v_duplex, v_scan, NULLIF(v_label, 0), NULLIF(v_print_len, 0),
          v_is_color_capable, v_wpp_at_risk, v_status, v_error_flags, v_supplies, v_source, v_odometer_source,
          now(), now()
        ) RETURNING id INTO v_prt_id;
      END IF;

      v_delta_pages := GREATEST(0, v_total - v_prev_total);

      SELECT k_level, c_level, m_level, y_level
      INTO v_prev_reading
      FROM public.telemetry_readings
      WHERE printer_id = v_prt_id
      ORDER BY recorded_at DESC
      LIMIT 1;

      v_idem_key := v_prt_id::text || '-' || to_char(now(), 'YYYY-MM-DD-HH24-MI');
      INSERT INTO public.telemetry_readings (
        tenant_id, printer_id, client_id, idempotency_key,
        total_pages, mono_pages, color_pages, duplex_sheets, scan_pages, label_count,
        c_level, m_level, y_level, k_level, supplies_snapshot, recorded_at
      ) VALUES (
        v_unit.tenant_id, v_prt_id, v_unit.client_id, v_idem_key,
        v_total, v_mono, v_color, v_duplex, v_scan, v_label,
        v_c, v_m, v_y, v_k, v_supplies, now()
      ) ON CONFLICT (idempotency_key) DO NOTHING;

      -- Second pass over supplies: percentage alerts + state_only low/empty alerts
      IF jsonb_array_length(v_supplies) > 0 THEN
        FOR v_sup_elem IN SELECT * FROM jsonb_array_elements(v_supplies) LOOP
          v_sup_color_field := lower(TRIM(COALESCE(v_sup_elem->>'color', '')));
          v_sup_name_raw := SUBSTRING(COALESCE(
            NULLIF(TRIM(v_sup_elem->>'name'), ''),
            NULLIF(TRIM(v_sup_elem->>'description'), ''),
            NULLIF(v_sup_color_field, ''),
            'Suprimento'
          ) FROM 1 FOR 120);
          v_sup_name_lc := lower(v_sup_name_raw);
          v_sup_class_raw := lower(TRIM(COALESCE(v_sup_elem->>'supply_class', v_sup_elem->>'class', '')));
          v_sup_serial := NULLIF(TRIM(COALESCE(v_sup_elem->>'serial_number', v_sup_elem->>'serial', '')), '');

          IF v_sup_class_raw IN ('waste', '4', 'receptacle', 'waste_box')
             OR v_sup_color_field IN ('waste', 'residuo', 'maintenance', 'manutencao')
             OR v_sup_name_lc ~ '(waste|residuo|resíduo|manuten[çc][ãa]o|maintenance box|almofada|descarte|lixeira)' THEN
            v_sup_class := 'waste';
          ELSE
            v_sup_class := 'consumed';
          END IF;

          v_level_state := lower(TRIM(COALESCE(NULLIF(v_sup_elem->>'level_state', ''), 'exact')));
          v_meas_mode := lower(TRIM(COALESCE(NULLIF(v_sup_elem->>'measurement_mode', ''), CASE WHEN v_level_state = 'state_only' THEN 'state_only' ELSE 'percentage' END)));
          v_supply_state := lower(TRIM(COALESCE(NULLIF(v_sup_elem->>'supply_state', ''), 'unknown')));
          v_raw_level := CASE
            WHEN COALESCE(v_sup_elem->>'level', '') ~ '^-?[0-9]+$' THEN (v_sup_elem->>'level')::int
            ELSE NULL
          END;

          -- State-only tank / printhead monitoring without inventing fake percentages
          IF v_meas_mode = 'state_only' OR v_level_state = 'state_only' THEN
            IF v_supply_state IN ('low', 'empty') THEN
              IF NOT EXISTS (
                SELECT 1 FROM public.supply_alerts
                WHERE printer_id = v_prt_id
                  AND alert_type IN ('toner_low', 'low_supply')
                  AND supply_name = v_sup_name_raw
                  AND resolved = false
                  AND created_at >= now() - interval '24 hours'
              ) THEN
                INSERT INTO public.supply_alerts (
                  tenant_id, printer_id, client_id, alert_type, severity, supply_name,
                  previous_level_pct, new_level_pct, estimated_waste_brl, message, resolved, created_at
                ) VALUES (
                  v_unit.tenant_id, v_prt_id, v_unit.client_id, 'low_supply',
                  CASE WHEN v_supply_state = 'empty' THEN 'critical' ELSE 'warning' END,
                  v_sup_name_raw, NULL, NULL, 0,
                  'Sensor físico reportou estado "' || v_supply_state || '" em ' || v_sup_name_raw || '. Verifique reabastecimento/inspeção.',
                  false, now()
                );
              END IF;
            ELSIF v_supply_state = 'ok' THEN
              UPDATE public.supply_alerts
              SET resolved = true
              WHERE printer_id = v_prt_id
                AND alert_type IN ('toner_low', 'low_supply')
                AND supply_name = v_sup_name_raw
                AND resolved = false;
            END IF;
            CONTINUE;
          END IF;

          IF v_level_state = 'exact'
             AND (v_sup_elem->>'percent') IS NOT NULL
             AND (v_sup_elem->>'percent') <> 'null'
             AND (v_sup_elem->>'percent') ~ '^-?[0-9]+$'
             AND (v_raw_level IS NULL OR v_raw_level >= 0) THEN
            v_sup_pct := (v_sup_elem->>'percent')::int;
            IF v_sup_pct < 0 OR v_sup_pct > 100 THEN
              v_sup_pct := NULL;
            END IF;
          ELSE
            v_sup_pct := NULL;
          END IF;

          IF v_sup_pct IS NULL THEN
            CONTINUE;
          END IF;

          IF v_sup_class = 'waste' THEN
            v_sup_color_norm := 'waste';
          ELSIF v_sup_color_field IN ('black', 'preto', 'preta', 'k', 'bk', 'pb', 'p&b') THEN
            v_sup_color_norm := 'black';
          ELSIF v_sup_color_field IN ('cyan', 'ciano', 'c') THEN
            v_sup_color_norm := 'cyan';
          ELSIF v_sup_color_field IN ('magenta', 'm') THEN
            v_sup_color_norm := 'magenta';
          ELSIF v_sup_color_field IN ('yellow', 'amarelo', 'amarela', 'y') THEN
            v_sup_color_norm := 'yellow';
          ELSIF v_sup_name_lc ~ '(^|[^a-z])(black|preto|preta|pb|p&b|bk|k)([^a-z]|$)' THEN
            v_sup_color_norm := 'black';
          ELSIF v_sup_name_lc ~ '(^|[^a-z])(cyan|ciano|c)([^a-z]|$)' THEN
            v_sup_color_norm := 'cyan';
          ELSIF v_sup_name_lc ~ '(^|[^a-z])(magenta|m)([^a-z]|$)' THEN
            v_sup_color_norm := 'magenta';
          ELSIF v_sup_name_lc ~ '(^|[^a-z])(yellow|amarelo|amarela|y)([^a-z]|$)' THEN
            v_sup_color_norm := 'yellow';
          ELSE
            v_sup_color_norm := 'other';
          END IF;

          IF v_sup_class = 'waste' THEN
            v_meta_key := 'waste:' || regexp_replace(v_sup_name_lc, '[^a-z0-9]+', '_', 'g');
          ELSIF v_sup_name_lc ~ '(drum|cilindro|fotocondutor|fusor|fuser|belt|transfer|unidade de imagem|opc)' THEN
            v_meta_key := 'part:' || regexp_replace(v_sup_name_lc, '[^a-z0-9]+', '_', 'g');
          ELSIF v_sup_color_norm <> 'other' THEN
            v_meta_key := 'consumed:' || v_sup_color_norm;
          ELSE
            v_meta_key := 'consumed:' || regexp_replace(v_sup_name_lc, '[^a-z0-9]+', '_', 'g');
          END IF;

          v_prev_meta := v_meta -> v_meta_key;
          v_prev_pct := CASE
            WHEN COALESCE(v_prev_meta->>'level', '') ~ '^[0-9]+$' THEN (v_prev_meta->>'level')::int
            ELSE NULL
          END;
          v_prev_serial := NULLIF(TRIM(COALESCE(v_prev_meta->>'serial', '')), '');

          IF v_prev_pct IS NULL AND v_prt.id IS NOT NULL AND v_sup_class = 'consumed' THEN
            IF v_sup_color_norm = 'black' THEN
              v_prev_pct := v_prev_reading.k_level;
            ELSIF v_sup_color_norm = 'cyan' THEN
              v_prev_pct := v_prev_reading.c_level;
            ELSIF v_sup_color_norm = 'magenta' THEN
              v_prev_pct := v_prev_reading.m_level;
            ELSIF v_sup_color_norm = 'yellow' THEN
              v_prev_pct := v_prev_reading.y_level;
            END IF;
          END IF;

          IF v_sup_class = 'waste' THEN
            IF v_sup_pct >= 85 THEN
              IF NOT EXISTS (
                SELECT 1 FROM public.supply_alerts
                WHERE printer_id = v_prt_id
                  AND alert_type = 'waste_full'
                  AND supply_name = v_sup_name_raw
                  AND resolved = false
              ) THEN
                INSERT INTO public.supply_alerts (
                  tenant_id, printer_id, client_id, alert_type, severity, supply_name,
                  previous_level_pct, new_level_pct, estimated_waste_brl, message, resolved, created_at
                ) VALUES (
                  v_unit.tenant_id, v_prt_id, v_unit.client_id, 'waste_full',
                  CASE WHEN v_sup_pct >= 95 THEN 'critical' ELSE 'warning' END,
                  v_sup_name_raw, v_prev_pct, v_sup_pct, 0,
                  'Caixa de manutenção / resíduo (' || v_sup_name_raw || ') atingiu ' || v_sup_pct || '% de ocupação. Substituição necessária.',
                  false, now()
                );
              END IF;
            ELSIF v_sup_pct <= 20 THEN
              UPDATE public.supply_alerts
              SET resolved = true
              WHERE printer_id = v_prt_id
                AND alert_type = 'waste_full'
                AND supply_name = v_sup_name_raw
                AND resolved = false;
            END IF;
          ELSE
            IF v_sup_pct <= 15 THEN
              IF NOT EXISTS (
                SELECT 1 FROM public.supply_alerts
                WHERE printer_id = v_prt_id
                  AND alert_type IN ('toner_low', 'low_supply')
                  AND supply_name = v_sup_name_raw
                  AND resolved = false
                  AND created_at >= now() - interval '24 hours'
              ) THEN
                INSERT INTO public.supply_alerts (
                  tenant_id, printer_id, client_id, alert_type, severity, supply_name,
                  previous_level_pct, new_level_pct, estimated_waste_brl, message, resolved, created_at
                ) VALUES (
                  v_unit.tenant_id, v_prt_id, v_unit.client_id, 'toner_low',
                  CASE WHEN v_sup_pct <= 5 THEN 'critical' ELSE 'warning' END,
                  v_sup_name_raw, COALESCE(v_prev_pct, v_sup_pct), v_sup_pct, 0,
                  'Nível crítico em ' || v_sup_name_raw || ': ' || v_sup_pct || '% restante. Substituição preventiva recomendada.',
                  false, now()
                );
              END IF;
            ELSIF v_sup_pct >= 70 THEN
              UPDATE public.supply_alerts
              SET resolved = true
              WHERE printer_id = v_prt_id
                AND alert_type IN ('toner_low', 'low_supply')
                AND supply_name = v_sup_name_raw
                AND resolved = false;
            END IF;

            IF v_prev_pct IS NOT NULL AND v_prev_pct BETWEEN 20 AND 85 THEN
              IF (v_sup_pct - v_prev_pct >= 25 AND v_sup_pct >= 90)
                 OR (v_prev_serial IS NOT NULL AND v_sup_serial IS NOT NULL AND v_prev_serial <> v_sup_serial) THEN
                v_waste_brl := ROUND(((v_prev_pct / 100.0) * 180.0)::numeric, 2);
                INSERT INTO public.supply_alerts (
                  tenant_id, printer_id, client_id, alert_type, severity, supply_name,
                  previous_level_pct, new_level_pct, estimated_waste_brl, message, resolved, created_at
                ) VALUES (
                  v_unit.tenant_id, v_prt_id, v_unit.client_id, 'premature_swap', 'critical',
                  v_sup_name_raw, v_prev_pct, v_sup_pct, v_waste_brl,
                  'Troca prematura detectada em ' || v_sup_name_raw || ': substituído com ' || v_prev_pct || '% restante (novo nível ' || v_sup_pct || '%). Desperdício estimado: R$ ' || v_waste_brl::text || '.',
                  false, now()
                );
              END IF;
            END IF;

            IF v_prt.id IS NOT NULL
               AND v_prev_pct IS NOT NULL
               AND (v_prev_pct - v_sup_pct) >= 8
               AND v_delta_pages <= 2 THEN
              IF NOT EXISTS (
                SELECT 1 FROM public.supply_alerts
                WHERE printer_id = v_prt_id
                  AND alert_type = 'head_cleaning_drain'
                  AND supply_name = v_sup_name_raw
                  AND resolved = false
                  AND created_at >= now() - interval '1 hour'
              ) THEN
                v_waste_brl := ROUND((((v_prev_pct - v_sup_pct) / 100.0) * 120.0)::numeric, 2);
                INSERT INTO public.supply_alerts (
                  tenant_id, printer_id, client_id, alert_type, severity, supply_name,
                  previous_level_pct, new_level_pct, estimated_waste_brl, message, resolved, created_at
                ) VALUES (
                  v_unit.tenant_id, v_prt_id, v_unit.client_id, 'head_cleaning_drain', 'warning',
                  v_sup_name_raw, v_prev_pct, v_sup_pct, v_waste_brl,
                  'Possível drenagem por limpeza de cabeçote em ' || v_sup_name_raw || ': queda de ' || v_prev_pct || '% para ' || v_sup_pct || '% com apenas ' || v_delta_pages || ' página(s) impressa(s).',
                  false, now()
                );
              END IF;
            END IF;
          END IF;

          v_meta := jsonb_set(
            v_meta,
            ARRAY[v_meta_key],
            jsonb_build_object(
              'level', v_sup_pct,
              'serial', COALESCE(v_sup_serial, v_prev_serial, ''),
              'class', v_sup_class,
              'updated_at', v_now_iso
            ),
            true
          );
        END LOOP;

        UPDATE public.printers
        SET supply_alerts_meta = v_meta
        WHERE id = v_prt_id;
      END IF;

      v_synced := v_synced + 1;
    END LOOP;
  END IF;

  -- 3. Upsert jobs_summary into public.print_jobs_summary (enforcing Physical Color Gate)
  IF p_payload->'jobs_summary' IS NOT NULL
     AND jsonb_typeof(p_payload->'jobs_summary') = 'array'
     AND jsonb_array_length(p_payload->'jobs_summary') > 0 THEN
    FOR v_job IN SELECT * FROM jsonb_array_elements(p_payload->'jobs_summary') LOOP
      v_job_printer_id := NULL;
      v_job_color_capable := false;

      IF COALESCE(v_job->>'printer_id', '') ~ '^[0-9a-fA-F-]{36}$' THEN
        SELECT id, is_color_capable INTO v_job_printer_id, v_job_color_capable
        FROM public.printers
        WHERE id = (v_job->>'printer_id')::uuid AND tenant_id = v_unit.tenant_id
        LIMIT 1;
      END IF;

      IF v_job_printer_id IS NULL THEN
        v_job_uuid := NULLIF(TRIM(COALESCE(v_job->>'device_uuid', v_job->>'printer_uuid', '')), '');
        v_job_serial := NULLIF(TRIM(COALESCE(v_job->>'printer_serial', v_job->>'serial_number', v_job->>'serial', '')), '');
        v_job_ip := NULLIF(TRIM(COALESCE(v_job->>'printer_ip', v_job->>'ip_address', v_job->>'ip', '')), '');
        v_job_usb := NULLIF(TRIM(COALESCE(v_job->>'usb_port', '')), '');

        SELECT id, is_color_capable INTO v_job_printer_id, v_job_color_capable
        FROM public.printers
        WHERE tenant_id = v_unit.tenant_id
          AND (
            (v_job_uuid IS NOT NULL AND device_uuid = v_job_uuid)
            OR (v_job_serial IS NOT NULL AND serial = v_job_serial)
            OR (v_job_ip IS NOT NULL AND v_job_ip <> '0.0.0.0' AND ip = v_job_ip)
            OR (v_job_usb IS NOT NULL AND usb_port = v_job_usb)
            OR (NULLIF(TRIM(v_job->>'printer_name'), '') IS NOT NULL AND lower(model) = lower(TRIM(v_job->>'printer_name')))
          )
        LIMIT 1;
      END IF;

      v_job_period := SUBSTRING(COALESCE(NULLIF(TRIM(v_job->>'period_month'), ''), to_char(now(), 'YYYY-MM')) FROM 1 FOR 7);
      v_job_user := SUBSTRING(COALESCE(NULLIF(TRIM(v_job->>'user_name'), ''), 'Sistema') FROM 1 FOR 120);
      v_job_machine := SUBSTRING(COALESCE(NULLIF(TRIM(v_job->>'machine_name'), ''), v_host_name) FROM 1 FOR 120);
      v_job_dept := SUBSTRING(COALESCE(NULLIF(TRIM(v_job->>'department'), ''), NULLIF(TRIM(v_job->>'domain'), ''), 'Geral') FROM 1 FOR 120);
      v_job_source := SUBSTRING(COALESCE(NULLIF(TRIM(v_job->>'source'), ''), 'event_307') FROM 1 FOR 30);

      v_job_mono := GREATEST(0, COALESCE((v_job->>'mono_pages')::bigint, 0));
      v_job_color := CASE
        WHEN COALESCE(v_job_color_capable, false) THEN GREATEST(0, COALESCE((v_job->>'color_pages')::bigint, 0))
        ELSE 0
      END;
      IF NOT COALESCE(v_job_color_capable, false) AND COALESCE((v_job->>'color_pages')::bigint, 0) > 0 THEN
        v_job_mono := GREATEST(v_job_mono, COALESCE((v_job->>'total_pages')::bigint, v_job_mono + COALESCE((v_job->>'color_pages')::bigint, 0)));
      END IF;
      v_job_total := GREATEST(COALESCE((v_job->>'total_pages')::bigint, 0), v_job_mono + v_job_color);
      v_job_duplex := LEAST(v_job_total, GREATEST(0, COALESCE((v_job->>'duplex_pages')::bigint, 0)));
      v_job_count := GREATEST(0, COALESCE((v_job->>'jobs_count')::int, (v_job->>'job_count')::int, 0));
      v_job_is_delta := COALESCE((v_job->>'is_delta')::boolean, false);

      IF v_job_printer_id IS NOT NULL THEN
        INSERT INTO public.print_jobs_summary (
          tenant_id, client_id, unit_id, printer_id, period_month,
          user_name, machine_name, department,
          total_pages, mono_pages, color_pages, duplex_pages, jobs_count, source, updated_at
        ) VALUES (
          v_unit.tenant_id, v_unit.client_id, v_unit.id, v_job_printer_id, v_job_period,
          v_job_user, v_job_machine, v_job_dept,
          v_job_total, v_job_mono, v_job_color, v_job_duplex, v_job_count, v_job_source, now()
        )
        ON CONFLICT (tenant_id, period_month, printer_id, user_name, machine_name, department)
        DO UPDATE SET
          client_id = COALESCE(EXCLUDED.client_id, public.print_jobs_summary.client_id),
          unit_id = COALESCE(EXCLUDED.unit_id, public.print_jobs_summary.unit_id),
          total_pages = CASE
            WHEN v_job_is_delta THEN public.print_jobs_summary.total_pages + EXCLUDED.total_pages
            ELSE GREATEST(public.print_jobs_summary.total_pages, EXCLUDED.total_pages)
          END,
          mono_pages = CASE
            WHEN v_job_is_delta THEN public.print_jobs_summary.mono_pages + EXCLUDED.mono_pages
            ELSE GREATEST(public.print_jobs_summary.mono_pages, EXCLUDED.mono_pages)
          END,
          color_pages = CASE
            WHEN NOT COALESCE(v_job_color_capable, false) THEN 0
            WHEN v_job_is_delta THEN public.print_jobs_summary.color_pages + EXCLUDED.color_pages
            ELSE GREATEST(public.print_jobs_summary.color_pages, EXCLUDED.color_pages)
          END,
          duplex_pages = CASE
            WHEN v_job_is_delta THEN public.print_jobs_summary.duplex_pages + EXCLUDED.duplex_pages
            ELSE GREATEST(public.print_jobs_summary.duplex_pages, EXCLUDED.duplex_pages)
          END,
          jobs_count = CASE
            WHEN v_job_is_delta THEN public.print_jobs_summary.jobs_count + EXCLUDED.jobs_count
            ELSE GREATEST(public.print_jobs_summary.jobs_count, EXCLUDED.jobs_count)
          END,
          source = EXCLUDED.source,
          updated_at = now();
      ELSE
        UPDATE public.print_jobs_summary
        SET
          client_id = COALESCE(v_unit.client_id, client_id),
          unit_id = COALESCE(v_unit.id, unit_id),
          total_pages = CASE WHEN v_job_is_delta THEN total_pages + v_job_total ELSE GREATEST(total_pages, v_job_total) END,
          mono_pages = CASE WHEN v_job_is_delta THEN mono_pages + v_job_mono ELSE GREATEST(mono_pages, v_job_mono) END,
          color_pages = 0,
          duplex_pages = CASE WHEN v_job_is_delta THEN duplex_pages + v_job_duplex ELSE GREATEST(duplex_pages, v_job_duplex) END,
          jobs_count = CASE WHEN v_job_is_delta THEN jobs_count + v_job_count ELSE GREATEST(jobs_count, v_job_count) END,
          source = v_job_source,
          updated_at = now()
        WHERE tenant_id = v_unit.tenant_id
          AND period_month = v_job_period
          AND printer_id IS NULL
          AND user_name = v_job_user
          AND machine_name = v_job_machine
          AND department = v_job_dept;

        IF NOT FOUND THEN
          INSERT INTO public.print_jobs_summary (
            tenant_id, client_id, unit_id, printer_id, period_month,
            user_name, machine_name, department,
            total_pages, mono_pages, color_pages, duplex_pages, jobs_count, source, updated_at
          ) VALUES (
            v_unit.tenant_id, v_unit.client_id, v_unit.id, NULL, v_job_period,
            v_job_user, v_job_machine, v_job_dept,
            v_job_total, v_job_mono, 0, v_job_duplex, v_job_count, v_job_source, now()
          );
        END IF;
      END IF;

      v_jobs_synced := v_jobs_synced + 1;
    END LOOP;
  END IF;

  RETURN jsonb_build_object(
    'status', 'success',
    'message', 'Telemetria sincronizada com sucesso',
    'tenant_id', v_unit.tenant_id,
    'unit_id', v_unit.id,
    'unit_name', v_unit.unit_name,
    'synced_devices', v_synced,
    'synced_jobs', v_jobs_synced,
    'remote_config', jsonb_build_object(
      'poll_interval_seconds', COALESCE(v_unit.poll_interval_sec, 300),
      'rate_limit_packets_per_sec', COALESCE(v_unit.rate_limit_pps, 25),
      'scan_subnets', COALESCE(to_jsonb(v_unit.scan_subnets), jsonb_build_array('192.168.1.0/24', '192.168.0.0/24', '10.0.0.0/24')),
      'static_ips', COALESCE(to_jsonb(v_unit.static_ips), '[]'::jsonb),
      'enable_usb_spooler', COALESCE(v_unit.enable_usb_spooler, true),
      'enable_http_wakeup_for_pantum', COALESCE(v_unit.enable_http_wakeup, true),
      'diagnostic_requested', COALESCE(v_unit.diagnostic_requested, false),
      'snmp_v3', COALESCE(v_unit.snmp_v3_profile, '{}'::jsonb),
      'update_msi_url', COALESCE(v_unit.update_msi_url, ''),
      'update_sha256', COALESCE(v_unit.update_sha256, ''),
      'update_exe_url', COALESCE(v_unit.update_exe_url, ''),
      'update_exe_sha256', COALESCE(v_unit.update_exe_sha256, ''),
      'update_version', COALESCE(v_unit.update_version, ''),
      'update_linux_url', COALESCE(v_unit.update_linux_url, ''),
      'update_linux_sha256', COALESCE(v_unit.update_linux_sha256, '')
    )
  );
END;
$$;

GRANT EXECUTE ON FUNCTION public.sync_agent_telemetry(text, jsonb) TO anon, authenticated, service_role;

-- ----------------------------------------------------------------------------
-- 7. PORTAL DO TOMADOR (Tokens SHA-256 + Reimpressão Segura + Chamados + Anti-Bruteforce)
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS public.portal_access_tokens (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id UUID NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,
  printer_id UUID REFERENCES public.printers(id) ON DELETE CASCADE,
  client_id UUID REFERENCES public.clients(id) ON DELETE CASCADE,
  token_scope TEXT NOT NULL DEFAULT 'printer_ticket_only' CHECK (token_scope IN ('full_client', 'printer_ticket_only')),
  token_sha256 VARCHAR(64) UNIQUE NOT NULL,
  token_raw TEXT NOT NULL,
  pin_hash VARCHAR(64) NOT NULL,
  label VARCHAR(120),
  is_active BOOLEAN NOT NULL DEFAULT TRUE,
  failed_pin_attempts INT NOT NULL DEFAULT 0,
  locked_until TIMESTAMPTZ,
  expires_at TIMESTAMPTZ,
  last_used_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT chk_portal_token_scope_target CHECK (
    (token_scope = 'full_client' AND client_id IS NOT NULL)
    OR (token_scope = 'printer_ticket_only' AND printer_id IS NOT NULL)
  )
);

ALTER TABLE public.portal_access_tokens
  ADD COLUMN IF NOT EXISTS token_scope TEXT NOT NULL DEFAULT 'printer_ticket_only';

ALTER TABLE public.portal_access_tokens
  ALTER COLUMN printer_id DROP NOT NULL;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'portal_access_tokens_token_scope_check'
  ) THEN
    ALTER TABLE public.portal_access_tokens
      ADD CONSTRAINT portal_access_tokens_token_scope_check
      CHECK (token_scope IN ('full_client', 'printer_ticket_only'));
  END IF;
END $$;

UPDATE public.portal_access_tokens
SET printer_id = NULL
WHERE token_scope = 'full_client' AND printer_id IS NOT NULL;

DELETE FROM public.portal_access_tokens
WHERE (token_scope = 'full_client' AND client_id IS NULL)
   OR (COALESCE(token_scope, 'printer_ticket_only') = 'printer_ticket_only' AND printer_id IS NULL);

ALTER TABLE public.portal_access_tokens
  DROP CONSTRAINT IF EXISTS chk_portal_token_scope_target;

ALTER TABLE public.portal_access_tokens
  ADD CONSTRAINT chk_portal_token_scope_target CHECK (
    (token_scope = 'full_client' AND client_id IS NOT NULL AND printer_id IS NULL)
    OR (token_scope = 'printer_ticket_only' AND printer_id IS NOT NULL)
  );

CREATE INDEX IF NOT EXISTS idx_portal_tokens_printer_active
  ON public.portal_access_tokens(tenant_id, printer_id)
  WHERE is_active = TRUE;

CREATE INDEX IF NOT EXISTS idx_portal_tokens_client_scope_active
  ON public.portal_access_tokens(tenant_id, client_id, token_scope)
  WHERE is_active = TRUE;

ALTER TABLE public.portal_access_tokens ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS "Tenant users can manage portal_access_tokens" ON public.portal_access_tokens;
CREATE POLICY "Tenant users can manage portal_access_tokens"
  ON public.portal_access_tokens
  FOR ALL
  TO authenticated
  USING (
    lower(COALESCE(auth.jwt()->>'email', '')) = 'lucaspereiracardoso2024@gmail.com'
    OR tenant_id IN (SELECT tenant_id FROM public.profiles WHERE id = auth.uid())
  )
  WITH CHECK (
    lower(COALESCE(auth.jwt()->>'email', '')) = 'lucaspereiracardoso2024@gmail.com'
    OR tenant_id IN (SELECT tenant_id FROM public.profiles WHERE id = auth.uid())
  );

CREATE TABLE IF NOT EXISTS public.service_tickets (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id UUID NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,
  printer_id UUID REFERENCES public.printers(id) ON DELETE SET NULL,
  client_id UUID REFERENCES public.clients(id) ON DELETE SET NULL,
  portal_token_id UUID REFERENCES public.portal_access_tokens(id) ON DELETE SET NULL,
  protocol_code VARCHAR(24) NOT NULL,
  category VARCHAR(40) NOT NULL CHECK (category IN ('toner', 'atolagem', 'qualidade', 'offline', 'chamado_tecnico', 'outro')),
  status VARCHAR(25) NOT NULL DEFAULT 'aberto' CHECK (status IN ('aberto', 'em_atendimento', 'resolvido', 'cancelado')),
  requester_name VARCHAR(120) NOT NULL,
  requester_phone VARCHAR(30),
  description TEXT NOT NULL,
  internal_notes TEXT,
  resolution_note TEXT,
  resolved_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE public.service_tickets ADD COLUMN IF NOT EXISTS resolution_note TEXT;

CREATE INDEX IF NOT EXISTS idx_service_tickets_tenant_status
  ON public.service_tickets(tenant_id, status, created_at DESC);

ALTER TABLE public.service_tickets ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS "Tenant users can manage service_tickets" ON public.service_tickets;
CREATE POLICY "Tenant users can manage service_tickets"
  ON public.service_tickets
  FOR ALL
  TO authenticated
  USING (
    lower(COALESCE(auth.jwt()->>'email', '')) = 'lucaspereiracardoso2024@gmail.com'
    OR tenant_id IN (SELECT tenant_id FROM public.profiles WHERE id = auth.uid())
  )
  WITH CHECK (
    lower(COALESCE(auth.jwt()->>'email', '')) = 'lucaspereiracardoso2024@gmail.com'
    OR tenant_id IN (SELECT tenant_id FROM public.profiles WHERE id = auth.uid())
  );

-- Authenticated tenant helper to get existing portal token without invalidating active QR stickers/links
-- Supports both 'printer_ticket_only' (p_printer_id) and 'full_client' (p_client_id), rotating only when p_rotate = true.
DROP FUNCTION IF EXISTS public.get_or_create_portal_token(uuid, text, boolean, text);
DROP FUNCTION IF EXISTS public.get_or_create_portal_token(uuid, text, boolean, text, text, uuid);

CREATE OR REPLACE FUNCTION public.get_or_create_portal_token(
  p_printer_id uuid DEFAULT NULL,
  p_pin text DEFAULT NULL,
  p_rotate boolean DEFAULT FALSE,
  p_label text DEFAULT NULL,
  p_scope text DEFAULT 'printer_ticket_only',
  p_client_id uuid DEFAULT NULL
)
RETURNS jsonb
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, extensions
AS $$
DECLARE
  v_scope text := lower(TRIM(COALESCE(p_scope, 'printer_ticket_only')));
  v_prt RECORD;
  v_cli RECORD;
  v_existing RECORD;
  v_tenant_id uuid;
  v_printer_id uuid;
  v_client_id uuid;
  v_default_label text;
  v_raw_tok text;
  v_tok_hash text;
  v_pin_input text := NULLIF(TRIM(COALESCE(p_pin, '')), '');
  v_pin_clean text;
  v_pin_hash text;
  v_caller_email text := lower(TRIM(COALESCE(auth.jwt()->>'email', '')));
BEGIN
  IF v_scope NOT IN ('full_client', 'printer_ticket_only') THEN
    RETURN jsonb_build_object('status', 'error', 'message', 'Escopo de token inválido.');
  END IF;

  IF v_scope = 'printer_ticket_only' THEN
    IF p_printer_id IS NULL THEN
      RETURN jsonb_build_object('status', 'error', 'message', 'Impressora não informada.');
    END IF;
    SELECT p.* INTO v_prt FROM public.printers p WHERE p.id = p_printer_id;
    IF NOT FOUND THEN
      RETURN jsonb_build_object('status', 'error', 'message', 'Impressora não encontrada.');
    END IF;
    v_tenant_id := v_prt.tenant_id;
    v_printer_id := v_prt.id;
    v_client_id := v_prt.client_id;
    v_default_label := v_prt.model || ' (' || v_prt.serial || ')';
  ELSE
    IF p_client_id IS NULL THEN
      RETURN jsonb_build_object('status', 'error', 'message', 'Cliente não informado para Portal do Cliente.');
    END IF;
    SELECT c.* INTO v_cli FROM public.clients c WHERE c.id = p_client_id;
    IF NOT FOUND THEN
      RETURN jsonb_build_object('status', 'error', 'message', 'Cliente não encontrado.');
    END IF;
    v_tenant_id := v_cli.tenant_id;
    v_printer_id := NULL;
    v_client_id := v_cli.id;
    v_default_label := 'Portal Completo — ' || v_cli.name;
  END IF;

  IF v_caller_email <> 'lucaspereiracardoso2024@gmail.com'
     AND NOT EXISTS (SELECT 1 FROM public.profiles WHERE id = auth.uid() AND tenant_id = v_tenant_id) THEN
    RAISE EXCEPTION 'Unauthorized' USING ERRCODE = '42501';
  END IF;

  IF v_pin_input IS NOT NULL THEN
    v_pin_clean := REGEXP_REPLACE(v_pin_input, '[^A-Za-z0-9]', '', 'g');
    IF length(v_pin_clean) < 4 THEN
      RETURN jsonb_build_object('status', 'error', 'message', 'O PIN do portal deve ter pelo menos 4 caracteres alfanuméricos.');
    END IF;
    v_pin_hash := encode(digest(v_pin_clean, 'sha256'), 'hex');
  END IF;

  IF v_scope = 'printer_ticket_only' THEN
    SELECT * INTO v_existing
    FROM public.portal_access_tokens
    WHERE tenant_id = v_tenant_id
      AND token_scope = 'printer_ticket_only'
      AND printer_id = v_printer_id
      AND is_active = TRUE
      AND (expires_at IS NULL OR expires_at > now())
    ORDER BY created_at DESC
    LIMIT 1;
  ELSE
    SELECT * INTO v_existing
    FROM public.portal_access_tokens
    WHERE tenant_id = v_tenant_id
      AND token_scope = 'full_client'
      AND client_id = v_client_id
      AND is_active = TRUE
      AND (expires_at IS NULL OR expires_at > now())
    ORDER BY created_at DESC
    LIMIT 1;
  END IF;

  IF FOUND AND NOT COALESCE(p_rotate, FALSE) THEN
    IF v_pin_input IS NOT NULL OR NULLIF(TRIM(COALESCE(p_label, '')), '') IS NOT NULL THEN
      UPDATE public.portal_access_tokens
      SET pin_hash = COALESCE(v_pin_hash, pin_hash),
          label = COALESCE(NULLIF(TRIM(p_label), ''), label),
          failed_pin_attempts = CASE WHEN v_pin_hash IS NOT NULL THEN 0 ELSE failed_pin_attempts END,
          locked_until = CASE WHEN v_pin_hash IS NOT NULL THEN NULL ELSE locked_until END
      WHERE id = v_existing.id;
    END IF;
    RETURN jsonb_build_object(
      'status', 'success',
      'token_id', v_existing.id,
      'token_raw', v_existing.token_raw,
      'token_scope', v_existing.token_scope,
      'reused', TRUE
    );
  END IF;

  IF v_pin_hash IS NULL THEN
    IF FOUND AND v_existing.pin_hash IS NOT NULL THEN
      v_pin_hash := v_existing.pin_hash;
    ELSE
      v_pin_hash := encode(digest('1234', 'sha256'), 'hex');
    END IF;
  END IF;

  IF COALESCE(p_rotate, FALSE) THEN
    IF v_scope = 'printer_ticket_only' THEN
      UPDATE public.portal_access_tokens
      SET is_active = FALSE
      WHERE tenant_id = v_tenant_id
        AND token_scope = 'printer_ticket_only'
        AND printer_id = v_printer_id
        AND is_active = TRUE;
    ELSE
      UPDATE public.portal_access_tokens
      SET is_active = FALSE
      WHERE tenant_id = v_tenant_id
        AND token_scope = 'full_client'
        AND client_id = v_client_id
        AND is_active = TRUE;
    END IF;
  END IF;

  v_raw_tok := 'pmp_' || encode(gen_random_bytes(16), 'hex');
  v_tok_hash := encode(digest(v_raw_tok, 'sha256'), 'hex');

  INSERT INTO public.portal_access_tokens (
    tenant_id, printer_id, client_id, token_scope, token_sha256, token_raw, pin_hash, label, is_active
  ) VALUES (
    v_tenant_id, v_printer_id, v_client_id, v_scope, v_tok_hash, v_raw_tok, v_pin_hash,
    COALESCE(NULLIF(TRIM(p_label), ''), v_default_label),
    TRUE
  )
  RETURNING * INTO v_existing;

  RETURN jsonb_build_object(
    'status', 'success',
    'token_id', v_existing.id,
    'token_raw', v_existing.token_raw,
    'token_scope', v_existing.token_scope,
    'reused', FALSE
  );
END;
$$;

GRANT EXECUTE ON FUNCTION public.get_or_create_portal_token(uuid, text, boolean, text, text, uuid) TO authenticated, service_role;

-- Public anon/authenticated read-only portal view via SHA-256 token lookup (zero financial/tenant leaks)
CREATE OR REPLACE FUNCTION public.get_portal_data_by_token(p_token text)
RETURNS jsonb
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, extensions
AS $$
DECLARE
  v_tok_hash text;
  v_tok RECORD;
  v_scope text;
  v_prt RECORD;
  v_cli RECORD;
  v_ten RECORD;
  v_printers jsonb := '[]'::jsonb;
  v_tickets jsonb := '[]'::jsonb;
BEGIN
  IF NULLIF(TRIM(COALESCE(p_token, '')), '') IS NULL THEN
    RETURN jsonb_build_object('status', 'error', 'message', 'Token de acesso não informado.');
  END IF;

  v_tok_hash := encode(digest(TRIM(p_token), 'sha256'), 'hex');

  SELECT * INTO v_tok
  FROM public.portal_access_tokens
  WHERE token_sha256 = v_tok_hash
    AND is_active = TRUE
    AND (expires_at IS NULL OR expires_at > now())
  LIMIT 1;

  IF NOT FOUND THEN
    RETURN jsonb_build_object('status', 'error', 'message', 'Link ou QR Code inválido, expirado ou revogado.');
  END IF;

  UPDATE public.portal_access_tokens SET last_used_at = now() WHERE id = v_tok.id;

  v_scope := COALESCE(v_tok.token_scope, 'printer_ticket_only');
  SELECT trade_name, phone INTO v_ten FROM public.tenants WHERE id = v_tok.tenant_id;

  IF v_scope = 'full_client' THEN
    SELECT id, name INTO v_cli
    FROM public.clients
    WHERE id = v_tok.client_id AND tenant_id = v_tok.tenant_id;

    IF NOT FOUND THEN
      RETURN jsonb_build_object('status', 'error', 'message', 'Cliente não vinculado ao portal.');
    END IF;

    SELECT COALESCE(jsonb_agg(
      jsonb_build_object(
        'id', p.id,
        'model', p.model,
        'serial', p.serial,
        'status', p.status,
        'unit', p.unit,
        'department', p.department,
        'location', p.location,
        'is_color_capable', p.is_color_capable,
        'supplies', p.supplies,
        'last_seen', p.last_seen
      ) ORDER BY COALESCE(p.unit, ''), COALESCE(p.department, ''), p.model
    ), '[]'::jsonb)
    INTO v_printers
    FROM public.printers p
    WHERE p.tenant_id = v_tok.tenant_id AND p.client_id = v_cli.id;

    SELECT COALESCE(jsonb_agg(
      jsonb_build_object(
        'protocol_code', t.protocol_code,
        'printer_id', t.printer_id,
        'printer_model', t.printer_model,
        'printer_serial', t.printer_serial,
        'category', t.category,
        'status', t.status,
        'description', t.description,
        'resolution_note', t.resolution_note,
        'created_at', t.created_at
      ) ORDER BY t.created_at DESC
    ), '[]'::jsonb)
    INTO v_tickets
    FROM (
      SELECT
        st.protocol_code,
        st.printer_id,
        pr.model AS printer_model,
        pr.serial AS printer_serial,
        st.category,
        st.status,
        st.description,
        st.resolution_note,
        st.created_at
      FROM public.service_tickets st
      LEFT JOIN public.printers pr ON pr.id = st.printer_id AND pr.tenant_id = st.tenant_id
      WHERE st.tenant_id = v_tok.tenant_id
        AND (
          st.client_id = v_cli.id
          OR st.printer_id IN (SELECT id FROM public.printers WHERE tenant_id = v_tok.tenant_id AND client_id = v_cli.id)
        )
      ORDER BY st.created_at DESC
      LIMIT 20
    ) t;

    RETURN jsonb_build_object(
      'status', 'success',
      'token_scope', 'full_client',
      'requires_pin', TRUE,
      'provider_name', COALESCE(v_ten.trade_name, 'Assistência PrintMind'),
      'provider_phone', COALESCE(v_ten.phone, ''),
      'locked_until', v_tok.locked_until,
      'client', jsonb_build_object(
        'id', v_cli.id,
        'name', v_cli.name
      ),
      'printers', v_printers,
      'recent_tickets', v_tickets
    );
  END IF;

  SELECT id, model, serial, status, unit, department, location, supplies, is_color_capable, last_seen
  INTO v_prt
  FROM public.printers
  WHERE id = v_tok.printer_id AND tenant_id = v_tok.tenant_id;

  IF NOT FOUND THEN
    RETURN jsonb_build_object('status', 'error', 'message', 'Equipamento não vinculado.');
  END IF;

  SELECT COALESCE(jsonb_agg(
    jsonb_build_object(
      'protocol_code', t.protocol_code,
      'printer_id', t.printer_id,
      'category', t.category,
      'status', t.status,
      'description', t.description,
      'resolution_note', t.resolution_note,
      'created_at', t.created_at
    ) ORDER BY t.created_at DESC
  ), '[]'::jsonb)
  INTO v_tickets
  FROM (
    SELECT protocol_code, printer_id, category, status, description, resolution_note, created_at
    FROM public.service_tickets
    WHERE printer_id = v_prt.id AND tenant_id = v_tok.tenant_id
    ORDER BY created_at DESC
    LIMIT 10
  ) t;

  RETURN jsonb_build_object(
    'status', 'success',
    'token_scope', 'printer_ticket_only',
    'requires_pin', TRUE,
    'provider_name', COALESCE(v_ten.trade_name, 'Assistência PrintMind'),
    'provider_phone', COALESCE(v_ten.phone, ''),
    'locked_until', v_tok.locked_until,
    'printer', jsonb_build_object(
      'id', v_prt.id,
      'model', v_prt.model,
      'serial', v_prt.serial,
      'status', v_prt.status,
      'unit', v_prt.unit,
      'department', v_prt.department,
      'location', v_prt.location,
      'is_color_capable', v_prt.is_color_capable,
      'supplies', v_prt.supplies,
      'last_seen', v_prt.last_seen
    ),
    'printers', jsonb_build_array(
      jsonb_build_object(
        'id', v_prt.id,
        'model', v_prt.model,
        'serial', v_prt.serial,
        'status', v_prt.status,
        'unit', v_prt.unit,
        'department', v_prt.department,
        'location', v_prt.location,
        'is_color_capable', v_prt.is_color_capable,
        'supplies', v_prt.supplies,
        'last_seen', v_prt.last_seen
      )
    ),
    'recent_tickets', v_tickets
  );
END;
$$;

GRANT EXECUTE ON FUNCTION public.get_portal_data_by_token(text) TO anon, authenticated, service_role;

-- Public anon/authenticated ticket submission supporting both 'printer_ticket_only' and 'full_client'
DROP FUNCTION IF EXISTS public.create_portal_ticket_by_token(text, text, text, text, text, text, uuid);

CREATE OR REPLACE FUNCTION public.create_portal_ticket_by_token(
  p_token text,
  p_pin text,
  p_requester_name text,
  p_requester_phone text DEFAULT NULL,
  p_category text DEFAULT 'chamado_tecnico',
  p_description text DEFAULT '',
  p_printer_id uuid DEFAULT NULL
)
RETURNS jsonb
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, extensions
AS $$
DECLARE
  v_tok_hash text;
  v_pin_hash text;
  v_tok RECORD;
  v_scope text;
  v_target_printer_id uuid;
  v_target_client_id uuid;
  v_client_printers_count int := 0;
  v_existing_dup RECORD;
  v_hourly_count int := 0;
  v_cat text;
  v_proto text;
  v_new_id uuid;
BEGIN
  IF NULLIF(TRIM(COALESCE(p_token, '')), '') IS NULL THEN
    RETURN jsonb_build_object('status', 'error', 'message', 'Token obrigatório.');
  END IF;

  v_tok_hash := encode(digest(TRIM(p_token), 'sha256'), 'hex');
  SELECT * INTO v_tok
  FROM public.portal_access_tokens
  WHERE token_sha256 = v_tok_hash
    AND is_active = TRUE
    AND (expires_at IS NULL OR expires_at > now())
  LIMIT 1;

  IF NOT FOUND THEN
    RETURN jsonb_build_object('status', 'error', 'message', 'Link ou QR Code inválido ou revogado.');
  END IF;

  IF v_tok.locked_until IS NOT NULL AND v_tok.locked_until > now() THEN
    RETURN jsonb_build_object('status', 'locked', 'message', 'Acesso temporariamente bloqueado por excesso de tentativas incorretas. Tente novamente em 15 minutos.');
  END IF;

  v_pin_hash := encode(digest(REGEXP_REPLACE(TRIM(COALESCE(p_pin, '')), '[^A-Za-z0-9]', '', 'g'), 'sha256'), 'hex');
  IF v_pin_hash <> v_tok.pin_hash THEN
    UPDATE public.portal_access_tokens
    SET
      failed_pin_attempts = failed_pin_attempts + 1,
      locked_until = CASE WHEN failed_pin_attempts + 1 >= 5 THEN now() + interval '15 minutes' ELSE NULL END
    WHERE id = v_tok.id;

    IF v_tok.failed_pin_attempts + 1 >= 5 THEN
      RETURN jsonb_build_object('status', 'locked', 'message', 'PIN incorreto 5 vezes seguidas. Acesso bloqueado por 15 minutos.');
    END IF;
    RETURN jsonb_build_object('status', 'invalid_pin', 'message', 'PIN de segurança incorreto.');
  END IF;

  v_scope := COALESCE(v_tok.token_scope, 'printer_ticket_only');

  IF v_scope = 'printer_ticket_only' THEN
    IF p_printer_id IS NOT NULL AND p_printer_id <> v_tok.printer_id THEN
      RETURN jsonb_build_object('status', 'error', 'message', 'Equipamento divergente da etiqueta QR vinculada.');
    END IF;
    v_target_printer_id := v_tok.printer_id;
    SELECT client_id INTO v_target_client_id
    FROM public.printers
    WHERE id = v_target_printer_id AND tenant_id = v_tok.tenant_id;
  ELSE
    v_target_client_id := v_tok.client_id;
    SELECT COUNT(*) INTO v_client_printers_count
    FROM public.printers
    WHERE tenant_id = v_tok.tenant_id AND client_id = v_target_client_id;

    IF p_printer_id IS NOT NULL THEN
      IF NOT EXISTS (
        SELECT 1 FROM public.printers
        WHERE id = p_printer_id AND tenant_id = v_tok.tenant_id AND client_id = v_target_client_id
      ) THEN
        RETURN jsonb_build_object('status', 'error', 'message', 'Equipamento selecionado não pertence a este cliente.');
      END IF;
      v_target_printer_id := p_printer_id;
    ELSIF v_client_printers_count > 0 THEN
      RETURN jsonb_build_object('status', 'error', 'message', 'Selecione o equipamento para registrar o chamado.');
    ELSE
      v_target_printer_id := NULL;
    END IF;
  END IF;

  UPDATE public.portal_access_tokens
  SET failed_pin_attempts = 0, locked_until = NULL, last_used_at = now()
  WHERE id = v_tok.id;

  SELECT COUNT(*) INTO v_hourly_count
  FROM public.service_tickets
  WHERE portal_token_id = v_tok.id
    AND created_at >= now() - interval '1 hour';

  IF v_hourly_count >= 5 THEN
    RETURN jsonb_build_object('status', 'rate_limited', 'message', 'Limite de 5 solicitações por hora atingido neste acesso.');
  END IF;

  v_cat := lower(TRIM(COALESCE(p_category, 'chamado_tecnico')));
  IF v_cat NOT IN ('toner', 'atolagem', 'qualidade', 'offline', 'chamado_tecnico', 'outro') THEN
    v_cat := 'chamado_tecnico';
  END IF;

  -- Deduplicate active occurrence within 6 hours for the same target & category
  SELECT id, protocol_code INTO v_existing_dup
  FROM public.service_tickets
  WHERE tenant_id = v_tok.tenant_id
    AND COALESCE(client_id, '00000000-0000-0000-0000-000000000000'::uuid) = COALESCE(v_target_client_id, '00000000-0000-0000-0000-000000000000'::uuid)
    AND COALESCE(printer_id, '00000000-0000-0000-0000-000000000000'::uuid) = COALESCE(v_target_printer_id, '00000000-0000-0000-0000-000000000000'::uuid)
    AND category = v_cat
    AND status IN ('aberto', 'em_atendimento')
    AND created_at >= now() - interval '6 hours'
  ORDER BY created_at DESC
  LIMIT 1;

  IF FOUND THEN
    RETURN jsonb_build_object(
      'status', 'duplicate',
      'ticket_id', v_existing_dup.id,
      'protocol_code', v_existing_dup.protocol_code,
      'message', 'Já existe um chamado aberto nas últimas 6 horas para esta mesma categoria.'
    );
  END IF;

  IF NULLIF(TRIM(COALESCE(p_requester_name, '')), '') IS NULL THEN
    RETURN jsonb_build_object('status', 'error', 'message', 'Informe o nome do solicitante.');
  END IF;
  IF NULLIF(TRIM(COALESCE(p_description, '')), '') IS NULL THEN
    RETURN jsonb_build_object('status', 'error', 'message', 'Descreva brevemente o ocorrido.');
  END IF;

  v_proto := 'CH-' || upper(substr(encode(gen_random_bytes(3), 'hex'), 1, 6));

  INSERT INTO public.service_tickets (
    tenant_id, printer_id, client_id, portal_token_id,
    protocol_code, category, status, requester_name, requester_phone, description
  ) VALUES (
    v_tok.tenant_id, v_target_printer_id, v_target_client_id, v_tok.id,
    v_proto, v_cat, 'aberto',
    SUBSTRING(TRIM(p_requester_name) FROM 1 FOR 120),
    SUBSTRING(NULLIF(TRIM(COALESCE(p_requester_phone, '')), '') FROM 1 FOR 30),
    SUBSTRING(TRIM(p_description) FROM 1 FOR 2000)
  ) RETURNING id INTO v_new_id;

  RETURN jsonb_build_object(
    'status', 'success',
    'ticket_id', v_new_id,
    'protocol_code', v_proto,
    'message', 'Solicitação registrada com sucesso.'
  );
END;
$$;

GRANT EXECUTE ON FUNCTION public.create_portal_ticket_by_token(text, text, text, text, text, text, uuid) TO anon, authenticated, service_role;

CREATE OR REPLACE FUNCTION public.create_portal_ticket(
  p_token text,
  p_pin text,
  p_requester_name text,
  p_requester_phone text DEFAULT NULL,
  p_category text DEFAULT 'chamado_tecnico',
  p_description text DEFAULT ''
)
RETURNS jsonb
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, extensions
AS $$
BEGIN
  RETURN public.create_portal_ticket_by_token(
    p_token,
    p_pin,
    p_requester_name,
    p_requester_phone,
    p_category,
    p_description,
    NULL
  );
END;
$$;

GRANT EXECUTE ON FUNCTION public.create_portal_ticket(text, text, text, text, text, text) TO anon, authenticated, service_role;

-- ----------------------------------------------------------------------------
-- 8. PAYMENT WEBHOOK EVENTS (Idempotência para Asaas Webhook)
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS public.payment_webhook_events (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  provider VARCHAR(30) NOT NULL DEFAULT 'asaas',
  event_id VARCHAR(150) UNIQUE NOT NULL,
  event_type VARCHAR(80) NOT NULL,
  payment_id VARCHAR(120),
  subscription_id VARCHAR(120),
  tenant_id UUID REFERENCES public.tenants(id) ON DELETE SET NULL,
  payload JSONB NOT NULL DEFAULT '{}'::JSONB,
  processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE public.payment_webhook_events ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS "Master admin can view payment_webhook_events" ON public.payment_webhook_events;
CREATE POLICY "Master admin can view payment_webhook_events"
  ON public.payment_webhook_events
  FOR SELECT
  TO authenticated
  USING (lower(COALESCE(auth.jwt()->>'email', '')) = 'lucaspereiracardoso2024@gmail.com');
