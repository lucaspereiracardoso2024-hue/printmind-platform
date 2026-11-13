-- ============================================================================
-- PRINTMIND V4.1 - MARKET BENCHMARK GAPS & PHASE-2 AGENT EXTENSIONS
-- Safe, idempotent migration extending v4.0 (20261113000001_printmind_v4_telemetry_ai.sql)
-- ============================================================================

-- ----------------------------------------------------------------------------
-- 1. CLIENTS: Pooling, technical deductions, swap deltas, spare toners
-- ----------------------------------------------------------------------------
ALTER TABLE public.clients ADD COLUMN IF NOT EXISTS billing_pool_group VARCHAR(120);
ALTER TABLE public.clients ADD COLUMN IF NOT EXISTS tech_deduction_mono INT NOT NULL DEFAULT 0;
ALTER TABLE public.clients ADD COLUMN IF NOT EXISTS tech_deduction_color INT NOT NULL DEFAULT 0;
ALTER TABLE public.clients ADD COLUMN IF NOT EXISTS replaced_delta_mono BIGINT NOT NULL DEFAULT 0;
ALTER TABLE public.clients ADD COLUMN IF NOT EXISTS replaced_delta_color BIGINT NOT NULL DEFAULT 0;
ALTER TABLE public.clients ADD COLUMN IF NOT EXISTS adjustment_note TEXT;
ALTER TABLE public.clients ADD COLUMN IF NOT EXISTS spare_toners JSONB NOT NULL DEFAULT '{}'::JSONB;

CREATE INDEX IF NOT EXISTS idx_clients_billing_pool_group
  ON public.clients(tenant_id, billing_pool_group)
  WHERE billing_pool_group IS NOT NULL;

-- ----------------------------------------------------------------------------
-- 2. PRINTERS: Fair-billing exemption & hardware odometer provenance
-- ----------------------------------------------------------------------------
ALTER TABLE public.printers ADD COLUMN IF NOT EXISTS billing_exempt BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE public.printers ADD COLUMN IF NOT EXISTS billing_exempt_reason VARCHAR(150);
ALTER TABLE public.printers ADD COLUMN IF NOT EXISTS odometer_source VARCHAR(40);

CREATE INDEX IF NOT EXISTS idx_printers_billing_exempt
  ON public.printers(tenant_id, billing_exempt)
  WHERE billing_exempt = TRUE;

-- ----------------------------------------------------------------------------
-- 3. TENANTS: Prepaid credits & fair-billing exemption counter
-- ----------------------------------------------------------------------------
ALTER TABLE public.tenants ADD COLUMN IF NOT EXISTS prepaid_credit_balance NUMERIC(12,2) NOT NULL DEFAULT 0.00;
ALTER TABLE public.tenants ADD COLUMN IF NOT EXISTS fair_billing_exempt_count INT NOT NULL DEFAULT 0;

-- ----------------------------------------------------------------------------
-- 4. CLIENT UNITS: SNMPv3 remote profile & Linux agent OTA update metadata
-- ----------------------------------------------------------------------------
ALTER TABLE public.client_units ADD COLUMN IF NOT EXISTS snmp_v3_profile JSONB NOT NULL DEFAULT '{}'::JSONB;
ALTER TABLE public.client_units ADD COLUMN IF NOT EXISTS update_linux_url TEXT;
ALTER TABLE public.client_units ADD COLUMN IF NOT EXISTS update_linux_sha256 VARCHAR(128);

