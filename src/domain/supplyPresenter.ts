import { MeasurementMode, Printer, SupplyItem, SupplyState } from '../types/models';
import { escapeHtml, formatElapsedSince } from '../utils/format';

export interface NormalizedSupply {
  rawName: string;
  displayName: string;
  colorKey: 'K' | 'C' | 'M' | 'Y' | null;
  supplyClass: string;
  isWaste: boolean;
  isPrinthead: boolean;
  measurementMode: MeasurementMode;
  supplyState: SupplyState;
  isStateOnly: boolean;
  hasKnownPct: boolean;
  pct: number | null;
  honestLabel: string;
  stateBadgeText: string;
  stateBadgeClass: string;
  barColor: string;
  isWasteAlert: boolean;
  isLowAlert: boolean;
  partNumber: string | null;
  serialNumber: string | null;
  manufactureDate: string | null;
  isGenuine: boolean | null;
}

export interface PrinterEffectiveStatus {
  isOffline: boolean;
  isError: boolean;
  stale: boolean;
  elapsedLabel: string;
  label: string;
  badgeClass: string;
  isRecentlyActiveZeroPages: boolean;
}

export function detectSupplyColorKey(nameOrMsg: string): 'K' | 'C' | 'M' | 'Y' {
  const txt = String(nameOrMsg || '').toLowerCase();
  if (/ciano|cyan|\bc\b/.test(txt)) return 'C';
  if (/magenta|\bm\b/.test(txt)) return 'M';
  if (/amarelo|yellow|\by\b/.test(txt)) return 'Y';
  return 'K';
}

function detectOptionalColorKey(rawColor: string, rawName: string): 'K' | 'C' | 'M' | 'Y' | null {
  const c = rawColor.toLowerCase().trim();
  const n = rawName.toLowerCase();
  if (c === 'k' || c.includes('black') || c.includes('preto') || /preto|black|\bk\b/.test(n)) return 'K';
  if (c === 'c' || c.includes('cyan') || c.includes('ciano') || /ciano|cyan/.test(n)) return 'C';
  if (c === 'm' || c.includes('magenta') || /magenta/.test(n)) return 'M';
  if (c === 'y' || c.includes('yellow') || c.includes('amarelo') || /amarelo|yellow/.test(n)) return 'Y';
  return null;
}

function normalizeSupplyState(rawState: unknown, levelState: string, rawLevel: number | null): SupplyState {
  const s = String(rawState ?? '').toLowerCase().trim();
  if (s === 'ok' || s === 'low' || s === 'empty' || s === 'unknown') {
    return s;
  }
  if (levelState === 'empty' || levelState === 'out') return 'empty';
  if (levelState === 'low' || levelState === 'very_low') return 'low';
  if (levelState === 'ok' || levelState === 'present' || levelState === 'some_remaining' || rawLevel === -3) return 'ok';
  return 'unknown';
}

