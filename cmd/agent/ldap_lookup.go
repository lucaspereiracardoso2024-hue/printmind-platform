package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ldapDefaultTimeout = 1500 * time.Millisecond

	ldapTagBindRequest       byte = 0x60
	ldapTagBindResponse      byte = 0x61
	ldapTagUnbindRequest     byte = 0x42
	ldapTagSearchRequest     byte = 0x63
	ldapTagSearchResultEntry byte = 0x64
	ldapTagSearchResultDone  byte = 0x65

	ldapTagEnumerated byte = 0x0A
	ldapTagBoolean    byte = 0x01
	ldapTagSet        byte = 0x31
	ldapTagFilterAnd  byte = 0xA0
	ldapTagFilterEq   byte = 0xA3
)

type ldapCacheEntry struct {
	department string
	expiresAt  time.Time
}

// LDAPDepartmentResolver performs lightweight read-only AD/LDAP department lookups with strict timeout and caching.
type LDAPDepartmentResolver struct {
	mu        sync.RWMutex
	cache     map[string]ldapCacheEntry
	timeout   time.Duration
	lookupSRV func(ctx context.Context, domain string) ([]string, error)
}

var defaultLDAPResolver = NewLDAPDepartmentResolver()

// NewLDAPDepartmentResolver creates a cached, 1.5s-bounded read-only LDAP department resolver.
func NewLDAPDepartmentResolver() *LDAPDepartmentResolver {
	return &LDAPDepartmentResolver{
		cache:     make(map[string]ldapCacheEntry),
		timeout:   ldapDefaultTimeout,
		lookupSRV: defaultLookupLDAPSRV,
	}
}

func defaultLookupLDAPSRV(ctx context.Context, dnsDomain string) ([]string, error) {
	if strings.TrimSpace(dnsDomain) == "" {
		return nil, fmt.Errorf("empty dns domain")
	}
	r := &net.Resolver{}
	_, addrs, err := r.LookupSRV(ctx, "ldap", "tcp", dnsDomain)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("SRV _ldap._tcp.%s lookup failed: %w", dnsDomain, err)
	}
	hosts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		target := strings.TrimSuffix(strings.TrimSpace(a.Target), ".")
		if target == "" {
			continue
		}
		port := int(a.Port)
		if port <= 0 {
			port = 389
		}
		hosts = append(hosts, net.JoinHostPort(target, strconv.Itoa(port)))
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no valid LDAP SRV targets")
	}
	return hosts, nil
}

