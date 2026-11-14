import { TelemetryReading } from '../types/models';

export interface DailySeriesPoint {
  dateKey: string;
  label: string;
  mono: number;
  color: number;
  total: number;
}

export function computeDailyProductionSeries(
  telemetryReadings: TelemetryReading[],
  daysCount = 14,
  filterPrinterIds: Set<string> | null = null,
): DailySeriesPoint[] {
  const today = new Date();
  const days: Array<{ key: string; label: string }> = [];

  for (let i = daysCount - 1; i >= 0; i--) {
    const d = new Date(today.getFullYear(), today.getMonth(), today.getDate() - i);
    const yyyy = d.getFullYear();
    const mm = String(d.getMonth() + 1).padStart(2, '0');
    const dd = String(d.getDate()).padStart(2, '0');
    days.push({ key: `${yyyy}-${mm}-${dd}`, label: `${dd}/${mm}` });
  }

  const byPrinter: Record<string, TelemetryReading[]> = {};
  for (const r of telemetryReadings) {
    if (!r.printer_id || !r.recorded_at) continue;
    if (filterPrinterIds && !filterPrinterIds.has(r.printer_id)) continue;
    if (!byPrinter[r.printer_id]) byPrinter[r.printer_id] = [];
    byPrinter[r.printer_id].push(r);
  }

  const dailyTotals: Record<string, { mono: number; color: number }> = {};
  for (const d of days) {
    dailyTotals[d.key] = { mono: 0, color: 0 };
  }

  for (const pid of Object.keys(byPrinter)) {
    const list = byPrinter[pid]
      .slice()
      .sort((a, b) => new Date(a.recorded_at).getTime() - new Date(b.recorded_at).getTime());

    const dayLast: Record<string, { mono: number; color: number; total: number }> = {};
    for (const r of list) {
      const dateKey = String(r.recorded_at).slice(0, 10);
      dayLast[dateKey] = {
        mono: Number(r.mono_pages ?? 0),
        color: Number(r.color_pages ?? 0),
        total: Number(r.total_pages ?? 0),
      };
    }

    const sortedDates = Object.keys(dayLast).sort();
    for (let idx = 0; idx < sortedDates.length; idx++) {
      const curKey = sortedDates[idx];
      if (!dailyTotals[curKey]) continue;
      if (idx === 0) continue;
      const prev = dayLast[sortedDates[idx - 1]];
      const cur = dayLast[curKey];
      const dMono = Math.max(0, cur.mono - prev.mono);
      const dColor = Math.max(0, cur.color - prev.color);
      const dTotal = Math.max(0, cur.total - prev.total);
      if (dMono + dColor > 0) {
        dailyTotals[curKey].mono += dMono;
        dailyTotals[curKey].color += dColor;
      } else if (dTotal > 0) {
        dailyTotals[curKey].mono += dTotal;
      }
    }
  }

  return days.map(d => {
    const bucket = dailyTotals[d.key];
    return {
      dateKey: d.key,
      label: d.label,
      mono: bucket.mono,
      color: bucket.color,
      total: bucket.mono + bucket.color,
    };
  });
}
