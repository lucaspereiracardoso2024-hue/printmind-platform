import QRCode from 'qrcode';

export async function renderQrDataUrl(text: string, width = 200): Promise<string> {
  return QRCode.toDataURL(text, {
    width,
    margin: 2,
    errorCorrectionLevel: 'M',
    color: {
      dark: '#0f172a',
      light: '#ffffff',
    },
  });
}
