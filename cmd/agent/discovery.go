package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// LANDiscoveryHint stores passive/active LAN discovery metadata for an IP before protocol probing.
type LANDiscoveryHint struct {
	IPAddress   string
	MACAddress  string
	ModelHint   string
	VendorHint  string
	IPPPort     int
	IPPPath     string
	HasIPPS     bool
	ColorHint   bool
	UUIDHint    string
	DiscoveryBy string
}

func isPrivateIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
		return false
	}
	switch {
	case ip4[0] == 10:
		return true
	case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
		return true
	case ip4[0] == 192 && ip4[1] == 168:
		return true
	default:
		return false
	}
}

func detectLocalSubnets() []string {
	seen := make(map[string]bool)
	var subnets []string

	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if (iface.Flags&net.FlagUp) == 0 || (iface.Flags&net.FlagLoopback) != 0 {
				continue
			}
			addrs, aErr := iface.Addrs()
			if aErr != nil {
				continue
			}
			for _, addr := range addrs {
				ipNet, ok := addr.(*net.IPNet)
				if !ok || !isPrivateIPv4(ipNet.IP) {
					continue
				}
				ip4 := ipNet.IP.To4()
				cidr := fmt.Sprintf("%d.%d.%d.0/24", ip4[0], ip4[1], ip4[2])
				if !seen[cidr] {
					seen[cidr] = true
					subnets = append(subnets, cidr)
				}
			}
		}
	}

	if len(subnets) == 0 {
		subnets = append(subnets, "192.168.1.0/24")
	}
	return subnets
}

func expandCIDRToIPs(cidr string) []string {
	if !strings.Contains(cidr, "/") {
		return []string{cidr}
	}
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	var all []string
	for cur := ip.Mask(ipnet.Mask); ipnet.Contains(cur); {
		all = append(all, cur.String())
		for i := len(cur) - 1; i >= 0; i-- {
			cur[i]++
			if cur[i] > 0 {
				break
			}
		}
	}
	ones, bits := ipnet.Mask.Size()
	if bits == 32 && ones < 31 && len(all) > 2 {
		return all[1 : len(all)-1]
	}
	return all
}

// --- mDNS / DNS-SD Discovery ---

func encodeDNSName(name string) []byte {
	var out []byte
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if label == "" {
			continue
		}
		if len(label) > 63 {
			label = label[:63]
		}
		out = append(out, byte(len(label)))
		out = append(out, []byte(label)...)
	}
	return append(out, 0x00)
}

func decodeDNSName(pkt []byte, offset int) (string, int, error) {
	var labels []string
	cur := offset
	jumped := false
	nextOffset := offset
	for steps := 0; steps < 64; steps++ {
		if cur >= len(pkt) {
			return "", 0, fmt.Errorf("dns name overflow")
		}
		l := int(pkt[cur])
		if l == 0 {
			if !jumped {
				nextOffset = cur + 1
			}
			return strings.Join(labels, "."), nextOffset, nil
		}
		if l&0xC0 == 0xC0 {
			if cur+1 >= len(pkt) {
				return "", 0, fmt.Errorf("truncated dns pointer")
			}
			ptr := int(l&0x3F)<<8 | int(pkt[cur+1])
			if !jumped {
				nextOffset = cur + 2
			}
			cur = ptr
			jumped = true
			continue
		}
		cur++
		if cur+l > len(pkt) {
			return "", 0, fmt.Errorf("truncated dns label")
		}
		labels = append(labels, string(pkt[cur:cur+l]))
		cur += l
	}
	return "", 0, fmt.Errorf("dns pointer loop")
}

// buildMDNSQuery builds a standard DNS-SD PTR query packet for multiple service types.
func buildMDNSQuery(services []string) []byte {
	var pkt []byte
	hdr := make([]byte, 12)
	binary.BigEndian.PutUint16(hdr[4:6], uint16(len(services))) // QDCOUNT
	pkt = append(pkt, hdr...)
	for _, svc := range services {
		pkt = append(pkt, encodeDNSName(svc)...)
		qTail := []byte{0x00, 0x0C, 0x00, 0x01} // QTYPE=PTR(12), QCLASS=IN(1)
		pkt = append(pkt, qTail...)
	}
	return pkt
}