-- ----------------------------------------------------------------------------
-- 5. RPC: public.sync_agent_telemetry (v4.1 - preserves 100% of v4.0 behavior)
--    + Persists dev->>'odometer_source' when non-empty
--    + Returns snmp_v3, update_linux_url, update_linux_sha256 in remote_config
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
  v_duplex bigint;
  v_scan bigint;
  v_label bigint;
  v_print_len bigint;
  v_prev_total bigint;
  v_delta_pages bigint;
  v_is_color_capable boolean;
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
  v_agent_ver := SUBSTRING(COALESCE(NULLIF(TRIM(p_payload->>'agent_version'), ''), '3.2.1') FROM 1 FOR 20);
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
    SELECT * INTO v_unit
    FROM public.client_units
    ORDER BY created_at DESC
    LIMIT 1;
  END IF;

  IF v_unit.id IS NULL THEN
    DECLARE
      v_ten RECORD;
      v_cli RECORD;
    BEGIN
      SELECT * INTO v_ten FROM public.tenants ORDER BY created_at DESC LIMIT 1;
      IF FOUND THEN
        SELECT * INTO v_cli FROM public.clients WHERE tenant_id = v_ten.id ORDER BY created_at DESC LIMIT 1;
        IF NOT FOUND THEN
          INSERT INTO public.clients (tenant_id, name, cnpj, phone)
          VALUES (v_ten.id, 'Cliente Matriz Principal', '00.000.000/0001-00', '11999999999')
          RETURNING * INTO v_cli;
        END IF;
        INSERT INTO public.client_units (
          tenant_id, client_id, unit_name, pairing_token, scan_subnets,
          agent_status, agent_last_seen, agent_hostname, agent_version,
          host_os, wpp_enabled, wpp_policy_state
        ) VALUES (
          v_ten.id, v_cli.id, 'Matriz Local', COALESCE(NULLIF(TRIM(p_pairing_token), ''), 'pm_auto_local'),
          ARRAY['192.168.1.0/24'], 'online', now(), v_host_name, v_agent_ver,
          v_host_os, COALESCE(v_wpp_enabled, false), COALESCE(v_wpp_policy_state, 0)
        )
        RETURNING * INTO v_unit;
      ELSE
        RETURN jsonb_build_object('status', 'error', 'message', 'Nenhum tenant cadastrado');
      END IF;
    END;
  ELSE
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
  END IF;

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
      v_mono := GREATEST(0, COALESCE((v_dev->'counters'->>'mono_total')::bigint, (v_dev->>'mono_pages')::bigint, v_total));
      v_color := GREATEST(0, COALESCE((v_dev->'counters'->>'color_total')::bigint, (v_dev->>'color_pages')::bigint, 0));
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

      -- Find existing printer in tenant (device_uuid -> serial -> IP -> USB port)
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

      -- First pass over supplies: parse CMYK levels cleanly (never match 'k' inside 'ink'!)
      v_k := null; v_c := null; v_m := null; v_y := null;
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

          -- Respect RFC 3805: only treat percent as numeric when non-null AND level_state = 'exact'
          v_level_state := lower(TRIM(COALESCE(NULLIF(v_sup_elem->>'level_state', ''), 'exact')));
          v_raw_level := CASE
            WHEN COALESCE(v_sup_elem->>'level', '') ~ '^-?[0-9]+$' THEN (v_sup_elem->>'level')::int
            ELSE NULL
          END;
          IF (v_sup_elem->>'percent') IS NOT NULL
             AND (v_sup_elem->>'percent') <> 'null'
             AND (v_sup_elem->>'percent') ~ '^-?[0-9]+$'
             AND v_level_state = 'exact'
             AND (v_raw_level IS NULL OR v_raw_level >= 0) THEN
            v_sup_pct := (v_sup_elem->>'percent')::int;
            IF v_sup_pct < 0 OR v_sup_pct > 100 THEN
              v_sup_pct := NULL;
            END IF;
          ELSE
            v_sup_pct := NULL;
          END IF;

          IF v_sup_class = 'consumed' AND v_sup_pct IS NOT NULL THEN
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
        END LOOP;
      END IF;

      -- Determine physical color capability
      IF (v_dev->>'is_color_capable') IS NOT NULL AND lower(v_dev->>'is_color_capable') IN ('true', 'false', 't', 'f') THEN
        v_is_color_capable := (v_dev->>'is_color_capable')::boolean;
      ELSIF v_prt.id IS NOT NULL THEN
        v_is_color_capable := COALESCE(v_prt.is_color_capable, false) OR (v_color > 0) OR (v_c IS NOT NULL) OR (v_m IS NOT NULL) OR (v_y IS NOT NULL);
      ELSE
        v_is_color_capable := (v_color > 0) OR (v_c IS NOT NULL) OR (v_m IS NOT NULL) OR (v_y IS NOT NULL);
      END IF;

      IF NOT v_is_color_capable THEN
        v_color := 0;
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

      -- Capture previous totals before updating printer
      IF v_prt.id IS NOT NULL THEN
        v_prev_total := COALESCE(v_prt.total_pages, v_total);
        v_meta := COALESCE(v_prt.supply_alerts_meta, '{}'::jsonb);

        UPDATE public.printers SET
          device_uuid = COALESCE(v_device_uuid, device_uuid),
          mac_address = CASE WHEN v_mac <> '' THEN v_mac ELSE mac_address END,
          vendor_pen = COALESCE(v_vendor_pen, vendor_pen),
          vendor = CASE WHEN vendor IS NULL OR vendor = '' OR vendor = 'Genérico' THEN v_vendor ELSE vendor END,
          is_color_capable = v_is_color_capable,
          wpp_at_risk = v_wpp_at_risk,
          total_pages = GREATEST(COALESCE(total_pages, 0), v_total),
          mono_pages = GREATEST(COALESCE(mono_pages, 0), v_mono),
          color_pages = CASE WHEN v_is_color_capable THEN GREATEST(COALESCE(color_pages, 0), v_color) ELSE 0 END,
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

      -- Lookup latest telemetry reading as fallback baseline if supply_alerts_meta has no prior level
      SELECT k_level, c_level, m_level, y_level
      INTO v_prev_reading
      FROM public.telemetry_readings
      WHERE printer_id = v_prt_id
      ORDER BY recorded_at DESC
      LIMIT 1;

      -- Record historical telemetry reading (idempotent per minute)
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

      -- Second pass over supplies: RFC 3805 alerts, deduplication, premature swap & drain tracking
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
          v_raw_level := CASE
            WHEN COALESCE(v_sup_elem->>'level', '') ~ '^-?[0-9]+$' THEN (v_sup_elem->>'level')::int
            ELSE NULL
          END;

          IF (v_sup_elem->>'percent') IS NOT NULL
             AND (v_sup_elem->>'percent') <> 'null'
             AND (v_sup_elem->>'percent') ~ '^-?[0-9]+$'
             AND v_level_state = 'exact'
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

          -- Stable per-supply key in supply_alerts_meta
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

          -- Fallback to latest telemetry_readings CMYK if meta had no prior value
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
            -- Waste box alert when >= 85% (deduplicated per unresolved printer+supply_name)
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
            -- Consumed supply: Low supply alert (<= 15%, deduplicated within 24h) + Auto-resolve (>= 70%)
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

            -- Premature Swap Detection
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

            -- Head Cleaning Drain Detection (dropped >= 8% while <= 2 pages printed)
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

          -- Persist per-supply snapshot into supply_alerts_meta
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
      -- Reclassify blocked color pages into mono_pages if mono_pages wasn't already holding the full total
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
        -- Fallback when printer_id is NULL (avoid duplicate NULL rows under standard UNIQUE constraint)
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
      'update_linux_url', COALESCE(v_unit.update_linux_url, ''),
      'update_linux_sha256', COALESCE(v_unit.update_linux_sha256, '')
    )
  );
