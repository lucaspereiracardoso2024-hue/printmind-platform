import type { Session } from '@supabase/supabase-js';
import { MASTER_ADMIN_EMAIL, sb } from '../services/supabaseClient';
import { appState, byId, loadTenantData } from '../state/store';
import { getErrorMessage } from '../utils/format';
import { applyOperationModeUI, checkTrialPrinterLock, syncMasterAccess } from './navigation';
import { Profile, Tenant } from '../types/models';

type AppReadyCallback = () => void;
let onAuthenticatedRenderCallback: AppReadyCallback | null = null;

export function showAuthAlert(msg: string, isError = true): void {
  const el = byId('authAlert');
  if (!el) return;
  el.innerText = msg;
  el.classList.remove('hidden', 'bg-rose-50', 'text-rose-700', 'border-rose-200', 'bg-emerald-50', 'text-emerald-700', 'border-emerald-200');
  if (isError) {
    el.classList.add('bg-rose-50', 'text-rose-700', 'border-rose-200');
  } else {
    el.classList.add('bg-emerald-50', 'text-emerald-700', 'border-emerald-200');
  }
}

export function switchAuthTab(tab: 'login' | 'signup' | 'forgot' | 'reset'): void {
  const loginForm = byId('formLogin');
  const signupForm = byId('formSignup');
  const forgotForm = byId('formForgotPassword');
  const resetForm = byId('formResetPassword');
  const btnLogin = byId('tabBtnLogin');
  const btnSignup = byId('tabBtnSignup');

  [loginForm, signupForm, forgotForm, resetForm].forEach(f => f?.classList.add('hidden'));
  byId('authAlert')?.classList.add('hidden');

  if (tab === 'login') {
    loginForm?.classList.remove('hidden');
    if (btnLogin) btnLogin.className = 'py-2 rounded-xl bg-white text-slate-900 shadow-sm transition font-bold';
    if (btnSignup) btnSignup.className = 'py-2 rounded-xl text-slate-600 hover:text-slate-900 transition font-semibold';
  } else if (tab === 'signup') {
    signupForm?.classList.remove('hidden');
    if (btnSignup) btnSignup.className = 'py-2 rounded-xl bg-white text-slate-900 shadow-sm transition font-bold';
    if (btnLogin) btnLogin.className = 'py-2 rounded-xl text-slate-600 hover:text-slate-900 transition font-semibold';
  } else if (tab === 'forgot') {
    forgotForm?.classList.remove('hidden');
  } else if (tab === 'reset') {
    resetForm?.classList.remove('hidden');
  }
}

async function ensureTenantAndProfileForUser(user: Session['user']): Promise<void> {
  const { data: profData, error: profErr } = await sb
    .from('profiles')
    .select('*')
    .eq('id', user.id)
    .maybeSingle();

  if (profErr) {
    console.warn('Erro ao buscar profile:', profErr.message);
  }

  let profile = profData as Profile | null;
  if (!profile) {
    const meta = (user.user_metadata ?? {}) as Record<string, unknown>;
    const tradeName =
      String(meta.trade_name ?? meta.full_name ?? user.email?.split('@')[0] ?? 'Minha Locadora').trim();
    const cnpjCpf = String(meta.cnpj_cpf ?? '').trim() || null;

    const { data: newTen, error: tenErr } = await sb
      .from('tenants')
      .insert({
        trade_name: tradeName,
        legal_name: tradeName,
        cnpj_cpf: cnpjCpf,
        email: user.email ?? '',
        plan_tier: 'trial',
        subscription_status: 'trialing',
        max_printers: 10,
      })
      .select('*')
      .single();

    if (tenErr || !newTen) {
      throw new Error(tenErr?.message ?? 'Falha ao provisionar conta da empresa.');
    }

    const { data: createdProf, error: createProfErr } = await sb
      .from('profiles')
      .insert({
        id: user.id,
        tenant_id: (newTen as Tenant).id,
        full_name: String(meta.full_name ?? tradeName),
        role: 'admin',
      })
      .select('*')
      .single();

    if (createProfErr || !createdProf) {
      throw new Error(createProfErr?.message ?? 'Falha ao criar perfil administrativo.');
    }

    profile = createdProf as Profile;
    appState.tenant = newTen as Tenant;
  }

  appState.profile = profile;

  if (!appState.tenant && profile.tenant_id) {
    const { data: tenData } = await sb
      .from('tenants')
      .select('*')
      .eq('id', profile.tenant_id)
      .single();
    if (tenData) {
      appState.tenant = tenData as Tenant;
    }
  }
}

export async function onSessionAuthenticated(session: Session): Promise<void> {
  appState.session = session;
  byId('authScreen')?.classList.add('hidden');
  byId('appScreen')?.classList.remove('hidden');

  await ensureTenantAndProfileForUser(session.user);

  const userEmailEl = byId('headerUserEmail');
  if (userEmailEl) {
    userEmailEl.innerText = session.user.email ?? '';
  }
  const tenNameEl = byId('headerTenantName');
  if (tenNameEl && appState.tenant) {
    tenNameEl.innerText = appState.tenant.trade_name;
  }

  syncMasterAccess(session.user.email ?? '');
  applyOperationModeUI(appState.tenant?.operation_mode ?? 'outsourcing');
  await loadTenantData();
  checkTrialPrinterLock();

  if (onAuthenticatedRenderCallback) {
    onAuthenticatedRenderCallback();
  }
}