// parseMDNSResponse parses A, SRV, PTR, and TXT records from an mDNS response packet.
func parseMDNSResponse(pkt []byte) []LANDiscoveryHint {
	if len(pkt) < 12 {
		return nil
	}
	qdCount := int(binary.BigEndian.Uint16(pkt[4:6]))
	anCount := int(binary.BigEndian.Uint16(pkt[6:8]))
	nsCount := int(binary.BigEndian.Uint16(pkt[8:10]))
	arCount := int(binary.BigEndian.Uint16(pkt[10:12]))

	off := 12
	for i := 0; i < qdCount; i++ {
		_, next, err := decodeDNSName(pkt, off)
		if err != nil || next+4 > len(pkt) {
			return nil
		}
		off = next + 4
	}

	type srvInfo struct {
		target string
		port   int
	}
	targetIPs := make(map[string]string)
	srvByName := make(map[string]srvInfo)
	txtByName := make(map[string]map[string]string)
	var serviceInstances []string

	totalRR := anCount + nsCount + arCount
	for i := 0; i < totalRR && off < len(pkt); i++ {
		name, next, err := decodeDNSName(pkt, off)
		if err != nil || next+10 > len(pkt) {
			break
		}
		rrType := binary.BigEndian.Uint16(pkt[next : next+2])
		rdLen := int(binary.BigEndian.Uint16(pkt[next+8 : next+10]))
		rdataStart := next + 10
		if rdataStart+rdLen > len(pkt) {
			break
		}
		rdata := pkt[rdataStart : rdataStart+rdLen]
		off = rdataStart + rdLen

		lowerName := strings.ToLower(name)
		switch rrType {
		case 1: // A
			if len(rdata) == 4 {
				ip := net.IPv4(rdata[0], rdata[1], rdata[2], rdata[3]).String()
				targetIPs[lowerName] = ip
			}
		case 12: // PTR
			if ptrName, _, pErr := decodeDNSName(pkt, rdataStart); pErr == nil {
				serviceInstances = append(serviceInstances, ptrName)
			}
		case 33: // SRV
			if len(rdata) >= 6 {
				port := int(binary.BigEndian.Uint16(rdata[4:6]))
				if target, _, tErr := decodeDNSName(pkt, rdataStart+6); tErr == nil {
					srvByName[lowerName] = srvInfo{target: strings.ToLower(target), port: port}
					serviceInstances = append(serviceInstances, name)
				}
			}
		case 16: // TXT
			kv := make(map[string]string)
			pos := 0
			for pos < len(rdata) {
				l := int(rdata[pos])
				pos++
				if l == 0 || pos+l > len(rdata) {
					break
				}
				entry := string(rdata[pos : pos+l])
				pos += l
				if eq := strings.IndexByte(entry, '='); eq != -1 {
					kv[strings.ToLower(entry[:eq])] = entry[eq+1:]
				}
			}
			txtByName[lowerName] = kv
		}
	}

	var hints []LANDiscoveryHint
	seenIP := make(map[string]bool)
	for _, inst := range serviceInstances {
		lInst := strings.ToLower(inst)
		srv, okSRV := srvByName[lInst]
		if !okSRV {
			continue
		}
		ip := targetIPs[srv.target]
		if ip == "" {
			continue
		}
		if seenIP[ip] {
			continue
		}
		seenIP[ip] = true

		txt := txtByName[lInst]
		model := txt["ty"]
		if model == "" {
			model = txt["product"]
		}
		if model == "" {
			model = strings.TrimSpace(txt["usb_mfg"] + " " + txt["usb_mdl"])
		}
		rp := strings.TrimPrefix(txt["rp"], "/")
		colorHint := strings.EqualFold(txt["color"], "T") || strings.EqualFold(txt["color"], "true")

		hints = append(hints, LANDiscoveryHint{
			IPAddress:   ip,
			ModelHint:   model,
			IPPPort:     srv.port,
			IPPPath:     rp,
			HasIPPS:     strings.Contains(lInst, "_ipps._tcp") || srv.port == 443,
			ColorHint:   colorHint,
			UUIDHint:    txt["uuid"],
			DiscoveryBy: "mdns",
		})
	}
	if len(hints) == 0 {
		for _, ip := range targetIPs {
			if !seenIP[ip] {
				seenIP[ip] = true
				hints = append(hints, LANDiscoveryHint{IPAddress: ip, DiscoveryBy: "mdns"})
			}
		}
	}
	return hints
}

