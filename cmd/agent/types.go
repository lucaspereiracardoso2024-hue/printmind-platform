package main

const (
	AgentVersion      = "4.1.0"
	ServiceName       = "PrintMindAgent"
	ServiceDisplay    = "PrintMind Satellite Agent"
	ServiceDesc       = "Agente autonomo PrintMind de monitoramento de impressoras, telemetria e auditoria de impressao."
	DefaultConfigName = "config.json"
	LegacyConfigName  = "printmind-config.json"
	DefaultDBName     = "telemetry.db"
	DefaultLogRelPath = "logs/agent.log"
	DefaultSupabaseRP = "https://zbsmzcbcivhzslcnfxmq.supabase.co/rest/v1/rpc/sync_agent_telemetry"
	DefaultAnonAPIKey = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJzdXBhYmFzZSIsInJlZiI6Inpic216Y2JjaXZoenNsY25meG1xIiwicm9sZSI6ImFub24iLCJpYXQiOjE3OTQ0NDUxNTcsImV4cCI6MjExMDAyMTE1N30.U9F-EL3ddBBqEK0MqkNuZBC6ZvwtuEElN6Z_VpEGT7Q"
)

// SNMPv3Config holds optional SNMPv3 USM authentication and privacy credentials.
type SNMPv3Config struct {
	Enabled        bool   `json:"enabled"`
	Username       string `json:"username,omitempty"`
	SecurityLevel  string `json:"security_level,omitempty"` // "noAuthNoPriv" | "authNoPriv" | "authPriv"
	AuthProtocol   string `json:"auth_protocol,omitempty"`  // "MD5" | "SHA1" | "SHA256"
	AuthPassphrase string `json:"auth_passphrase,omitempty"`
	PrivProtocol   string `json:"priv_protocol,omitempty"` // "DES" | "AES"
	PrivPassphrase string `json:"priv_passphrase,omitempty"`
}

// Config holds the persistent local agent configuration.
type Config struct {
	ServerURL         string       `json:"server_url"`
	APIKey            string       `json:"api_key"`
	PairingToken      string       `json:"pairing_token"`
	PollIntervalSec   int          `json:"poll_interval_sec"`
	EnableUSB         bool         `json:"enable_usb"`
	Subnets           []string     `json:"subnets,omitempty"`
	StaticIPs         []string     `json:"static_ips,omitempty"`
	SNMPCommunities   []string     `json:"snmp_communities,omitempty"`
	SNMPv3            SNMPv3Config `json:"snmp_v3,omitempty"`
	DisableAutoUpdate bool         `json:"disable_auto_update,omitempty"`
}

// OSPaths resolves the OS-appropriate paths for config, SQLite DB, logs, and staging.
type OSPaths struct {
	DataDir    string
	ConfigPath string
	DBPath     string
	LogPath    string
	StagingDir string
}

// SupplyItem represents a single consumable or waste receptacle following RFC 3805 semantics.
type SupplyItem struct {
	Name         string `json:"name"`
	Color        string `json:"color"`
	SupplyClass  string `json:"supply_class,omitempty"` // "consumed" | "waste"
	SupplyType   string `json:"supply_type,omitempty"`
	Level        int    `json:"level"`
	MaxCapacity  int    `json:"max_capacity"`
	Percent      *int   `json:"percent"`     // nil when Level is -2 (unknown) or -3 (some_remaining) or -1 (other)
	LevelState   string `json:"level_state"` // "exact" | "some_remaining" | "unknown" | "other"
	SerialNumber string `json:"serial_number,omitempty"`
	PartNumber   string `json:"part_number,omitempty"`
	WarrantyEnd  string `json:"warranty_end,omitempty"`
}

// DeviceCounters holds hardware odometer and job/maintenance counters.
type DeviceCounters struct {
	LifetimeEngineTotal uint32 `json:"lifetime_engine_total"`
	MonoTotal           uint32 `json:"mono_total"`
	ColorTotal          uint32 `json:"color_total"`
	DuplexSheets        uint32 `json:"duplex_sheets"`
	ScanTotal           uint32 `json:"scan_total"`
	LabelCount          uint32 `json:"label_count"`
	PrintLengthMM       uint32 `json:"print_length_mm,omitempty"`
	JamsTotal           uint32 `json:"jams_total,omitempty"`
	HeadCleanings       uint32 `json:"head_cleanings,omitempty"`
}

