-- ============================================================================
-- Migration 20261114000004: Ensure legacy/v5 schema compatibility on public.closings
-- ============================================================================

ALTER TABLE public.closings ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
ALTER TABLE public.closings ADD COLUMN IF NOT EXISTS client_name VARCHAR(150) NOT NULL DEFAULT '';
ALTER TABLE public.closings ADD COLUMN IF NOT EXISTS month_year VARCHAR(7) NOT NULL DEFAULT to_char(now(), 'YYYY-MM');
ALTER TABLE public.closings ALTER COLUMN client_name SET DEFAULT '';
ALTER TABLE public.closings ALTER COLUMN month_year SET DEFAULT to_char(now(), 'YYYY-MM');
ALTER TABLE public.closings DROP CONSTRAINT IF EXISTS closings_status_check;
ALTER TABLE public.closings ADD CONSTRAINT closings_status_check
  CHECK (status::text = ANY (ARRAY['aberto', 'processado', 'congelado', 'faturado', 'pago', 'vencido']));

NOTIFY pgrst, 'reload schema';