export function normalizeSupplyItem(s: SupplyItem): NormalizedSupply {
  const rawName = String(s.name ?? 'Consumível').trim();
  const rawColor = String(s.color ?? '').toLowerCase();
  const supplyClass = String(s.supply_class ?? s.type ?? '').toLowerCase().trim();
  const levelState = String(s.level_state ?? '').toLowerCase().trim();
  const rawMode = String(s.measurement_mode ?? s.MeasurementMode ?? '').toLowerCase().trim();
  const rawLevel = s.level !== undefined && s.level !== null ? Number(s.level) : null;

  const isPrinthead = supplyClass === 'printhead' || /printhead|cabe[cç]ote/i.test(rawName);
  const isWaste =
    !isPrinthead &&
    (supplyClass === 'waste' || /waste|res[ií]duo|manuten[cç][aã]o|maintenance/i.test(rawName));

  const partNumber = (s.part_number ?? s.PartNumber ?? '').trim() || null;
  const serialNumber = (s.serial_number ?? s.SerialNumber ?? '').trim() || null;
  const manufactureDate = (s.manufacture_date ?? s.ManufactureDate ?? '').trim() || null;
  const isGenuine = s.is_genuine !== undefined ? s.is_genuine : s.IsGenuine !== undefined ? s.IsGenuine : null;

  const measurementMode: MeasurementMode =
    rawMode === 'state_only' || levelState === 'state_only'
      ? 'state_only'
      : rawMode === 'unknown' || levelState === 'unknown' || rawLevel === -2 || rawLevel === -1
        ? 'unknown'
        : rawMode === 'exact'
          ? 'exact'
          : 'exact';

  const supplyState = normalizeSupplyState(s.supply_state ?? s.SupplyState, levelState, rawLevel);
  const isStateOnly = measurementMode === 'state_only';

  const rawPct = s.percent;
  const numericPct =
    rawPct !== null && rawPct !== undefined && Number.isFinite(Number(rawPct)) ? Number(rawPct) : null;

  const hasKnownPct =
    !isStateOnly &&
    !isPrinthead &&
    measurementMode !== 'unknown' &&
    levelState !== 'unknown' &&
    numericPct !== null &&
    numericPct >= 0 &&
    numericPct <= 100 &&
    rawLevel !== -1 &&
    rawLevel !== -2 &&
    rawLevel !== -3;

  const pct = hasKnownPct && numericPct !== null ? Math.round(numericPct) : null;

  let honestLabel = 'Nível não informado pela impressora';
  let stateBadgeText = 'Sem leitura %';
  let stateBadgeClass = 'bg-slate-100 text-slate-600 border border-slate-200';

  if (isPrinthead) {
    honestLabel = partNumber ? `Cabeçote • ${partNumber}` : 'Cabeçote de impressão';
    stateBadgeText = supplyState === 'low' || supplyState === 'empty' ? 'Verificar cabeçote' : 'Instalado';
    stateBadgeClass =
      supplyState === 'low' || supplyState === 'empty'
        ? 'bg-amber-50 text-amber-800 border border-amber-200'
        : 'bg-indigo-50 text-indigo-800 border border-indigo-200';
  } else if (hasKnownPct && pct !== null) {
    honestLabel = `${pct}%`;
    stateBadgeText = `${pct}%`;
    stateBadgeClass = 'bg-emerald-50 text-emerald-700 border border-emerald-200';
  } else if (isStateOnly) {
    if (supplyState === 'empty') {
      honestLabel = 'Vazio (Tanque sem sensor %)';
      stateBadgeText = 'VAZIO (Tanque)';
      stateBadgeClass = 'bg-rose-100 text-rose-800 border border-rose-300';
    } else if (supplyState === 'low') {
      honestLabel = 'Baixo (Tanque sem sensor %)';
      stateBadgeText = 'BAIXO (Tanque)';
      stateBadgeClass = 'bg-amber-100 text-amber-900 border border-amber-300';
    } else if (supplyState === 'ok') {
      honestLabel = 'Nível OK (Tanque sem sensor %)';
      stateBadgeText = 'OK (Sem sensor %)';
      stateBadgeClass = 'bg-emerald-50 text-emerald-800 border border-emerald-200';
    } else {
      honestLabel = 'Tanque sem sensor contínuo de %';
      stateBadgeText = 'Sem sensor %';
      stateBadgeClass = 'bg-slate-100 text-slate-600 border border-slate-200';
    }
  } else if (rawLevel === -3 || levelState === 'some_remaining' || levelState === 'ok' || levelState === 'present') {
    honestLabel = 'Com tinta (sem % exato)';
    stateBadgeText = 'Com tinta (sem %)';
    stateBadgeClass = 'bg-blue-50 text-blue-800 border border-blue-200';
  }

  const displayName = isWaste
    ? /caixa|res[ií]duo|manuten/i.test(rawName)
      ? `Caixa de Manutenção / Resíduo (${rawName})`
      : 'Caixa de Manutenção / Resíduo'
    : rawName;

  const isWasteAlert = isWaste && ((hasKnownPct && pct !== null && pct >= 85) || supplyState === 'low' || supplyState === 'empty');
  const isLowAlert =
    !isWaste &&
    !isPrinthead &&
    ((hasKnownPct && pct !== null && pct <= 15) || supplyState === 'low' || supplyState === 'empty');

  const colorKey = detectOptionalColorKey(rawColor, rawName);

  let barColor = 'bg-blue-600';
  if (isWaste && pct !== null) {
    barColor = pct >= 85 ? 'bg-rose-600' : pct >= 65 ? 'bg-amber-500' : 'bg-slate-600';
  } else if (colorKey === 'K') {
    barColor = 'bg-slate-900';
  } else if (colorKey === 'C') {
    barColor = 'bg-cyan-500';
  } else if (colorKey === 'M') {
    barColor = 'bg-pink-500';
  } else if (colorKey === 'Y') {
    barColor = 'bg-amber-400';
  }

  return {
    rawName,
    displayName,
    colorKey,
    supplyClass,
    isWaste,
    isPrinthead,
    measurementMode,
    supplyState,
    isStateOnly,
    hasKnownPct,
    pct,
    honestLabel,
    stateBadgeText,
    stateBadgeClass,
    barColor,
    isWasteAlert,
    isLowAlert,
    partNumber,
    serialNumber,
    manufactureDate,
    isGenuine,
  };
}