// PrinterAlert captures an active prtAlertTable entry or hardware error state.
type PrinterAlert struct {
	Code        int    `json:"code"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
}

// DiscoveredDevice describes a network or local printer collected during a cycle.
type DiscoveredDevice struct {
	DiscoverySource    string         `json:"discovery_source"`
	IPAddress          string         `json:"ip_address"`
	USBPort            string         `json:"usb_port,omitempty"`
	WSDPort            string         `json:"wsd_port,omitempty"`
	MACAddress         string         `json:"mac_address,omitempty"`
	SerialNumber       string         `json:"serial_number"`
	DeviceUUID         string         `json:"device_uuid,omitempty"`
	SysObjectID        string         `json:"sys_object_id,omitempty"`
	UptimeSeconds      uint32         `json:"uptime_seconds,omitempty"`
	VendorPEN          int            `json:"vendor_pen"`
	Vendor             string         `json:"vendor"`
	Model              string         `json:"model"`
	Status             string         `json:"status"`
	StatusReasons      []string       `json:"status_reasons,omitempty"`
	Alerts             []PrinterAlert `json:"alerts,omitempty"`
	IsColorCapable     bool           `json:"is_color_capable"`
	SupportedLanguages []string       `json:"supported_languages,omitempty"`
	OdometerSource     string         `json:"odometer_source,omitempty"`
	Counters           DeviceCounters `json:"counters"`
	Supplies           []SupplyItem   `json:"supplies"`
}

// PrintJobRecord represents an audited print job correlated across spooler/EventLog/CUPS.
type PrintJobRecord struct {
	JobID          int    `json:"job_id"`
	EventRecordID  int64  `json:"event_record_id,omitempty"`
	Timestamp      string `json:"timestamp"`
	PrinterName    string `json:"printer_name"`
	PortName       string `json:"port_name,omitempty"`
	PrinterIP      string `json:"printer_ip,omitempty"`
	UserName       string `json:"user_name"`
	MachineName    string `json:"machine_name"`
	Domain         string `json:"domain,omitempty"`
	Department     string `json:"department,omitempty"`
	DocumentName   string `json:"document_name,omitempty"`
	PagesPrinted   int    `json:"pages_printed"`
	Copies         int    `json:"copies"`
	EffectivePages int    `json:"effective_pages"`
	MonoPages      int    `json:"mono_pages"`
	ColorPages     int    `json:"color_pages"`
	RequestedColor bool   `json:"requested_color"`
	ColorGated     bool   `json:"color_gated"`
	Duplex         bool   `json:"duplex"`
	SizeBytes      int64  `json:"size_bytes,omitempty"`
	Source         string `json:"source"`
}

// JobSummary aggregates audited print volume by printer, user, machine, and domain/department.
type JobSummary struct {
	PrinterName   string `json:"printer_name"`
	PrinterIP     string `json:"printer_ip,omitempty"`
	UserName      string `json:"user_name"`
	MachineName   string `json:"machine_name"`
	Domain        string `json:"domain,omitempty"`
	Department    string `json:"department,omitempty"`
	JobCount      int    `json:"job_count"`
	TotalPages    int    `json:"total_pages"`
	MonoPages     int    `json:"mono_pages"`
	ColorPages    int    `json:"color_pages"`
	DuplexPages   int    `json:"duplex_pages"`
	SimplexPages  int    `json:"simplex_pages"`
	ColorGatedOut int    `json:"color_gated_out_pages,omitempty"`
	IsDelta       bool   `json:"is_delta"`
}

// WPPReadiness captures Windows 11 24H2 / Server 2025 Protected Print Mode policy state.
type WPPReadiness struct {
	Supported        bool     `json:"supported"`
	WPPEnabled       bool     `json:"wpp_enabled"`
	WPPPolicyState   uint32   `json:"wpp_policy_state"`
	ImpactedPrinters []string `json:"impacted_printers,omitempty"`
}

// SyncRequest is the telemetry payload transmitted to the PrintMind cloud or Supabase RPC.
type SyncRequest struct {
	AgentVersion string             `json:"agent_version"`
	PairingToken string             `json:"pairing_token"`
	UnitID       string             `json:"unit_id,omitempty"`
	CollectedAt  string             `json:"collected_at"`
	HostInfo     map[string]any     `json:"host_info"`
	Devices      []DiscoveredDevice `json:"devices"`
	JobsSummary  []JobSummary       `json:"jobs_summary,omitempty"`
	PrintJobs    []PrintJobRecord   `json:"print_jobs,omitempty"`
	WPPReadiness *WPPReadiness      `json:"wpp_readiness,omitempty"`
}

// SupabaseRPCPayload wraps SyncRequest for the Supabase sync_agent_telemetry RPC.
type SupabaseRPCPayload struct {
	PairingToken string      `json:"p_pairing_token"`
	Payload      SyncRequest `json:"p_payload"`
}

// RemoteConfig holds dynamic directives returned by the PrintMind backend.
type RemoteConfig struct {
	PollIntervalSeconds       int          `json:"poll_interval_seconds"`
	RateLimitPacketsPerSec    int          `json:"rate_limit_packets_per_sec"`
	ScanSubnets               []string     `json:"scan_subnets"`
	StaticIPs                 []string     `json:"static_ips"`
	SNMPCommunities           []string     `json:"snmp_communities,omitempty"`
	SNMPv3                    SNMPv3Config `json:"snmp_v3,omitempty"`
	EnableUSBSpooler          bool         `json:"enable_usb_spooler"`
	EnableHTTPWakeupForPantum bool         `json:"enable_http_wakeup_for_pantum"`
	DiagnosticRequested       bool         `json:"diagnostic_requested,omitempty"`
	DiagnosticUploadURL       string       `json:"diagnostic_upload_url,omitempty"`
	UpdateMSIURL              string       `json:"update_msi_url,omitempty"`
	UpdateSHA256              string       `json:"update_sha256,omitempty"`
	UpdateLinuxURL            string       `json:"update_linux_url,omitempty"`
	UpdateLinuxSHA256         string       `json:"update_linux_sha256,omitempty"`
	UpdateVersion             string       `json:"update_version,omitempty"`
}

// SyncResponse is the response returned by the PrintMind server or Supabase RPC.
type SyncResponse struct {
	Status       string       `json:"status"`
	Message      string       `json:"message,omitempty"`
	SyncedCount  int          `json:"synced_devices,omitempty"`
	RemoteConfig RemoteConfig `json:"remote_config"`
}

// IntPtr returns a pointer to the supplied integer value.
func IntPtr(v int) *int {
	return &v
}
