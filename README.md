# 📚 PrintMind V3.0 • Plataforma SaaS de Telemetria SNMP/USB, Outsourcing de Impressão (MPS) e FinTech B2B2C

Repositório oficial de produção do **PrintMind V3.0**, integrando:
- **Backend Cloud & Servidor Local (`main.go`)**: Servidor em Go com decodificador SNMPv2c ASN.1 BER completo (suporte a contadores de 1 a 8 bytes e *Long-Form Length*), leitura real de consumíveis CMYK ([Printer-MIB RFC 3805](https://mibbrowser.online/mibdb_search.php?mib=Printer-MIB)) e OIDs privados de 14 marcas, cálculo de Delta Mensal auditado, rateio ponderado P&B vs. Colorido por Centro de Custo e gerador EMVCo PIX com CRC16-CCITT (`0x1021`).
- **Micro-Coletor Satélite Multi-Local (`cmd/agent/main.go`)**: Binário estático em Go puro (`CGO_ENABLED=0`) que varre a sub-rede local, múltiplas VLANs/sub-redes roteadas em hospitais (`scan_subnets`) com *rate-limiting* de 20 IPs/s ([GoSNMP](https://github.com/gosnmp/gosnmp)), *wake-up* HTTP para impressoras Pantum e portas USB locais, mantendo buffer resiliente offline.
- **Banco de Dados & Autenticação (`Supabase`)**: Modelagem Multi-Tenant de 13 tabelas com Row-Level Security (RLS) estrito, login via E-mail/Senha e [Google OAuth 2.0](https://developers.google.com/identity/protocols/oauth2/web-server), trigger automático de onboarding e stored procedure `sync_agent_telemetry`.
- **Servidor WhatsApp Open-Source (`docker-compose.yml`)**: Orquestração pronta com [Evolution API](https://github.com/evolution-foundation/evolution-api) + Redis + PostgreSQL para disparo gratuito de faturas, chave PIX e PDF do Espelho Formal A4.
- **Motor FinTech em Dupla Camada (`Asaas / Efí Pay`)**: Cobrança de assinatura recorrente SaaS das locadoras e intermediação B2B2C com criação de Subcontas White-Label e [Split de Pagamento](https://docs.asaas.com/docs/split-de-pagamentos) automático na liquidação de cada boleto/PIX pago pelos hospitais/clientes.

## 🚀 Como Executar

### 1. Subir Stack Completa em Produção (Servidor + WhatsApp Evolution API + Redis)
```bash
docker compose up -d --build
```

### 2. Compilar e Rodar Localmente
```bash
# Servidor Principal (Porta 8090)
CGO_ENABLED=0 go build -ldflags="-s -w" -o printmind-server main.go
./printmind-server

# Agente Satélite Multi-Local (Windows / Linux)
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o printmind-agent.exe ./cmd/agent/main.go
./printmind-agent.exe -server="http://localhost:8090" -token="pm_live_hosp_central_01" -subnets="192.168.1.0/24,10.20.1.0/24"
```