export function getEffectivePrinterStatus(p: Printer, nowMs = Date.now()): PrinterEffectiveStatus {
  const rawStatus = String(p.status ?? '').toLowerCase().trim();
  const lastSeenMs = p.last_seen ? new Date(p.last_seen).getTime() : NaN;
  const diffMs = Number.isFinite(lastSeenMs) ? Math.max(0, nowMs - lastSeenMs) : Infinity;
  const stale = !p.last_seen || diffMs > 25 * 60 * 1000;
  const elapsedLabel = Number.isFinite(lastSeenMs) ? formatElapsedSince(diffMs) : 'sem registro';
  const isOffline = rawStatus === 'offline' || stale;
  const isError = !isOffline && (rawStatus === 'error' || (Array.isArray(p.error_flags) && p.error_flags.length > 0));
  const isMaintenance = !isOffline && rawStatus === 'maintenance';
  const isRecentlyActiveZeroPages = !isOffline && diffMs < 24 * 60 * 60 * 1000;

  if (isOffline) {
    return {
      isOffline: true,
      isError: false,
      stale,
      elapsedLabel,
      label: p.last_seen ? `Offline • Sem sinal há ${elapsedLabel}` : 'Offline • Sem sinal',
      badgeClass: 'bg-rose-50 text-rose-700 border border-rose-200',
      isRecentlyActiveZeroPages: false,
    };
  }
  if (isError) {
    return {
      isOffline: false,
      isError: true,
      stale: false,
      elapsedLabel,
      label: 'Alerta / Erro',
      badgeClass: 'bg-amber-50 text-amber-800 border border-amber-200',
      isRecentlyActiveZeroPages,
    };
  }
  if (isMaintenance) {
    return {
      isOffline: false,
      isError: false,
      stale: false,
      elapsedLabel,
      label: 'Manutenção',
      badgeClass: 'bg-slate-100 text-slate-700 border border-slate-200',
      isRecentlyActiveZeroPages,
    };
  }
  return {
    isOffline: false,
    isError: false,
    stale: false,
    elapsedLabel,
    label: 'Online',
    badgeClass: 'bg-emerald-50 text-emerald-700 border border-emerald-200',
    isRecentlyActiveZeroPages,
  };
}

export function renderSupplyListHtml(supplies: SupplyItem[] | null | undefined, compact = false): string {
  const list = Array.isArray(supplies) ? supplies : [];
  if (list.length === 0) {
    return `<div class="text-[11px] text-slate-400 italic py-1">Aguardando primeira telemetria de suprimentos do hardware.</div>`;
  }

  const normalized = list.map(normalizeSupplyItem);
  const consumables = normalized.filter(item => !item.isPrinthead);
  const printheads = normalized.filter(item => item.isPrinthead);
  const visibleConsumables = compact ? consumables.slice(0, 5) : consumables;

  const consumableHtml = visibleConsumables
    .map(item => {
      const textCls =
        item.isWasteAlert || item.isLowAlert ? 'text-rose-700 font-black' : 'text-slate-700 font-semibold';

      if (item.hasKnownPct && item.pct !== null) {
        return `
          <div class="space-y-1">
            <div class="flex justify-between items-center text-[11px] ${textCls}">
              <span class="truncate pr-2">${escapeHtml(item.displayName)}</span>
              <span class="mono whitespace-nowrap">${escapeHtml(item.honestLabel)}</span>
            </div>
            <div class="w-full h-1.5 bg-slate-100 rounded-full overflow-hidden">
              <div class="${item.barColor} h-full rounded-full" style="width:${item.pct}%"></div>
            </div>
          </div>`;
      }

      return `
        <div class="flex justify-between items-center text-[11px] py-0.5 gap-2">
          <span class="truncate text-slate-700 font-semibold">${escapeHtml(item.displayName)}</span>
          <span class="px-2 py-0.5 rounded-full text-[10px] font-bold whitespace-nowrap ${item.stateBadgeClass}">
            ${escapeHtml(item.honestLabel)}
          </span>
        </div>`;
    })
    .join('');

  const printheadHtml = printheads.length > 0
    ? `
      <div class="pt-1.5 mt-1.5 border-t border-slate-100 space-y-1">
        <span class="text-[10px] font-bold uppercase text-slate-400 block">Cabeçotes de Impressão</span>
        <div class="flex flex-wrap gap-1.5">
          ${printheads
            .slice(0, compact ? 2 : 6)
            .map(ph => {
              const parts: string[] = [ph.rawName];
              if (ph.partNumber) parts.push(`PN ${ph.partNumber}`);
              if (ph.serialNumber && !compact) parts.push(`S/N ${ph.serialNumber}`);
              if (ph.manufactureDate && !compact) parts.push(`Fab ${ph.manufactureDate}`);
              const genuineLabel = ph.isGenuine === true ? ' • Genuíno' : ph.isGenuine === false ? ' • Compatível' : '';
              return `<span class="px-2 py-0.5 rounded-lg text-[10px] font-semibold bg-indigo-50 text-indigo-800 border border-indigo-200">${escapeHtml(parts.join(' • ') + genuineLabel)}</span>`;
            })
            .join('')}
        </div>
      </div>`
    : '';

  return `<div class="space-y-1.5">${consumableHtml}${printheadHtml}</div>`;
}
