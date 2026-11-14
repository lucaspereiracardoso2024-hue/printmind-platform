export type PixKeyKind = 'cnpj' | 'cpf' | 'email' | 'phone' | 'evp' | 'invalid';

export interface SanitizedPixKeyResult {
  valid: boolean;
  key: string;
  kind: PixKeyKind;
  reason?: string;
}

export interface PixBuildInput {
  pixKey?: string | null;
  merchantName?: string | null;
  merchantCity?: string | null;
  amount: number;
  txId?: string;
}

export type PixBuildResult =
  | {
      ok: true;
      payload: string;
      sanitizedKey: string;
      merchantName: string;
      merchantCity: string;
    }
  | {
      ok: false;
      missingFields: Array<'pix_key' | 'city' | 'merchant_name' | 'amount'>;
      message: string;
    };

export function calcCRC16(payloadWithoutCrc: string): string {
  let crc = 0xffff;
  for (let i = 0; i < payloadWithoutCrc.length; i++) {
    crc ^= payloadWithoutCrc.charCodeAt(i) << 8;
    for (let j = 0; j < 8; j++) {
      crc = (crc & 0x8000) !== 0 ? (crc << 1) ^ 0x1021 : crc << 1;
      crc &= 0xffff;
    }
  }
  return crc.toString(16).toUpperCase().padStart(4, '0');
}

export function normalizeEmvText(input: string, maxLen: number): string {
  return String(input ?? '')
    .normalize('NFD')
    .replace(/[\u0300-\u036f]/g, '')
    .replace(/[^A-Za-z0-9 \-.]/g, ' ')
    .replace(/\s+/g, ' ')
    .trim()
    .toUpperCase()
    .slice(0, maxLen);
}

export function sanitizePixKey(rawKey: string | null | undefined): SanitizedPixKeyResult {
  const trimmed = String(rawKey ?? '').trim();
  if (!trimmed) {
    return { valid: false, key: '', kind: 'invalid', reason: 'Chave PIX não cadastrada.' };
  }

  // UUID EVP key
  if (/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(trimmed)) {
    return { valid: true, key: trimmed.toLowerCase(), kind: 'evp' };
  }

  // Email key
  if (/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(trimmed)) {
    return { valid: true, key: trimmed.toLowerCase(), kind: 'email' };
  }

  // International phone with +
  if (/^\+[1-9]\d{10,14}$/.test(trimmed.replace(/[\s\-()]/g, ''))) {
    const cleaned = '+' + trimmed.replace(/\D/g, '');
    return { valid: true, key: cleaned, kind: 'phone' };
  }

  // Numeric CNPJ/CPF (reject placeholder zeros / repeated digits / old 00.000.000/0001-00 placeholder)
  const digits = trimmed.replace(/\D/g, '');
  if (/^[\d.\-/()\s+]+$/.test(trimmed)) {
    if (/^(.)\1+$/.test(digits) || digits === '00000000000100') {
      return { valid: false, key: '', kind: 'invalid', reason: 'Chave PIX placeholder inválida.' };
    }
    if (digits.length === 14) {
      return { valid: true, key: digits, kind: 'cnpj' };
    }
    if (digits.length === 11) {
      if (trimmed.includes('(') || trimmed.startsWith('+')) {
        return { valid: true, key: '+55' + digits, kind: 'phone' };
      }
      return { valid: true, key: digits, kind: 'cpf' };
    }
    if (digits.length === 10 && (trimmed.includes('(') || trimmed.includes('-'))) {
      return { valid: true, key: '+55' + digits, kind: 'phone' };
    }
  }

  return {
    valid: false,
    key: '',
    kind: 'invalid',
    reason: 'Formato de chave PIX inválido (use CPF, CNPJ, e-mail, celular +55 ou chave aleatória EVP).',
  };
}

function tlv(id: string, value: string): string {
  return id + String(value.length).padStart(2, '0') + value;
}

export function buildPixPayloadStrict(input: PixBuildInput): PixBuildResult {
  const missingFields: Array<'pix_key' | 'city' | 'merchant_name' | 'amount'> = [];
  const keyCheck = sanitizePixKey(input.pixKey);
  if (!keyCheck.valid) {
    missingFields.push('pix_key');
  }

  const merchantCity = normalizeEmvText(String(input.merchantCity ?? ''), 15);
  if (merchantCity.length < 2) {
    missingFields.push('city');
  }

  const merchantName = normalizeEmvText(String(input.merchantName ?? ''), 25);
  if (merchantName.length < 2) {
    missingFields.push('merchant_name');
  }

  const amountValid = Number.isFinite(input.amount) && input.amount > 0;
  if (!amountValid) {
    missingFields.push('amount');
  }

  if (missingFields.length > 0) {
    const labelMap: Record<'pix_key' | 'city' | 'merchant_name' | 'amount', string> = {
      pix_key: 'Chave PIX válida',
      city: 'Cidade da empresa',
      merchant_name: 'Nome Fantasia',
      amount: 'Valor positivo da fatura',
    };
    return {
      ok: false,
      missingFields,
      message: `Complete em "Minha Empresa": ${missingFields.map(f => labelMap[f]).join(', ')} para gerar o QR Code / PIX Copia e Cola oficial.`,
    };
  }

  const cleanTxId = normalizeEmvText(input.txId ?? 'PRINTMIND', 25).replace(/\s+/g, '') || 'PRINTMIND';
  const gui = tlv('00', 'br.gov.bcb.pix') + tlv('01', keyCheck.key);
  const amountStr = input.amount.toFixed(2);
  const base =
    '000201' +
    '010212' +
    tlv('26', gui) +
    '52040000' +
    '5303986' +
    tlv('54', amountStr) +
    '5802BR' +
    tlv('59', merchantName) +
    tlv('60', merchantCity) +
    tlv('62', tlv('05', cleanTxId)) +
    '6304';

  const payload = base + calcCRC16(base);
  return {
    ok: true,
    payload,
    sanitizedKey: keyCheck.key,
    merchantName,
    merchantCity,
  };
}
