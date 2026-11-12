-- ============================================================================
-- PRINTMIND V3.0 - PRODUCTION MULTI-TENANT SCHEMA, RLS & RPC ENGINE (SUPABASE)
-- ============================================================================

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- 1. TENANTS (Locadoras de Impressão / Gráficas Assinantes)
CREATE TABLE IF NOT EXISTS public.tenants (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    trade_name VARCHAR(150) NOT NULL,
    legal_name VARCHAR(150),
    cnpj_cpf VARCHAR(20) UNIQUE,
    email VARCHAR(150) NOT NULL,
    phone VARCHAR(25),
    operation_mode VARCHAR(20) NOT NULL DEFAULT 'hibrido' CHECK (operation_mode IN ('locacao', 'grafica', 'hibrido')),
    plan_tier VARCHAR(30) NOT NULL DEFAULT 'trial' CHECK (plan_tier IN ('trial', 'starter', 'pro', 'enterprise')),
    subscription_status VARCHAR(25) NOT NULL DEFAULT 'active' CHECK (subscription_status IN ('active', 'past_due', 'canceled', 'trialing')),
    gateway_customer_id VARCHAR(100),
    gateway_subscription_id VARCHAR(100),
    max_printers INT NOT NULL DEFAULT 30,
    baas_provider VARCHAR(30) NOT NULL DEFAULT 'asaas',
    baas_subaccount_id VARCHAR(100),
    baas_wallet_id VARCHAR(100),
    baas_api_key_encrypted TEXT,
    baas_onboarding_status VARCHAR(30) NOT NULL DEFAULT 'pending',
    platform_split_percent NUMERIC(5, 2) NOT NULL DEFAULT 1.50,
    platform_split_fixed NUMERIC(10, 2) NOT NULL DEFAULT 0.00,
    pix_key VARCHAR(120),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 2. PROFILES (Vinculado nativamente ao Supabase Auth: Email/Senha + Google OAuth 2.0)
CREATE TABLE IF NOT EXISTS public.profiles (
    id UUID PRIMARY KEY REFERENCES auth.users(id) ON DELETE CASCADE,
    tenant_id UUID NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,
    client_id UUID,
    full_name VARCHAR(120) NOT NULL,
    email VARCHAR(150) NOT NULL,
    avatar_url TEXT,
    role VARCHAR(30) NOT NULL DEFAULT 'owner' CHECK (role IN ('owner', 'admin', 'tech', 'finance', 'client_viewer')),
    onboarding_completed BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_profiles_tenant_id ON public.profiles(tenant_id);
CREATE INDEX IF NOT EXISTS idx_profiles_client_id ON public.profiles(client_id);

-- 3. WHATSAPP INSTANCES (Servidor Open-Source Evolution API / whatsmeow)
CREATE TABLE IF NOT EXISTS public.whatsapp_instances (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID UNIQUE NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,
    instance_name VARCHAR(80) UNIQUE NOT NULL,
    server_url TEXT NOT NULL DEFAULT 'http://localhost:8085',
    phone_connected VARCHAR(25),
    status VARCHAR(25) NOT NULL DEFAULT 'disconnected' CHECK (status IN ('connected', 'qr_ready', 'connecting', 'disconnected')),
    qr_code_base64 TEXT,
    auto_send_billing BOOLEAN NOT NULL DEFAULT TRUE,
    auto_send_supply_alerts BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 4. CLIENTS (Clientes Contratantes: Hospitais, Clínicas, Escritórios)
CREATE TABLE IF NOT EXISTS public.clients (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,
    name VARCHAR(150) NOT NULL,
    cnpj VARCHAR(20),
    email_billing VARCHAR(150),
    phone VARCHAR(25),
    address TEXT,
    closing_day INT NOT NULL DEFAULT 15 CHECK (closing_day BETWEEN 1 AND 31),
    due_days_offset INT NOT NULL DEFAULT 8,
    contract_num VARCHAR(50),
    base_rate NUMERIC(12, 2) NOT NULL DEFAULT 0.00,
    franchise_mono INT NOT NULL DEFAULT 0,
    extra_mono NUMERIC(10, 4) NOT NULL DEFAULT 0.0500,
    franchise_color INT NOT NULL DEFAULT 0,
    extra_color NUMERIC(10, 4) NOT NULL DEFAULT 0.1000,
    is_corporate BOOLEAN NOT NULL DEFAULT FALSE,
    units TEXT[] NOT NULL DEFAULT '{}',
    departments TEXT[] NOT NULL DEFAULT '{}',
    baas_customer_id VARCHAR(100),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_clients_tenant_id ON public.clients(tenant_id);

-- 5. CLIENT UNITS (Filiais / Unidades Físicas onde roda o printmind-agent.exe)
CREATE TABLE IF NOT EXISTS public.client_units (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,
    client_id UUID NOT NULL REFERENCES public.clients(id) ON DELETE CASCADE,
    unit_name VARCHAR(120) NOT NULL,
    address TEXT,
    pairing_token VARCHAR(100) UNIQUE NOT NULL DEFAULT ('pm_live_' || encode(gen_random_bytes(16), 'hex')),
    scan_subnets TEXT[] NOT NULL DEFAULT ARRAY['192.168.1.0/24'],
    static_ips TEXT[] NOT NULL DEFAULT '{}',
    poll_interval_sec INT NOT NULL DEFAULT 900,
    rate_limit_pps INT NOT NULL DEFAULT 20,
    enable_usb_spooler BOOLEAN NOT NULL DEFAULT TRUE,
    enable_http_wakeup BOOLEAN NOT NULL DEFAULT TRUE,
    agent_version VARCHAR(20),
    agent_hostname VARCHAR(100),
    agent_local_ip VARCHAR(45),
    agent_status VARCHAR(20) NOT NULL DEFAULT 'pending',
    agent_last_seen TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_client_units_tenant_id ON public.client_units(tenant_id);
CREATE INDEX IF NOT EXISTS idx_client_units_client_id ON public.client_units(client_id);

-- 6. CLIENT DEPARTMENTS (Setores / Centros de Custo Hospitalares)
CREATE TABLE IF NOT EXISTS public.client_departments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,
    client_id UUID NOT NULL REFERENCES public.clients(id) ON DELETE CASCADE,
    dept_name VARCHAR(120) NOT NULL,
    cost_center_code VARCHAR(50),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_client_departments_tenant_id ON public.client_departments(tenant_id);
CREATE INDEX IF NOT EXISTS idx_client_departments_client_id ON public.client_departments(client_id);

-- 7. PRINTERS (Ativos Físicos identificados por Serial + MAC, suportando SNMP e USB)
CREATE TABLE IF NOT EXISTS public.printers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,
    client_id UUID REFERENCES public.clients(id) ON DELETE SET NULL,
    unit_id UUID REFERENCES public.client_units(id) ON DELETE SET NULL,
    department_id UUID REFERENCES public.client_departments(id) ON DELETE SET NULL,
    unit VARCHAR(120),
    department VARCHAR(120),
    location VARCHAR(120),
    ip VARCHAR(45),
    usb_port VARCHAR(30),
    mac_address VARCHAR(30),
    hostname VARCHAR(100),
    vendor_pen INT,
    vendor VARCHAR(60),
    model VARCHAR(120) NOT NULL,
    serial VARCHAR(100) NOT NULL,
    discovery_source VARCHAR(30) NOT NULL DEFAULT 'snmp_v2c',
    is_color_capable BOOLEAN NOT NULL DEFAULT FALSE,
    ownership VARCHAR(20) NOT NULL DEFAULT 'locada' CHECK (ownership IN ('locada', 'propria', 'cliente')),
    status VARCHAR(20) NOT NULL DEFAULT 'online' CHECK (status IN ('online', 'offline', 'error', 'maintenance')),
    latency_ms INT NOT NULL DEFAULT 0,
    total_pages BIGINT NOT NULL DEFAULT 0,
    mono_pages BIGINT NOT NULL DEFAULT 0,
    color_pages BIGINT NOT NULL DEFAULT 0,
    initial_total_pages BIGINT NOT NULL DEFAULT 0,
    initial_mono_pages BIGINT NOT NULL DEFAULT 0,
    initial_color_pages BIGINT NOT NULL DEFAULT 0,
    duplex_sheets BIGINT NOT NULL DEFAULT 0,
    a3_pages BIGINT NOT NULL DEFAULT 0,
    scan_pages BIGINT NOT NULL DEFAULT 0,
    label_count BIGINT,
    print_length_cm BIGINT,
    supplies JSONB NOT NULL DEFAULT '[]'::JSONB,
    error_flags TEXT[] NOT NULL DEFAULT '{}',
    last_seen TIMESTAMPTZ DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, serial)
);

-- 8. PRINTER ALLOCATIONS, 9. TELEMETRY READINGS, 10. SUPPLY ALERTS, 11. CLOSINGS, 12. CLOSING ALLOCATIONS, 13. PAYMENT WEBHOOKS
-- (Aplicados integralmente via Supabase Migrations no projeto zbsmzcbcivhzslcnfxmq)