func discoverMDNS(ctx context.Context, wait time.Duration) []LANDiscoveryHint {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil
	}
	defer conn.Close()

	services := []string{"_ipp._tcp.local", "_ipps._tcp.local", "_printer._tcp.local"}
	query := buildMDNSQuery(services)
	dst := &net.UDPAddr{IP: net.ParseIP("224.0.0.251"), Port: 5353}
	_, _ = conn.WriteToUDP(query, dst)

	deadline := time.Now().Add(wait)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetReadDeadline(deadline)

	var all []LANDiscoveryHint
	seen := make(map[string]bool)
	buf := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			break
		}
		n, src, rErr := conn.ReadFromUDP(buf)
		if rErr != nil {
			break
		}
		parsed := parseMDNSResponse(buf[:n])
		if len(parsed) == 0 && src != nil && isPrivateIPv4(src.IP) {
			parsed = []LANDiscoveryHint{{IPAddress: src.IP.String(), DiscoveryBy: "mdns"}}
		}
		for _, h := range parsed {
			if !seen[h.IPAddress] {
				seen[h.IPAddress] = true
				all = append(all, h)
			}
		}
	}
	return all
}

// --- WS-Discovery (UDP 3702) ---

func buildWSDProbe(msgID string) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<soap:Envelope xmlns:soap="http://www.w3.org/2003/05/soap-envelope" xmlns:wsa="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:wsd="http://schemas.xmlsoap.org/ws/2005/04/discovery" xmlns:wsdp="http://schemas.xmlsoap.org/ws/2006/02/devprof">
  <soap:Header>
    <wsa:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</wsa:To>
    <wsa:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</wsa:Action>
    <wsa:MessageID>urn:uuid:%s</wsa:MessageID>
  </soap:Header>
  <soap:Body>
    <wsd:Probe>
      <wsd:Types>wsdp:Device</wsd:Types>
    </wsd:Probe>
  </soap:Body>