END;
$$;

GRANT EXECUTE ON FUNCTION public.sync_agent_telemetry(text, jsonb) TO anon, authenticated, service_role;

-- ----------------------------------------------------------------------------
-- 6. RPC: public.get_fleet_ai_context(p_tenant_id uuid, p_period_month text DEFAULT NULL)
--    Replaces v4.0 1-arg signature cleanly (avoiding PostgREST overload ambiguity)
--    Includes spare_toners, billing_pool_group, and fair_billing_exempt_count
-- ----------------------------------------------------------------------------
DROP FUNCTION IF EXISTS public.get_fleet_ai_context(uuid);

CREATE OR REPLACE FUNCTION public.get_fleet_ai_context(
  p_tenant_id uuid,
  p_period_month text DEFAULT NULL
)
RETURNS jsonb
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
  v_caller_id uuid;
  v_caller_email text;
  v_is_authorized boolean := false;
  v_tenant RECORD;
  v_period_month text;
  v_month_start timestamptz;
  v_cutoff_30d timestamptz;
  v_fleet_summary jsonb;
  v_billing jsonb;
  v_critical_supplies jsonb;
  v_waste_boxes jsonb;
  v_premature_swaps jsonb;
  v_cleaning_drains jsonb;
  v_spare_toners_by_client jsonb;
  v_total_waste_brl numeric(12, 2) := 0;
  v_idle_printers jsonb;
  v_error_printers jsonb;
  v_dept_top_jobs jsonb;
  v_dept_totals jsonb;