export async function handleLogout(): Promise<void> {
  const { error } = await sb.auth.signOut();
  if (error) {
    console.warn('Erro ao encerrar sessão:', error.message);
  }
  appState.session = null;
  appState.profile = null;
  appState.tenant = null;
  appState.clients = [];
  appState.printers = [];
  appState.closings = [];
  byId('appScreen')?.classList.add('hidden');
  byId('authScreen')?.classList.remove('hidden');
  switchAuthTab('login');
}

export function initAuthView(onReady: AppReadyCallback): void {
  onAuthenticatedRenderCallback = onReady;

  byId('tabBtnLogin')?.addEventListener('click', () => switchAuthTab('login'));
  byId('tabBtnSignup')?.addEventListener('click', () => switchAuthTab('signup'));
  byId('btnGoToForgotPassword')?.addEventListener('click', () => switchAuthTab('forgot'));
  byId('btnBackToLogin')?.addEventListener('click', () => switchAuthTab('login'));
  byId('btnGoogleLogin')?.addEventListener('click', () => {
    void (async () => {
      const redirectTo = window.location.origin + window.location.pathname;
      const { error } = await sb.auth.signInWithOAuth({
        provider: 'google',
        options: { redirectTo },
      });
      if (error) showAuthAlert('Erro no login Google: ' + error.message, true);
    })();
  });
  byId('btnLogout')?.addEventListener('click', () => void handleLogout());

  byId<HTMLFormElement>('formLogin')?.addEventListener('submit', e => {
    e.preventDefault();
    void (async () => {
      const email = byId<HTMLInputElement>('loginEmail')?.value.trim() ?? '';
      const password = byId<HTMLInputElement>('loginPassword')?.value ?? '';
      const { data, error } = await sb.auth.signInWithPassword({ email, password });
      if (error || !data.session) {
        showAuthAlert('E-mail ou senha inválidos: ' + (error?.message ?? 'verifique os dados.'), true);
        return;
      }
      await onSessionAuthenticated(data.session);
    })();
  });

  byId<HTMLFormElement>('formSignup')?.addEventListener('submit', e => {
    e.preventDefault();
    void (async () => {
      const tradeName = byId<HTMLInputElement>('signupTradeName')?.value.trim() ?? '';
      const cnpjCpf = byId<HTMLInputElement>('signupCnpj')?.value.trim() ?? '';
      const email = byId<HTMLInputElement>('signupEmail')?.value.trim() ?? '';
      const password = byId<HTMLInputElement>('signupPassword')?.value ?? '';

      if (password.length < 6) {
        showAuthAlert('A senha deve ter pelo menos 6 caracteres.', true);
        return;
      }
      const { data, error } = await sb.auth.signUp({
        email,
        password,
        options: {
          data: {
            trade_name: tradeName,
            cnpj_cpf: cnpjCpf,
          },
        },
      });
      if (error) {
        showAuthAlert('Erro no cadastro: ' + error.message, true);
        return;
      }
      if (data.session) {
        await onSessionAuthenticated(data.session);
      } else {
        showAuthAlert('Conta criada! Verifique seu e-mail para confirmar e entrar.', false);
      }
    })();
  });

  byId<HTMLFormElement>('formForgotPassword')?.addEventListener('submit', e => {
    e.preventDefault();
    void (async () => {
      const email = byId<HTMLInputElement>('forgotEmail')?.value.trim() ?? '';
      const redirectTo = window.location.origin + window.location.pathname;
      const { error } = await sb.auth.resetPasswordForEmail(email, { redirectTo });
      if (error) {
        showAuthAlert('Erro ao enviar recuperação: ' + error.message, true);
        return;
      }
      showAuthAlert('Link de redefinição enviado! Verifique sua caixa de entrada e spam.', false);
    })();
  });

  byId<HTMLFormElement>('formResetPassword')?.addEventListener('submit', e => {
    e.preventDefault();
    void (async () => {
      const newPassword = byId<HTMLInputElement>('resetNewPassword')?.value ?? '';
      const confirmPass = byId<HTMLInputElement>('resetConfirmPassword')?.value ?? '';
      if (newPassword.length < 6 || newPassword !== confirmPass) {
        showAuthAlert('As senhas precisam coincidir e ter pelo menos 6 caracteres.', true);
        return;
      }
      const { error } = await sb.auth.updateUser({ password: newPassword });
      if (error) {
        showAuthAlert('Erro ao atualizar senha: ' + error.message, true);
        return;
      }
      showAuthAlert('Senha atualizada com sucesso! Entrando...', false);
      const { data } = await sb.auth.getSession();
      if (data.session) await onSessionAuthenticated(data.session);
    })();
  });

  sb.auth.onAuthStateChange((event, session) => {
    if (event === 'PASSWORD_RECOVERY') {
      byId('appScreen')?.classList.add('hidden');
      byId('authScreen')?.classList.remove('hidden');
      switchAuthTab('reset');
      return;
    }
    if (event === 'SIGNED_IN' && session && !appState.session) {
      void onSessionAuthenticated(session).catch(err => {
        showAuthAlert(getErrorMessage(err, 'Erro ao autenticar sessão.'), true);
      });
    }
  });
}

export function getMasterAdminBadgeVisible(): boolean {
  return String(appState.session?.user?.email ?? '').toLowerCase().trim() === MASTER_ADMIN_EMAIL;
}