// escapeLDAPFilterValue escapes special characters per RFC 4515 Section 3.
func escapeLDAPFilterValue(val string) string {
	var b strings.Builder
	for i := 0; i < len(val); i++ {
		c := val[i]
		switch c {
		case '\\':
			b.WriteString(`\5c`)
		case '*':
			b.WriteString(`\2a`)
		case '(':
			b.WriteString(`\28`)
		case ')':
			b.WriteString(`\29`)
		case 0x00:
			b.WriteString(`\00`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// domainToBaseDN converts a DNS domain (e.g. "corp.empresa.local") to "dc=corp,dc=empresa,dc=local".
func domainToBaseDN(domain string) string {
	domain = strings.TrimSpace(strings.TrimSuffix(domain, "."))
	if domain == "" {
		return ""
	}
	parts := strings.Split(domain, ".")
	dcs := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			dcs = append(dcs, "dc="+p)
		}
	}
	return strings.Join(dcs, ",")
}

// extractOUFromDN parses an RFC 4514 Distinguished Name and returns the most specific OU value.
func extractOUFromDN(dn string) string {
	if strings.TrimSpace(dn) == "" {
		return ""
	}
	// Split on unescaped commas
	var rdns []string
	var cur strings.Builder
	escaped := false
	for i := 0; i < len(dn); i++ {
		ch := dn[i]
		if escaped {
			cur.WriteByte(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == ',' {
			rdns = append(rdns, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(ch)
	}
	if cur.Len() > 0 {
		rdns = append(rdns, strings.TrimSpace(cur.String()))
	}

	var fallbackOU string
	for _, rdn := range rdns {
		eq := strings.IndexByte(rdn, '=')
		if eq <= 0 {
			continue
		}
		key := strings.ToUpper(strings.TrimSpace(rdn[:eq]))
		val := strings.TrimSpace(rdn[eq+1:])
		if key == "OU" && val != "" {
			lower := strings.ToLower(val)
			if lower == "domain controllers" || lower == "users" || lower == "computers" {
				if fallbackOU == "" {
					fallbackOU = val
				}
				continue
			}
			return val
		}
	}
	return fallbackOU
}

func isNonADDomain(domain, machineName string) bool {
	d := strings.ToUpper(strings.TrimSpace(domain))
	if d == "" || d == "WORKGROUP" || d == "LOCAL" || d == "DEFAULT" || d == "NT AUTHORITY" || d == "BUILTIN" {
		return true
	}
	if machineName != "" && strings.EqualFold(d, strings.TrimSpace(machineName)) {
		return true
	}
	return false
}

func resolveLDAPSearchDomain(domain string) string {
	d := strings.TrimSpace(domain)
	if strings.Contains(d, ".") {
		return d
	}
	if dnsDom := strings.TrimSpace(os.Getenv("USERDNSDOMAIN")); dnsDom != "" {
		if d == "" || strings.EqualFold(dnsDom, d) || strings.HasPrefix(strings.ToLower(dnsDom), strings.ToLower(d)+".") {
			return dnsDom
		}
	}
	return d
}

func buildLDAPBindRequest(msgID int) []byte {
	var bindPayload []byte
	bindPayload = append(bindPayload, asn1Integer, 0x01, 0x03) // version = 3
	bindPayload = append(bindPayload, wrapBERTLV(asn1OctetStr, nil)...)
	bindPayload = append(bindPayload, 0x80, 0x00) // simple auth ""

	var msg []byte
	msg = append(msg, wrapBERTLV(asn1Integer, encodeASN1NonNegInt(uint32(msgID)))...)
	msg = append(msg, wrapBERTLV(ldapTagBindRequest, bindPayload)...)
	return wrapBERTLV(asn1Sequence, msg)
}

func buildLDAPSearchRequest(msgID int, baseDN, sAMAccountName string) []byte {
	escapedUser := escapeLDAPFilterValue(sAMAccountName)

	eqUser := wrapBERTLV(ldapTagFilterEq, append(
		wrapBERTLV(asn1OctetStr, []byte("sAMAccountName")),
		wrapBERTLV(asn1OctetStr, []byte(escapedUser))...,
	))
	eqObj := wrapBERTLV(ldapTagFilterEq, append(
		wrapBERTLV(asn1OctetStr, []byte("objectClass")),
		wrapBERTLV(asn1OctetStr, []byte("user"))...,
	))
	filter := wrapBERTLV(ldapTagFilterAnd, append(eqObj, eqUser...))

	var attrsPayload []byte
	for _, attr := range []string{"department", "ou", "distinguishedName"} {
		attrsPayload = append(attrsPayload, wrapBERTLV(asn1OctetStr, []byte(attr))...)
	}
	attrsSeq := wrapBERTLV(asn1Sequence, attrsPayload)

	var searchPayload []byte
	searchPayload = append(searchPayload, wrapBERTLV(asn1OctetStr, []byte(baseDN))...)
	searchPayload = append(searchPayload, ldapTagEnumerated, 0x01, 0x02) // scope: wholeSubtree
	searchPayload = append(searchPayload, ldapTagEnumerated, 0x01, 0x00) // derefAliases: never
	searchPayload = append(searchPayload, asn1Integer, 0x01, 0x01)       // sizeLimit: 1
	searchPayload = append(searchPayload, asn1Integer, 0x01, 0x01)       // timeLimit: 1s
	searchPayload = append(searchPayload, ldapTagBoolean, 0x01, 0x00)    // typesOnly: false
	searchPayload = append(searchPayload, filter...)
	searchPayload = append(searchPayload, attrsSeq...)

	var msg []byte
	msg = append(msg, wrapBERTLV(asn1Integer, encodeASN1NonNegInt(uint32(msgID)))...)
	msg = append(msg, wrapBERTLV(ldapTagSearchRequest, searchPayload)...)
	return wrapBERTLV(asn1Sequence, msg)
}

type ldapParsedMessage struct {
	MsgID      int
	OpTag      byte
	ResultCode int
	ObjectDN   string
	Attrs      map[string]string
}

func parseLDAPMessage(pkt []byte) (*ldapParsedMessage, int, error) {
	tag, body, consumed, err := readBERTLV(pkt)
	if err != nil {
		return nil, 0, err
	}
	if tag != asn1Sequence {
		return nil, 0, fmt.Errorf("expected LDAP SEQUENCE, got 0x%02X", tag)
	}
	_, idBytes, n1, err := readBERTLV(body)
	if err != nil {
		return nil, 0, err
	}
	opTag, opBody, _, err := readBERTLV(body[n1:])
	if err != nil {
		return nil, 0, err
	}
	out := &ldapParsedMessage{
		MsgID: decodeASN1Int(idBytes),
		OpTag: opTag,
		Attrs: make(map[string]string),
	}

	switch opTag {
	case ldapTagBindResponse, ldapTagSearchResultDone:
		_, codeBytes, _, err := readBERTLV(opBody)
		if err != nil {
			return nil, 0, err
		}
		out.ResultCode = decodeASN1Int(codeBytes)

	case ldapTagSearchResultEntry:
		_, dnBytes, nObj, err := readBERTLV(opBody)
		if err != nil {
			return nil, 0, err
		}
		out.ObjectDN = string(dnBytes)
		aTag, aList, _, err := readBERTLV(opBody[nObj:])
		if err == nil && aTag == asn1Sequence {
			for len(aList) > 0 {
				pTag, pBody, c, pErr := readBERTLV(aList)
				if pErr != nil || pTag != asn1Sequence {
					break
				}
				aList = aList[c:]
				_, nameBytes, nAttr, nErr := readBERTLV(pBody)
				if nErr != nil {
					continue
				}
				vTag, vSet, _, vErr := readBERTLV(pBody[nAttr:])
				if vErr != nil || vTag != ldapTagSet {
					continue
				}
				_, valBytes, _, valErr := readBERTLV(vSet)
				if valErr == nil {
					attrName := strings.ToLower(strings.TrimSpace(string(nameBytes)))
					if attrName != "" {
						out.Attrs[attrName] = strings.TrimSpace(string(valBytes))
					}
				}
			}
		}
	}
	return out, consumed, nil
}

func extractDepartmentFromLDAPEntry(entry *ldapParsedMessage) string {
	if entry == nil {
		return ""
	}
	if dept := strings.TrimSpace(entry.Attrs["department"]); dept != "" {
		return dept
	}
	if ou := strings.TrimSpace(entry.Attrs["ou"]); ou != "" {
		return ou
	}
	dn := strings.TrimSpace(entry.Attrs["distinguishedname"])
	if dn == "" {
		dn = entry.ObjectDN
	}
	return extractOUFromDN(dn)
}

func readLDAPPackets(conn net.Conn) ([]*ldapParsedMessage, error) {
	buf := make([]byte, 16384)
	total := 0
	var msgs []*ldapParsedMessage
	for {
		n, err := conn.Read(buf[total:])
		if n > 0 {
			total += n
			pos := 0
			for pos < total {
				msg, used, pErr := parseLDAPMessage(buf[pos:total])
				if pErr != nil || used == 0 {
					break
				}
				msgs = append(msgs, msg)
				pos += used
				if msg.OpTag == ldapTagBindResponse || msg.OpTag == ldapTagSearchResultDone {
					return msgs, nil
				}
			}
			if pos > 0 && pos < total {
				copy(buf, buf[pos:total])
				total -= pos
			} else if pos == total {
				total = 0
			}
		}
		if err != nil {
			if len(msgs) > 0 {
				return msgs, nil
			}
			return nil, err
		}
		if total >= len(buf) {
			break
		}
	}
	return msgs, nil
}

func queryLDAPDepartmentOverTCP(ctx context.Context, addr, baseDN, username string, timeout time.Duration) (string, error) {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	// 1. Anonymous Bind (LDAPv3)
	if _, err := conn.Write(buildLDAPBindRequest(1)); err != nil {
		return "", err
	}
	bindMsgs, err := readLDAPPackets(conn)
	if err != nil || len(bindMsgs) == 0 || bindMsgs[0].ResultCode != 0 {
		return "", fmt.Errorf("ldap bind declined")
	}

	// 2. Read-only SearchRequest for user's department / OU / DN
	if _, err := conn.Write(buildLDAPSearchRequest(2, baseDN, username)); err != nil {
		return "", err
	}
	searchMsgs, err := readLDAPPackets(conn)
	if err != nil {
		return "", err
	}
	for _, m := range searchMsgs {
		if m.OpTag == ldapTagSearchResultEntry {
			if dept := extractDepartmentFromLDAPEntry(m); dept != "" {
				return dept, nil
			}
		}
	}
	return "", fmt.Errorf("department not found")
}

// ResolveDepartment looks up a user's AD department via SRV + read-only LDAP (1.5s cap, cached).
// Safely falls back to domain name when offline, unauthenticated, or non-AD.
func (r *LDAPDepartmentResolver) ResolveDepartment(ctx context.Context, domain, username, machineName string) string {
	fallback := strings.TrimSpace(domain)
	user := strings.TrimSpace(username)
	if idx := strings.IndexByte(user, '\\'); idx >= 0 {
		if fallback == "" {
			fallback = strings.TrimSpace(user[:idx])
		}
		user = strings.TrimSpace(user[idx+1:])
	}
	if at := strings.IndexByte(user, '@'); at > 0 {
		if fallback == "" {
			fallback = strings.TrimSpace(user[at+1:])
		}
		user = strings.TrimSpace(user[:at])
	}

	if user == "" || isNonADDomain(fallback, machineName) {
		return fallback
	}

	cacheKey := strings.ToLower(fallback + `\` + user)
	now := time.Now()

	r.mu.RLock()
	if ent, ok := r.cache[cacheKey]; ok && now.Before(ent.expiresAt) {
		r.mu.RUnlock()
		if ent.department != "" {
			return ent.department
		}
		return fallback
	}
	r.mu.RUnlock()

	timeout := r.timeout
	if timeout <= 0 {
		timeout = ldapDefaultTimeout
	}
	qCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dnsDomain := resolveLDAPSearchDomain(fallback)
	baseDN := domainToBaseDN(dnsDomain)
	if baseDN == "" {
		r.storeCache(cacheKey, "", 5*time.Minute)
		return fallback
	}

	servers, err := r.lookupSRV(qCtx, dnsDomain)
	if err != nil || len(servers) == 0 {
		r.storeCache(cacheKey, "", 5*time.Minute)
		return fallback
	}

	for _, addr := range servers {
		if qCtx.Err() != nil {
			break
		}
		if dept, qErr := queryLDAPDepartmentOverTCP(qCtx, addr, baseDN, user, timeout); qErr == nil && dept != "" {
			r.storeCache(cacheKey, dept, 30*time.Minute)
			return dept
		}
	}

	r.storeCache(cacheKey, "", 5*time.Minute)
	return fallback
}

func (r *LDAPDepartmentResolver) storeCache(key, dept string, ttl time.Duration) {
	r.mu.Lock()
	r.cache[key] = ldapCacheEntry{
		department: dept,
		expiresAt:  time.Now().Add(ttl),
	}
	r.mu.Unlock()
}

// Keep encoding/binary imported for future LDAPS/SASL length framing if needed; ensure no unused import.
var _ = binary.BigEndian
