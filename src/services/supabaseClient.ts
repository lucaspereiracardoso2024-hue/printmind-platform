import { createClient, SupabaseClient } from '@supabase/supabase-js';

export const APP_VERSION = '4.2.0';
export const MASTER_ADMIN_EMAIL = 'lucaspereiracardoso2024@gmail.com';
export const AGENT_RELEASE_BASE =
  'https://github.com/lucaspereiracardoso2024-hue/printmind-platform/releases/latest/download';

const FALLBACK_PUBLIC_URL = 'https://zbsmzcbcivhzslcnfxmq.supabase.co';
const FALLBACK_PUBLIC_ANON =
  'eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJzdXBhYmFzZSIsInJlZiI6Inpic216Y2JjaXZoenNsY25meG1xIiwicm9sZSI6ImFub24iLCJpYXQiOjE3OTQ0NDUxNTcsImV4cCI6MjExMDAyMTE1N30.U9F-EL3ddBBqEK0MqkNuZBC6ZvwtuEElN6Z_VpEGT7Q';

const envUrl = String(import.meta.env.VITE_SUPABASE_URL ?? '').trim();
const envAnon = String(import.meta.env.VITE_SUPABASE_ANON_KEY ?? '').trim();

export const SUPABASE_URL = envUrl || FALLBACK_PUBLIC_URL;
export const SUPABASE_ANON_KEY = envAnon || FALLBACK_PUBLIC_ANON;

export const sb: SupabaseClient = createClient(SUPABASE_URL, SUPABASE_ANON_KEY, {
  auth: {
    persistSession: true,
    autoRefreshToken: true,
    detectSessionInUrl: true,
  },
});