</soap:Envelope>`, msgID))
}

// parseWSDProbeMatch extracts device IP addresses from a WS-Discovery ProbeMatches SOAP response.
func parseWSDProbeMatch(xmlBytes []byte) []LANDiscoveryHint {
	decoder := xml.NewDecoder(bytes.NewReader(xmlBytes))
	var xaddrs string
	var types string
	var inXAddrs, inTypes bool
	for {
		tok, err := decoder.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			local := strings.ToLower(t.Name.Local)
			if local == "xaddrs" {
				inXAddrs = true
			} else if local == "types" {
				inTypes = true
			}
		case xml.EndElement:
			inXAddrs = false
			inTypes = false
		case xml.CharData:
			if inXAddrs {
				xaddrs += " " + string(t)
			} else if inTypes {
				types += " " + string(t)
			}
		}
	}

	lowerTypes := strings.ToLower(types)
	if types != "" && !strings.Contains(lowerTypes, "print") && !strings.Contains(lowerTypes, "device") {
		return nil
	}

	var hints []LANDiscoveryHint
	seen := make(map[string]bool)
	for _, field := range strings.Fields(xaddrs) {
		u, err := url.Parse(field)
		if err != nil {
			continue
		}
		host := u.Hostname()
		if ip := net.ParseIP(host); ip != nil && ip.To4() != nil && !seen[host] {
			seen[host] = true
			hints = append(hints, LANDiscoveryHint{
				IPAddress:   host,
				DiscoveryBy: "wsd",
			})
		}
	}
	return hints
}

func discoverWSD(ctx context.Context, wait time.Duration) []LANDiscoveryHint {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil
	}
	defer conn.Close()

	probe := buildWSDProbe(fmt.Sprintf("%d", time.Now().UnixNano()))
	dst := &net.UDPAddr{IP: net.ParseIP("239.255.255.250"), Port: 3702}
	_, _ = conn.WriteToUDP(probe, dst)

	deadline := time.Now().Add(wait)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetReadDeadline(deadline)

	var all []LANDiscoveryHint
	seen := make(map[string]bool)
	buf := make([]byte, 8192)
	for {
		if ctx.Err() != nil {
			break
		}
		n, src, rErr := conn.ReadFromUDP(buf)
		if rErr != nil {
			break
		}
		parsed := parseWSDProbeMatch(buf[:n])
		if len(parsed) == 0 && src != nil && isPrivateIPv4(src.IP) && bytes.Contains(bytes.ToLower(buf[:n]), []byte("print")) {
			parsed = []LANDiscoveryHint{{IPAddress: src.IP.String(), DiscoveryBy: "wsd"}}
		}
		for _, h := range parsed {
			if !seen[h.IPAddress] {
				seen[h.IPAddress] = true
				all = append(all, h)
			}
		}
	}
	return all
}

// --- Epson ENPC Strict Query-Only (UDP 3289) ---

// buildENPCQueryPacket constructs strictly read-only EPSONQ discovery packets.
// EPSONC command packets are forbidden to prevent altering WHO_IS_HOLDING IP state.
func buildENPCQueryPacket(queryType byte) ([]byte, error) {
	pkt := make([]byte, 14)
	copy(pkt[:6], "EPSONQ")
	switch queryType {
	case 0x00:
		// "EPSONQ" + 00 00 00 10 00 00 00 00 -> network info (MAC/IP)
		pkt[6] = 0x00
		pkt[9] = 0x10
	case 0x03:
		// "EPSONQ" + 03 00 00 00 00 00 00 00 -> device info (model)
		pkt[6] = 0x03
	default:
		return nil, fmt.Errorf("unsupported ENPC query type 0x%02X", queryType)
	}
	if !bytes.HasPrefix(pkt, []byte("EPSONQ")) || bytes.HasPrefix(pkt, []byte("EPSONC")) {
		return nil, fmt.Errorf("ENPC safety guard: only EPSONQ packets are allowed")
	}
	return pkt, nil
}

// parseENPCResponse parses EPSONQ network (37B+) and device-info (147B+) responses.
func parseENPCResponse(data []byte) (LANDiscoveryHint, bool) {
	if len(data) < 37 {
		return LANDiscoveryHint{}, false
	}
	// Query 0x00 response: MAC at bytes 16..21, IP at bytes 24..27
	if bytes.HasPrefix(data, []byte("EPSONQ")) && data[6] == 0x00 && len(data) >= 37 {
		mac := formatMAC(data[16:22])
		ip := net.IPv4(data[24], data[25], data[26], data[27]).String()
		if ip == "0.0.0.0" {
			return LANDiscoveryHint{}, false
		}
		return LANDiscoveryHint{
			IPAddress:   ip,
			MACAddress:  mac,
			VendorHint:  "Epson",
			DiscoveryBy: "enpc_query",
		}, true
	}
	// Query 0x03 response: model starting at byte 20
	if bytes.HasPrefix(data, []byte("EPSONQ")) && data[6] == 0x03 && len(data) >= 147 {
		rawModel := data[20:147]
		if nul := bytes.IndexByte(rawModel, 0); nul >= 0 {
			rawModel = rawModel[:nul]
		}
		model := strings.TrimSpace(string(rawModel))
		if model == "" {
			return LANDiscoveryHint{}, false
		}
		return LANDiscoveryHint{
			ModelHint:   model,
			VendorHint:  "Epson",
			DiscoveryBy: "enpc_query",
		}, true
	}
	return LANDiscoveryHint{}, false
}

func discoverEpsonENPC(ctx context.Context, wait time.Duration) []LANDiscoveryHint {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil
	}
	defer conn.Close()

	qNet, err1 := buildENPCQueryPacket(0x00)
	qDev, err2 := buildENPCQueryPacket(0x03)
	if err1 != nil || err2 != nil {
		return nil
	}

	bcast := &net.UDPAddr{IP: net.IPv4bcast, Port: 3289}
	_, _ = conn.WriteToUDP(qNet, bcast)
	_, _ = conn.WriteToUDP(qDev, bcast)

	deadline := time.Now().Add(wait)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetReadDeadline(deadline)

	byIP := make(map[string]LANDiscoveryHint)
	buf := make([]byte, 2048)
	for {
		if ctx.Err() != nil {
			break
		}
		n, src, rErr := conn.ReadFromUDP(buf)
		if rErr != nil {
			break
		}
		hint, ok := parseENPCResponse(buf[:n])
		if !ok {
			continue
		}
		if hint.IPAddress == "" && src != nil {
			hint.IPAddress = src.IP.String()
		}
		if hint.IPAddress == "" {
			continue
		}
		existing := byIP[hint.IPAddress]
		existing.IPAddress = hint.IPAddress
		existing.VendorHint = "Epson"
		existing.DiscoveryBy = "enpc_query"
		if hint.MACAddress != "" {
			existing.MACAddress = hint.MACAddress
		}
		if hint.ModelHint != "" {
			existing.ModelHint = hint.ModelHint
		}
		byIP[hint.IPAddress] = existing
	}

	out := make([]LANDiscoveryHint, 0, len(byIP))
	for _, h := range byIP {
		out = append(out, h)
	}
	return out
}

// discoverLANHints runs passive mDNS, WS-Discovery, and ENPC query-only broadcasts concurrently.
func discoverLANHints(ctx context.Context) map[string]LANDiscoveryHint {
	hints := make(map[string]LANDiscoveryHint)
	merge := func(list []LANDiscoveryHint) {
		for _, h := range list {
			if h.IPAddress == "" {
				continue
			}
			cur := hints[h.IPAddress]
			cur.IPAddress = h.IPAddress
			if cur.MACAddress == "" {
				cur.MACAddress = h.MACAddress
			}
			if cur.ModelHint == "" {
				cur.ModelHint = h.ModelHint
			}
			if cur.VendorHint == "" {
				cur.VendorHint = h.VendorHint
			}
			if cur.IPPPort == 0 {
				cur.IPPPort = h.IPPPort
			}
			if cur.IPPPath == "" {
				cur.IPPPath = h.IPPPath
			}
			cur.HasIPPS = cur.HasIPPS || h.HasIPPS
			cur.ColorHint = cur.ColorHint || h.ColorHint
			if cur.UUIDHint == "" {
				cur.UUIDHint = h.UUIDHint
			}
			if cur.DiscoveryBy == "" {
				cur.DiscoveryBy = h.DiscoveryBy
			}
			hints[h.IPAddress] = cur
		}
	}

	ch := make(chan []LANDiscoveryHint, 3)
	go func() { ch <- discoverMDNS(ctx, 1100*time.Millisecond) }()
	go func() { ch <- discoverWSD(ctx, 1100*time.Millisecond) }()
	go func() { ch <- discoverEpsonENPC(ctx, 900*time.Millisecond) }()

	for i := 0; i < 3; i++ {
		merge(<-ch)
	}
	return hints
}