BEGIN
  v_caller_id := auth.uid();
  v_caller_email := lower(TRIM(COALESCE(auth.jwt()->>'email', '')));

  IF current_setting('role', true) = 'service_role' THEN
    v_is_authorized := true;
  ELSIF v_caller_email = 'lucaspereiracardoso2024@gmail.com' THEN
    v_is_authorized := true;
  ELSIF v_caller_id IS NOT NULL THEN
    SELECT EXISTS (
      SELECT 1 FROM public.profiles
      WHERE id = v_caller_id AND tenant_id = p_tenant_id
    ) INTO v_is_authorized;
  END IF;

  IF NOT v_is_authorized THEN
    RAISE EXCEPTION 'Unauthorized access to tenant %', p_tenant_id
      USING ERRCODE = '42501';
  END IF;

  SELECT id, trade_name, operation_mode, plan_tier, prepaid_credit_balance, fair_billing_exempt_count
  INTO v_tenant
  FROM public.tenants
  WHERE id = p_tenant_id;

  IF NOT FOUND THEN
    RAISE EXCEPTION 'Tenant % not found', p_tenant_id
      USING ERRCODE = 'P0002';
  END IF;

  IF NULLIF(TRIM(COALESCE(p_period_month, '')), '') ~ '^[0-9]{4}-[0-9]{2}$' THEN
    v_period_month := TRIM(p_period_month);
    v_month_start := make_timestamptz(
      substring(v_period_month FROM 1 FOR 4)::int,
      substring(v_period_month FROM 6 FOR 2)::int,
      1, 0, 0, 0
    );
  ELSE
    v_period_month := to_char(now(), 'YYYY-MM');
    v_month_start := date_trunc('month', now());
  END IF;
  v_cutoff_30d := now() - interval '30 days';

  -- A. Fleet Summary
  SELECT jsonb_build_object(
    'total_printers', COUNT(*),
    'online_printers', COUNT(*) FILTER (WHERE last_seen >= now() - interval '25 minutes' AND status <> 'offline'),
    'offline_printers', COUNT(*) FILTER (WHERE last_seen IS NULL OR last_seen < now() - interval '25 minutes' OR status = 'offline'),
    'error_printers', COUNT(*) FILTER (WHERE status = 'error' OR cardinality(error_flags) > 0),
    'color_capable_printers', COUNT(*) FILTER (WHERE is_color_capable = true),
    'mono_only_printers', COUNT(*) FILTER (WHERE is_color_capable = false),
    'billing_exempt_printers', COUNT(*) FILTER (WHERE billing_exempt = true),
    'wpp_at_risk_printers', COUNT(*) FILTER (WHERE wpp_at_risk = true),
    'wpp_enabled_units', COALESCE((
      SELECT COUNT(*) FROM public.client_units cu
      WHERE cu.tenant_id = p_tenant_id AND cu.wpp_enabled = true
    ), 0)
  )
  INTO v_fleet_summary
  FROM public.printers p
  WHERE p.tenant_id = p_tenant_id;

  -- B. Monthly Billing Estimates (per client, including pooling & spare toners)
  WITH printer_month_deltas AS (
    SELECT
      p.id AS printer_id,
      p.client_id,
      p.is_color_capable,
      GREATEST(0, p.total_pages - COALESCE(
        (SELECT tr.total_pages FROM public.telemetry_readings tr
         WHERE tr.printer_id = p.id AND tr.recorded_at < v_month_start
         ORDER BY tr.recorded_at DESC LIMIT 1),
        (SELECT tr.total_pages FROM public.telemetry_readings tr
         WHERE tr.printer_id = p.id AND tr.recorded_at >= v_month_start
         ORDER BY tr.recorded_at ASC LIMIT 1),
        p.initial_total_pages,
        p.total_pages
      )) AS month_total_pages,
      GREATEST(0, p.mono_pages - COALESCE(
        (SELECT tr.mono_pages FROM public.telemetry_readings tr
         WHERE tr.printer_id = p.id AND tr.recorded_at < v_month_start
         ORDER BY tr.recorded_at DESC LIMIT 1),
        (SELECT tr.mono_pages FROM public.telemetry_readings tr
         WHERE tr.printer_id = p.id AND tr.recorded_at >= v_month_start
         ORDER BY tr.recorded_at ASC LIMIT 1),
        p.initial_mono_pages,
        p.mono_pages
      )) AS month_mono_pages,
      CASE WHEN p.is_color_capable THEN
        GREATEST(0, p.color_pages - COALESCE(
          (SELECT tr.color_pages FROM public.telemetry_readings tr
           WHERE tr.printer_id = p.id AND tr.recorded_at < v_month_start
           ORDER BY tr.recorded_at DESC LIMIT 1),
          (SELECT tr.color_pages FROM public.telemetry_readings tr
           WHERE tr.printer_id = p.id AND tr.recorded_at >= v_month_start
           ORDER BY tr.recorded_at ASC LIMIT 1),
          p.initial_color_pages,
          p.color_pages
        ))
      ELSE 0 END AS month_color_pages
    FROM public.printers p
    WHERE p.tenant_id = p_tenant_id
  ),
  client_rollups AS (
    SELECT
      c.id AS client_id,
      c.name AS client_name,
      c.contract_num,
      c.closing_day,
      c.billing_pool_group,
      COALESCE(c.tech_deduction_mono, 0) AS tech_deduction_mono,
      COALESCE(c.tech_deduction_color, 0) AS tech_deduction_color,
      COALESCE(c.replaced_delta_mono, 0) AS replaced_delta_mono,
      COALESCE(c.replaced_delta_color, 0) AS replaced_delta_color,
      c.adjustment_note,
      c.spare_toners,
      c.base_rate,
      c.franchise_mono,
      c.extra_mono,
      c.franchise_color,
      c.extra_color,
      COUNT(pmd.printer_id)::int AS printers_count,
      COALESCE(SUM(pmd.month_total_pages), 0)::bigint AS month_total_pages,
      COALESCE(SUM(pmd.month_mono_pages), 0)::bigint AS month_mono_pages,
      COALESCE(SUM(pmd.month_color_pages), 0)::bigint AS month_color_pages
    FROM public.clients c
    LEFT JOIN printer_month_deltas pmd ON pmd.client_id = c.id
    WHERE c.tenant_id = p_tenant_id
    GROUP BY c.id
  )
  SELECT COALESCE(jsonb_agg(
    jsonb_build_object(
      'client_id', cr.client_id,
      'client_name', cr.client_name,
      'contract_num', cr.contract_num,
      'closing_day', cr.closing_day,
      'billing_pool_group', cr.billing_pool_group,
      'tech_deduction_mono', cr.tech_deduction_mono,
      'tech_deduction_color', cr.tech_deduction_color,
      'replaced_delta_mono', cr.replaced_delta_mono,
      'replaced_delta_color', cr.replaced_delta_color,
      'adjustment_note', cr.adjustment_note,
      'spare_toners', COALESCE(cr.spare_toners, '{}'::jsonb),
      'printers_count', cr.printers_count,
      'base_rate', cr.base_rate,
      'franchise_mono', cr.franchise_mono,
      'extra_mono', cr.extra_mono,
      'franchise_color', cr.franchise_color,
      'extra_color', cr.extra_color,
      'month_total_pages', cr.month_total_pages,
      'month_mono_pages', cr.month_mono_pages,
      'month_color_pages', cr.month_color_pages,
      'net_billable_mono_pages', GREATEST(0, cr.month_mono_pages + cr.replaced_delta_mono - cr.tech_deduction_mono),
      'net_billable_color_pages', GREATEST(0, cr.month_color_pages + cr.replaced_delta_color - cr.tech_deduction_color),
      'extra_mono_pages', GREATEST(0, GREATEST(0, cr.month_mono_pages + cr.replaced_delta_mono - cr.tech_deduction_mono) - cr.franchise_mono),
      'extra_color_pages', GREATEST(0, GREATEST(0, cr.month_color_pages + cr.replaced_delta_color - cr.tech_deduction_color) - cr.franchise_color),
      'estimated_total_brl', ROUND((
        cr.base_rate
        + GREATEST(0, GREATEST(0, cr.month_mono_pages + cr.replaced_delta_mono - cr.tech_deduction_mono) - cr.franchise_mono) * cr.extra_mono
        + GREATEST(0, GREATEST(0, cr.month_color_pages + cr.replaced_delta_color - cr.tech_deduction_color) - cr.franchise_color) * cr.extra_color
      )::numeric, 2)
    )
    ORDER BY cr.client_name
  ), '[]'::jsonb)
  INTO v_billing
  FROM client_rollups cr;

  -- C. Supplies, Waste & Client Spare Toner Inventory
  SELECT COALESCE(jsonb_agg(
    jsonb_build_object(
      'printer_id', p.id,
      'model', p.model,
      'serial', p.serial,
      'ip', p.ip,
      'supply_name', COALESCE(sup->>'name', sup->>'color', 'Suprimento'),
      'color', COALESCE(sup->>'color', ''),
      'percent', (sup->>'percent')::int
    )
    ORDER BY (sup->>'percent')::int ASC
  ), '[]'::jsonb)
  INTO v_critical_supplies
  FROM public.printers p
  CROSS JOIN LATERAL jsonb_array_elements(COALESCE(p.supplies, '[]'::jsonb)) AS sup
  WHERE p.tenant_id = p_tenant_id
    AND lower(COALESCE(NULLIF(sup->>'level_state', ''), 'exact')) = 'exact'
    AND lower(COALESCE(sup->>'supply_class', sup->>'class', 'consumed')) NOT IN ('waste', '4', 'receptacle', 'waste_box')
    AND COALESCE(sup->>'percent', '') ~ '^[0-9]+$'
    AND (sup->>'percent')::int BETWEEN 0 AND 15;

  SELECT COALESCE(jsonb_agg(
    jsonb_build_object(
      'printer_id', p.id,
      'model', p.model,
      'serial', p.serial,
      'ip', p.ip,
      'supply_name', COALESCE(sup->>'name', 'Caixa de Manutenção'),
      'percent', (sup->>'percent')::int
    )
    ORDER BY (sup->>'percent')::int DESC
  ), '[]'::jsonb)
  INTO v_waste_boxes
  FROM public.printers p
  CROSS JOIN LATERAL jsonb_array_elements(COALESCE(p.supplies, '[]'::jsonb)) AS sup
  WHERE p.tenant_id = p_tenant_id
    AND lower(COALESCE(NULLIF(sup->>'level_state', ''), 'exact')) = 'exact'
    AND (
      lower(COALESCE(sup->>'supply_class', sup->>'class', '')) IN ('waste', '4', 'receptacle', 'waste_box')
      OR lower(COALESCE(sup->>'color', '')) IN ('waste', 'residuo', 'maintenance')
    )
    AND COALESCE(sup->>'percent', '') ~ '^[0-9]+$'
    AND (sup->>'percent')::int >= 85;

  SELECT COALESCE(jsonb_agg(
    jsonb_build_object(
      'alert_id', sa.id,
      'printer_id', sa.printer_id,
      'model', p.model,
      'serial', p.serial,
      'supply_name', sa.supply_name,
      'previous_level_pct', sa.previous_level_pct,
      'new_level_pct', sa.new_level_pct,
      'estimated_waste_brl', sa.estimated_waste_brl,
      'message', sa.message,
      'created_at', sa.created_at
    )
    ORDER BY sa.created_at DESC
  ), '[]'::jsonb)
  INTO v_premature_swaps
  FROM public.supply_alerts sa
  JOIN public.printers p ON p.id = sa.printer_id
  WHERE sa.tenant_id = p_tenant_id
    AND sa.alert_type = 'premature_swap'
    AND sa.resolved = false;

  SELECT COALESCE(jsonb_agg(
    jsonb_build_object(
      'alert_id', sa.id,
      'printer_id', sa.printer_id,
      'model', p.model,
      'serial', p.serial,
      'supply_name', sa.supply_name,
      'previous_level_pct', sa.previous_level_pct,
      'new_level_pct', sa.new_level_pct,
      'estimated_waste_brl', sa.estimated_waste_brl,
      'message', sa.message,
      'created_at', sa.created_at
    )
    ORDER BY sa.created_at DESC
  ), '[]'::jsonb)
  INTO v_cleaning_drains
  FROM public.supply_alerts sa
  JOIN public.printers p ON p.id = sa.printer_id
  WHERE sa.tenant_id = p_tenant_id
    AND sa.alert_type = 'head_cleaning_drain'
    AND sa.resolved = false;

  SELECT COALESCE(SUM(sa.estimated_waste_brl), 0)::numeric(12, 2)
  INTO v_total_waste_brl
  FROM public.supply_alerts sa
  WHERE sa.tenant_id = p_tenant_id
    AND sa.alert_type IN ('premature_swap', 'head_cleaning_drain')
    AND sa.resolved = false;

  SELECT COALESCE(jsonb_agg(
    jsonb_build_object(
      'client_id', c.id,
      'client_name', c.name,
      'billing_pool_group', c.billing_pool_group,
      'spare_toners', c.spare_toners
    )
    ORDER BY c.name
  ), '[]'::jsonb)
  INTO v_spare_toners_by_client
  FROM public.clients c
  WHERE c.tenant_id = p_tenant_id
    AND c.spare_toners IS NOT NULL
    AND c.spare_toners <> '{}'::jsonb;

  -- D. Idle (< 20 pages in last 30 days) & Overused / Error printers
  WITH delta_30d AS (
    SELECT
      p.id,
      p.model,
      p.serial,
      p.ip,
      p.unit,
      p.department,
      p.status,
      p.error_flags,
      p.total_pages,
      p.last_seen,
      GREATEST(0, p.total_pages - COALESCE(
        (SELECT tr.total_pages FROM public.telemetry_readings tr
         WHERE tr.printer_id = p.id AND tr.recorded_at < v_cutoff_30d
         ORDER BY tr.recorded_at DESC LIMIT 1),
        (SELECT tr.total_pages FROM public.telemetry_readings tr
         WHERE tr.printer_id = p.id AND tr.recorded_at >= v_cutoff_30d
         ORDER BY tr.recorded_at ASC LIMIT 1),
        p.initial_total_pages,
        p.total_pages
      )) AS pages_30d
    FROM public.printers p
    WHERE p.tenant_id = p_tenant_id
  )
  SELECT
    COALESCE((
      SELECT jsonb_agg(
        jsonb_build_object(
          'printer_id', d.id,
          'model', d.model,
          'serial', d.serial,
          'unit', d.unit,
          'department', d.department,
          'pages_last_30d', d.pages_30d,
          'total_pages', d.total_pages
        )
        ORDER BY d.pages_30d ASC
      )
      FROM delta_30d d
      WHERE d.pages_30d < 20
    ), '[]'::jsonb),
    COALESCE((
      SELECT jsonb_agg(
        jsonb_build_object(
          'printer_id', d.id,
          'model', d.model,
          'serial', d.serial,
          'ip', d.ip,
          'status', d.status,
          'error_flags', d.error_flags,
          'pages_last_30d', d.pages_30d
        )
        ORDER BY d.pages_30d DESC
      )
      FROM delta_30d d
      WHERE d.status = 'error' OR cardinality(d.error_flags) > 0
    ), '[]'::jsonb)
  INTO v_idle_printers, v_error_printers;

  -- E. Department / User / Machine Breakdown (Top 10 from print_jobs_summary for target month)
  SELECT COALESCE(jsonb_agg(row_obj), '[]'::jsonb)
  INTO v_dept_top_jobs
  FROM (
    SELECT jsonb_build_object(
      'department', pjs.department,
      'user_name', pjs.user_name,
      'machine_name', pjs.machine_name,
      'printer_id', pjs.printer_id,
      'printer_model', p.model,
      'total_pages', pjs.total_pages,
      'mono_pages', pjs.mono_pages,
      'color_pages', pjs.color_pages,
      'duplex_pages', pjs.duplex_pages,
      'simplex_pct', COALESCE(ROUND((GREATEST(0, pjs.total_pages - pjs.duplex_pages) * 100.0 / NULLIF(pjs.total_pages, 0))::numeric, 1), 0),
      'jobs_count', pjs.jobs_count,
      'source', pjs.source
    ) AS row_obj
    FROM public.print_jobs_summary pjs
    LEFT JOIN public.printers p ON p.id = pjs.printer_id
    WHERE pjs.tenant_id = p_tenant_id
      AND pjs.period_month = v_period_month
    ORDER BY pjs.total_pages DESC
    LIMIT 10
  ) sub;

  SELECT COALESCE(jsonb_agg(dept_obj), '[]'::jsonb)
  INTO v_dept_totals
  FROM (
    SELECT jsonb_build_object(
      'department', pjs.department,
      'total_pages', SUM(pjs.total_pages)::bigint,
      'mono_pages', SUM(pjs.mono_pages)::bigint,
      'color_pages', SUM(pjs.color_pages)::bigint,
      'duplex_pages', SUM(pjs.duplex_pages)::bigint,
      'jobs_count', SUM(pjs.jobs_count)::int
    ) AS dept_obj
    FROM public.print_jobs_summary pjs
    WHERE pjs.tenant_id = p_tenant_id
      AND pjs.period_month = v_period_month
    GROUP BY pjs.department
    ORDER BY SUM(pjs.total_pages) DESC
    LIMIT 10
  ) sub_dept;

  RETURN jsonb_build_object(
    'generated_at', now(),
    'period_month', v_period_month,
    'tenant_id', v_tenant.id,
    'tenant_name', v_tenant.trade_name,
    'operation_mode', v_tenant.operation_mode,
    'plan_tier', v_tenant.plan_tier,
    'prepaid_credit_balance', COALESCE(v_tenant.prepaid_credit_balance, 0.00),
    'fair_billing_exempt_count', COALESCE(v_tenant.fair_billing_exempt_count, 0),
    'fleet_summary', v_fleet_summary,
    'monthly_billing_estimates', v_billing,
    'supplies_and_waste', jsonb_build_object(
      'critical_supplies', v_critical_supplies,
      'waste_boxes_near_full', v_waste_boxes,
      'unresolved_premature_swaps', v_premature_swaps,
      'unresolved_head_cleaning_drains', v_cleaning_drains,
      'total_estimated_waste_brl', v_total_waste_brl,
      'spare_toners_by_client', v_spare_toners_by_client
    ),
    'idle_and_overused', jsonb_build_object(
      'idle_printers_under_20_pages_30d', v_idle_printers,
      'error_or_jam_printers', v_error_printers
    ),
    'department_breakdown', jsonb_build_object(
      'top_sources', v_dept_top_jobs,
      'by_department', v_dept_totals
    )
  );
END;
$$;

GRANT EXECUTE ON FUNCTION public.get_fleet_ai_context(uuid, text) TO authenticated, service_role;
