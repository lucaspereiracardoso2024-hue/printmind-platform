-- ============================================================================
-- PRINTMIND V5.0.1 - SECURITY INVOKER ON TRIGGER FUNCTIONS
-- Fixes privilege bypass where SECURITY DEFINER caused current_user = 'postgres'
-- inside triggers called via PostgREST authenticated sessions.
-- ============================================================================

CREATE OR REPLACE FUNCTION public.protect_tenant_billing_columns()
RETURNS trigger
LANGUAGE plpgsql
SECURITY INVOKER
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

CREATE OR REPLACE FUNCTION public.prevent_frozen_closing_mutation()
RETURNS trigger
LANGUAGE plpgsql
SECURITY INVOKER
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

CREATE OR REPLACE FUNCTION public.prevent_frozen_closing_items_mutation()
RETURNS trigger
LANGUAGE plpgsql
SECURITY INVOKER
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
