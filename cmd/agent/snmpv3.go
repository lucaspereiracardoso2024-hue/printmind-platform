package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

const (
	pduReport byte = 0xA8

	snmpV3FlagAuth       byte = 0x01
	snmpV3FlagPriv       byte = 0x02
	snmpV3FlagReportable byte = 0x04

	snmpV3MaxMsgSize = 65507
)

var snmpV3GlobalCounter uint64 = 1

func nextSNMPV3Counter() uint64 {
	return atomic.AddUint64(&snmpV3GlobalCounter, 1)
}

func normalizeSNMPv3AuthProto(proto string) string {
	switch strings.ToUpper(strings.TrimSpace(proto)) {
	case "MD5":
		return "MD5"
	case "SHA", "SHA1", "SHA-1":
		return "SHA1"
	case "SHA256", "SHA-256", "HMAC192SHA256":
		return "SHA256"
	case "SHA256-128", "HMAC128SHA256":
		return "SHA256-128"
	default:
		return strings.ToUpper(strings.TrimSpace(proto))
	}
}

func normalizeSNMPv3PrivProto(proto string) string {
	switch strings.ToUpper(strings.TrimSpace(proto)) {
	case "DES", "CBC-DES":
		return "DES"
	case "AES", "AES128", "AES-128", "AES-128-CFB", "CFB128-AES-128":
		return "AES"
	default:
		return strings.ToUpper(strings.TrimSpace(proto))
	}
}

func normalizeSNMPv3SecurityLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "noauthnopriv", "no_auth_no_priv":
		return "noAuthNoPriv"
	case "authnopriv", "auth_no_priv":
		return "authNoPriv"
	case "authpriv", "auth_priv":
		return "authPriv"
	default:
		return strings.TrimSpace(level)
	}
}

func snmpv3HashFunc(authProto string) (func() hash.Hash, int, error) {
	switch normalizeSNMPv3AuthProto(authProto) {
	case "MD5":
		return md5.New, 12, nil
	case "SHA1":
		return sha1.New, 12, nil
	case "SHA256":
		return sha256.New, 24, nil
	case "SHA256-128":
		return sha256.New, 16, nil
	default:
		return nil, 0, fmt.Errorf("unsupported SNMPv3 auth protocol %q", authProto)
	}
}

// deriveSNMPv3MasterKey implements RFC 3414 Appendix A.2.1 password-to-key (Ku) expansion to 1 MiB.
func deriveSNMPv3MasterKey(authProto, passphrase string) ([]byte, error) {
	if passphrase == "" {
		return nil, fmt.Errorf("SNMPv3 passphrase cannot be empty")
	}
	newHash, _, err := snmpv3HashFunc(authProto)
	if err != nil {
		return nil, err
	}

	h := newHash()
	pw := []byte(passphrase)
	pwLen := len(pw)

	var block [64]byte
	pIdx := 0
	for count := 0; count < 1048576; count += len(block) {
		for i := 0; i < len(block); i++ {
			block[i] = pw[pIdx]
			pIdx++
			if pIdx == pwLen {
				pIdx = 0
			}
		}
		_, _ = h.Write(block[:])
	}
	return h.Sum(nil), nil
}

// localizeSNMPv3Key implements RFC 3414 Section 2.6 key localization: Kul = H(Ku || engineID || Ku).
func localizeSNMPv3Key(authProto string, ku, engineID []byte) ([]byte, error) {
	if len(ku) == 0 {
		return nil, fmt.Errorf("empty master key Ku")
	}
	if len(engineID) == 0 {
		return nil, fmt.Errorf("empty engineID")
	}
	newHash, _, err := snmpv3HashFunc(authProto)
	if err != nil {
		return nil, err
	}
	h := newHash()
	_, _ = h.Write(ku)
	_, _ = h.Write(engineID)
	_, _ = h.Write(ku)
	return h.Sum(nil), nil
}

// encodeASN1NonNegInt encodes a uint32 as a minimal big-endian ASN.1 INTEGER payload (never negative).
func encodeASN1NonNegInt(v uint32) []byte {
	if v == 0 {
		return []byte{0x00}
	}
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], v)
	i := 0
	for i < 3 && raw[i] == 0 {
		i++
	}
	out := raw[i:]
	if out[0]&0x80 != 0 {
		prefixed := make([]byte, 1+len(out))
		prefixed[0] = 0x00
		copy(prefixed[1:], out)
		return prefixed
	}
	cp := make([]byte, len(out))
	copy(cp, out)
	return cp
}

// SNMPv3Session holds USM state (discovered engineID, boots, time, localized keys, and counters).
type SNMPv3Session struct {
	Config      SNMPv3Config
	EngineID    []byte
	EngineBoots uint32
	EngineTime  uint32
	ContextName string
	AuthKul     []byte
	PrivKul     []byte
	SaltCounter uint64
}

func newSNMPv3Session(cfg SNMPv3Config) (*SNMPv3Session, error) {
	cfg.SecurityLevel = normalizeSNMPv3SecurityLevel(cfg.SecurityLevel)
	if cfg.AuthProtocol == "" && (cfg.SecurityLevel == "authNoPriv" || cfg.SecurityLevel == "authPriv") {
		cfg.AuthProtocol = "SHA1"
	} else {
		cfg.AuthProtocol = normalizeSNMPv3AuthProto(cfg.AuthProtocol)
	}
	if cfg.PrivProtocol == "" && cfg.SecurityLevel == "authPriv" {
		cfg.PrivProtocol = "AES"
	} else {
		cfg.PrivProtocol = normalizeSNMPv3PrivProto(cfg.PrivProtocol)
	}

	if cfg.SecurityLevel == "authNoPriv" || cfg.SecurityLevel == "authPriv" {
		if cfg.AuthPassphrase == "" {
			return nil, fmt.Errorf("SNMPv3 auth_passphrase required for %s", cfg.SecurityLevel)
		}
		if _, _, err := snmpv3HashFunc(cfg.AuthProtocol); err != nil {
			return nil, err
		}
	}
	if cfg.SecurityLevel == "authPriv" {
		if cfg.PrivPassphrase == "" {
			return nil, fmt.Errorf("SNMPv3 priv_passphrase required for authPriv")
		}
		if cfg.PrivProtocol != "DES" && cfg.PrivProtocol != "AES" {
			return nil, fmt.Errorf("unsupported SNMPv3 priv protocol %q", cfg.PrivProtocol)
		}
	}

	var seed [8]byte
	if _, err := rand.Read(seed[:]); err != nil {
		binary.BigEndian.PutUint64(seed[:], uint64(time.Now().UnixNano()))
	}
	return &SNMPv3Session{
		Config:      cfg,
		SaltCounter: binary.BigEndian.Uint64(seed[:]),
	}, nil
}

func (s *SNMPv3Session) setEngine(engineID []byte, boots, engTime uint32) error {
	s.EngineID = append([]byte(nil), engineID...)
	s.EngineBoots = boots
	s.EngineTime = engTime

	lvl := normalizeSNMPv3SecurityLevel(s.Config.SecurityLevel)
	if lvl == "authNoPriv" || lvl == "authPriv" {
		ku, err := deriveSNMPv3MasterKey(s.Config.AuthProtocol, s.Config.AuthPassphrase)
		if err != nil {
			return err
		}
		kul, err := localizeSNMPv3Key(s.Config.AuthProtocol, ku, s.EngineID)
		if err != nil {
			return err
		}
		s.AuthKul = kul
	}
	if lvl == "authPriv" {
		kuP, err := deriveSNMPv3MasterKey(s.Config.AuthProtocol, s.Config.PrivPassphrase)
		if err != nil {
			return err
		}
		kulP, err := localizeSNMPv3Key(s.Config.AuthProtocol, kuP, s.EngineID)
		if err != nil {
			return err
		}
		if len(kulP) < 16 {
			return fmt.Errorf("localized priv key too short (%d bytes)", len(kulP))
		}
		s.PrivKul = kulP
	}
	return nil
}

func encryptSNMPv3ScopedPDU(privProto string, privKul []byte, boots, engTime uint32, saltCounter uint64, plaintext []byte) ([]byte, []byte, error) {
	if len(privKul) < 16 {
		return nil, nil, fmt.Errorf("priv key must be >= 16 bytes")
	}
	proto := normalizeSNMPv3PrivProto(privProto)
	switch proto {
	case "AES":
		block, err := aes.NewCipher(privKul[:16])
		if err != nil {
			return nil, nil, err
		}
		salt := make([]byte, 8)
		binary.BigEndian.PutUint64(salt, saltCounter)
		iv := make([]byte, 16)
		binary.BigEndian.PutUint32(iv[0:4], boots)
		binary.BigEndian.PutUint32(iv[4:8], engTime)
		copy(iv[8:16], salt)

		ciphertext := make([]byte, len(plaintext))
		cipher.NewCFBEncrypter(block, iv).XORKeyStream(ciphertext, plaintext)
		return ciphertext, salt, nil

	case "DES":
		block, err := des.NewCipher(privKul[:8])
		if err != nil {
			return nil, nil, err
		}
		salt := make([]byte, 8)
		binary.BigEndian.PutUint32(salt[0:4], boots)
		binary.BigEndian.PutUint32(salt[4:8], uint32(saltCounter))
		iv := make([]byte, 8)
		for i := 0; i < 8; i++ {
			iv[i] = privKul[8+i] ^ salt[i]
		}
		padded := plaintext
		if rem := len(padded) % 8; rem != 0 {
			padded = make([]byte, len(plaintext)+(8-rem))
			copy(padded, plaintext)
		}
		ciphertext := make([]byte, len(padded))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)
		return ciphertext, salt, nil

	default:
		return nil, nil, fmt.Errorf("unsupported priv protocol %q", privProto)
	}
}

func decryptSNMPv3ScopedPDU(privProto string, privKul []byte, boots, engTime uint32, privParams, ciphertext []byte) ([]byte, error) {
	if len(privKul) < 16 {
		return nil, fmt.Errorf("priv key must be >= 16 bytes")
	}
	if len(privParams) != 8 {
		return nil, fmt.Errorf("invalid SNMPv3 privacy parameters length %d", len(privParams))
	}
	proto := normalizeSNMPv3PrivProto(privProto)
	switch proto {
	case "AES":
		block, err := aes.NewCipher(privKul[:16])
		if err != nil {
			return nil, err
		}
		iv := make([]byte, 16)
		binary.BigEndian.PutUint32(iv[0:4], boots)
		binary.BigEndian.PutUint32(iv[4:8], engTime)
		copy(iv[8:16], privParams)

		plaintext := make([]byte, len(ciphertext))
		cipher.NewCFBDecrypter(block, iv).XORKeyStream(plaintext, ciphertext)
		return plaintext, nil

	case "DES":
		if len(ciphertext) == 0 || len(ciphertext)%8 != 0 {
			return nil, fmt.Errorf("invalid DES ciphertext length %d", len(ciphertext))
		}
		block, err := des.NewCipher(privKul[:8])
		if err != nil {
			return nil, err
		}
		iv := make([]byte, 8)
		for i := 0; i < 8; i++ {
			iv[i] = privKul[8+i] ^ privParams[i]
		}
		plaintext := make([]byte, len(ciphertext))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, ciphertext)
		return plaintext, nil

	default:
		return nil, fmt.Errorf("unsupported priv protocol %q", privProto)
	}
}

func computeSNMPv3MAC(authProto string, authKul, msg []byte, macLen int) ([]byte, error) {
	newHash, defaultLen, err := snmpv3HashFunc(authProto)
	if err != nil {
		return nil, err
	}
	if macLen <= 0 {
		macLen = defaultLen
	}
	mac := hmac.New(newHash, authKul)
	_, _ = mac.Write(msg)
	sum := mac.Sum(nil)
	if len(sum) < macLen {
		return nil, fmt.Errorf("digest shorter than expected MAC length %d", macLen)
	}
	return sum[:macLen], nil
}

func buildSNMPPDU(pduTag byte, reqID uint32, oids []string) ([]byte, error) {
	var vbListPayload []byte
	for _, oidStr := range oids {
		oidBytes, err := encodeOID(oidStr)
		if err != nil {
			return nil, err
		}
		vbInner := append(wrapBERTLV(asn1OID, oidBytes), asn1Null, 0x00)
		vbListPayload = append(vbListPayload, wrapBERTLV(asn1Sequence, vbInner)...)
	}
	vbList := wrapBERTLV(asn1Sequence, vbListPayload)

	posReqID := reqID & 0x7FFFFFFF
	var pduPayload []byte
	pduPayload = append(pduPayload, wrapBERTLV(asn1Integer, encodeASN1NonNegInt(posReqID))...)
	pduPayload = append(pduPayload, asn1Integer, 0x01, 0x00) // error-status = 0
	pduPayload = append(pduPayload, asn1Integer, 0x01, 0x00) // error-index = 0
	pduPayload = append(pduPayload, vbList...)
	return wrapBERTLV(pduTag, pduPayload), nil
}

func buildSNMPv3Packet(sess *SNMPv3Session, pduTag byte, msgID, reqID uint32, oids []string, discovery bool) ([]byte, error) {
	pdu, err := buildSNMPPDU(pduTag, reqID, oids)
	if err != nil {
		return nil, err
	}

	var (
		flags      byte = snmpV3FlagReportable
		engineID   []byte
		boots      uint32
		engTime    uint32
		username   string
		authMacLen int
		privParams []byte
	)

	if !discovery && sess != nil {
		engineID = sess.EngineID
		boots = sess.EngineBoots
		engTime = sess.EngineTime
		username = sess.Config.Username

		lvl := normalizeSNMPv3SecurityLevel(sess.Config.SecurityLevel)
		if lvl == "authNoPriv" || lvl == "authPriv" {
			flags |= snmpV3FlagAuth
			_, authMacLen, err = snmpv3HashFunc(sess.Config.AuthProtocol)
			if err != nil {
				return nil, err
			}
		}
		if lvl == "authPriv" {
			flags |= snmpV3FlagPriv
		}
	}

	ctxEngineID := engineID
	ctxName := ""
	if sess != nil {
		ctxName = sess.ContextName
	}
	var scopedPayload []byte
	scopedPayload = append(scopedPayload, wrapBERTLV(asn1OctetStr, ctxEngineID)...)
	scopedPayload = append(scopedPayload, wrapBERTLV(asn1OctetStr, []byte(ctxName))...)
	scopedPayload = append(scopedPayload, pdu...)
	scopedPDU := wrapBERTLV(asn1Sequence, scopedPayload)

	msgData := scopedPDU
	if flags&snmpV3FlagPriv != 0 {
		sess.SaltCounter++
		enc, salt, encErr := encryptSNMPv3ScopedPDU(sess.Config.PrivProtocol, sess.PrivKul, boots, engTime, sess.SaltCounter, scopedPDU)
		if encErr != nil {
			return nil, encErr
		}
		privParams = salt
		msgData = wrapBERTLV(asn1OctetStr, enc)
	}

	var globalPayload []byte
	globalPayload = append(globalPayload, wrapBERTLV(asn1Integer, encodeASN1NonNegInt(msgID&0x7FFFFFFF))...)
	globalPayload = append(globalPayload, wrapBERTLV(asn1Integer, encodeASN1NonNegInt(snmpV3MaxMsgSize))...)
	globalPayload = append(globalPayload, wrapBERTLV(asn1OctetStr, []byte{flags})...)
	globalPayload = append(globalPayload, asn1Integer, 0x01, 0x03) // USM = 3
	globalTLV := wrapBERTLV(asn1Sequence, globalPayload)

	authPlaceholder := make([]byte, authMacLen)
	authTLV := wrapBERTLV(asn1OctetStr, authPlaceholder)
	privTLV := wrapBERTLV(asn1OctetStr, privParams)

	var usmPayload []byte
	usmPayload = append(usmPayload, wrapBERTLV(asn1OctetStr, engineID)...)
	usmPayload = append(usmPayload, wrapBERTLV(asn1Integer, encodeASN1NonNegInt(boots))...)
	usmPayload = append(usmPayload, wrapBERTLV(asn1Integer, encodeASN1NonNegInt(engTime))...)
	usmPayload = append(usmPayload, wrapBERTLV(asn1OctetStr, []byte(username))...)
	usmPayload = append(usmPayload, authTLV...)
	usmPayload = append(usmPayload, privTLV...)
	usmSeq := wrapBERTLV(asn1Sequence, usmPayload)
	secParamsTLV := wrapBERTLV(asn1OctetStr, usmSeq)

	versionTLV := []byte{asn1Integer, 0x01, 0x03} // SNMPv3 (3)
	var msgBody []byte
	msgBody = append(msgBody, versionTLV...)
	msgBody = append(msgBody, globalTLV...)
	msgBody = append(msgBody, secParamsTLV...)
	msgBody = append(msgBody, msgData...)

	pkt := wrapBERTLV(asn1Sequence, msgBody)

	if flags&snmpV3FlagAuth != 0 {
		headerOverhead := len(pkt) - len(msgBody)
		macStart := headerOverhead + len(versionTLV) + len(globalTLV) + len(secParamsTLV) - len(privTLV) - authMacLen
		mac, err := computeSNMPv3MAC(sess.Config.AuthProtocol, sess.AuthKul, pkt, authMacLen)
		if err != nil {
			return nil, err
		}
		copy(pkt[macStart:macStart+authMacLen], mac)
	}
	return pkt, nil
}

type snmpV3ParsedMessage struct {
	MsgID       uint32
	Flags       byte
	EngineID    []byte
	EngineBoots uint32
	EngineTime  uint32
	Username    string
	PDUTag      byte
	ReqID       uint32
	ErrStatus   int
	VarBinds    []SNMPVarBind
}

func parseSNMPv3Packet(pkt []byte, sess *SNMPv3Session) (*snmpV3ParsedMessage, error) {
	tag, msgBody, _, err := readBERTLV(pkt)
	if err != nil || tag != asn1Sequence {
		return nil, fmt.Errorf("invalid outer SNMPv3 sequence")
	}

	// msgVersion
	verTag, verBytes, n, err := readBERTLV(msgBody)
	if err != nil || verTag != asn1Integer || decodeASN1Int(verBytes) != 3 {
		return nil, fmt.Errorf("not an SNMPv3 packet")
	}
	rest := msgBody[n:]

	// msgGlobalData
	gTag, gBody, n, err := readBERTLV(rest)
	if err != nil || gTag != asn1Sequence {
		return nil, fmt.Errorf("invalid SNMPv3 msgGlobalData")
	}
	rest = rest[n:]

	_, msgIDBytes, n1, err := readBERTLV(gBody)
	if err != nil {
		return nil, err
	}
	msgID, _ := decodeASN1Uint32(msgIDBytes)
	gRest := gBody[n1:]
	_, _, n2, err := readBERTLV(gRest) // msgMaxSize
	if err != nil {
		return nil, err
	}
	gRest = gRest[n2:]
	_, flagBytes, _, err := readBERTLV(gRest)
	if err != nil || len(flagBytes) == 0 {
		return nil, fmt.Errorf("missing SNMPv3 msgFlags")
	}
	flags := flagBytes[0]

	// msgSecurityParameters (OCTET STRING wrapping UsmSecurityParameters SEQUENCE)
	secTag, secBytes, n, err := readBERTLV(rest)
	if err != nil || secTag != asn1OctetStr {
		return nil, fmt.Errorf("invalid msgSecurityParameters")
	}
	secAbsStart := len(pkt) - len(rest) + (len(rest[:n]) - len(secBytes))
	rest = rest[n:]

	usmTag, usmBody, _, err := readBERTLV(secBytes)
	if err != nil || usmTag != asn1Sequence {
		return nil, fmt.Errorf("invalid UsmSecurityParameters")
	}
	usmAbsStart := secAbsStart + (len(secBytes) - len(usmBody))

	// engineID
	_, engineID, u1, err := readBERTLV(usmBody)
	if err != nil {
		return nil, err
	}
	uRest := usmBody[u1:]

	// engineBoots
	_, bootsBytes, u2, err := readBERTLV(uRest)
	if err != nil {
		return nil, err
	}
	boots, _ := decodeASN1Uint32(bootsBytes)
	uRest = uRest[u2:]

	// engineTime
	_, timeBytes, u3, err := readBERTLV(uRest)
	if err != nil {
		return nil, err
	}
	engTime, _ := decodeASN1Uint32(timeBytes)
	uRest = uRest[u3:]

	// userName
	_, userBytes, u4, err := readBERTLV(uRest)
	if err != nil {
		return nil, err
	}
	uRest = uRest[u4:]

	// msgAuthenticationParameters
	_, authParams, u5, err := readBERTLV(uRest)
	if err != nil {
		return nil, err
	}
	authValAbsStart := usmAbsStart + (len(usmBody) - len(uRest)) + (u5 - len(authParams))
	uRest = uRest[u5:]

	// msgPrivacyParameters
	_, privParams, _, err := readBERTLV(uRest)
	if err != nil {
		return nil, err
	}

	if flags&snmpV3FlagAuth != 0 {
		if sess == nil || len(sess.AuthKul) == 0 {
			return nil, fmt.Errorf("authenticated SNMPv3 message received without auth key")
		}
		if len(authParams) < 12 {
			return nil, fmt.Errorf("SNMPv3 auth parameters too short (%d)", len(authParams))
		}
		verifyCopy := make([]byte, len(pkt))
		copy(verifyCopy, pkt)
		for i := 0; i < len(authParams); i++ {
			verifyCopy[authValAbsStart+i] = 0x00
		}
		expectedMAC, mErr := computeSNMPv3MAC(sess.Config.AuthProtocol, sess.AuthKul, verifyCopy, len(authParams))
		if mErr != nil || !hmac.Equal(authParams, expectedMAC) {
			return nil, fmt.Errorf("SNMPv3 HMAC verification failed")
		}
	}

	// ScopedPDUData (plaintext SEQUENCE or encrypted OCTET STRING)
	dataTag, dataBody, _, err := readBERTLV(rest)
	if err != nil {
		return nil, err
	}

	var scopedSeqBody []byte
	if flags&snmpV3FlagPriv != 0 {
		if dataTag != asn1OctetStr {
			return nil, fmt.Errorf("expected encrypted ScopedPDU OCTET STRING, got 0x%02X", dataTag)
		}
		if sess == nil || len(sess.PrivKul) == 0 {
			return nil, fmt.Errorf("encrypted SNMPv3 message received without priv key")
		}
		plainScoped, decErr := decryptSNMPv3ScopedPDU(sess.Config.PrivProtocol, sess.PrivKul, boots, engTime, privParams, dataBody)
		if decErr != nil {
			return nil, decErr
		}
		sTag, sBody, _, sErr := readBERTLV(plainScoped)
		if sErr != nil || sTag != asn1Sequence {
			return nil, fmt.Errorf("decrypted ScopedPDU is not an ASN.1 sequence")
		}
		scopedSeqBody = sBody
	} else {
		if dataTag != asn1Sequence {
			return nil, fmt.Errorf("expected plaintext ScopedPDU sequence, got 0x%02X", dataTag)
		}
		scopedSeqBody = dataBody
	}

	// contextEngineID, contextName, PDU
	_, _, sc1, err := readBERTLV(scopedSeqBody)
	if err != nil {
		return nil, err
	}
	scRest := scopedSeqBody[sc1:]
	_, _, sc2, err := readBERTLV(scRest)
	if err != nil {
		return nil, err
	}
	pduBytes := scRest[sc2:]

	pduTag, pduBody, _, err := readBERTLV(pduBytes)
	if err != nil {
		return nil, err
	}
	if pduTag != pduGetResponse && pduTag != pduReport {
		return nil, fmt.Errorf("unexpected SNMPv3 PDU tag 0x%02X", pduTag)
	}

	reqID, errStatus, vbs, err := parseSNMPPDUFields(pduBody)
	if err != nil {
		return nil, err
	}

	return &snmpV3ParsedMessage{
		MsgID:       msgID,
		Flags:       flags,
		EngineID:    engineID,
		EngineBoots: boots,
		EngineTime:  engTime,
		Username:    string(userBytes),
		PDUTag:      pduTag,
		ReqID:       reqID,
		ErrStatus:   errStatus,
		VarBinds:    vbs,
	}, nil
}

func parseSNMPPDUFields(pduBody []byte) (reqID uint32, errStatus int, varBinds []SNMPVarBind, err error) {
	_, reqIDBytes, n, err := readBERTLV(pduBody)
	if err != nil {
		return 0, 0, nil, err
	}
	for _, b := range reqIDBytes {
		reqID = (reqID << 8) | uint32(b)
	}
	pduRest := pduBody[n:]

	_, errBytes, n, err := readBERTLV(pduRest)
	if err != nil {
		return 0, 0, nil, err
	}
	errStatus = decodeASN1Int(errBytes)
	pduRest = pduRest[n:]

	_, _, n, err = readBERTLV(pduRest)
	if err != nil {
		return 0, 0, nil, err
	}
	pduRest = pduRest[n:]

	vbListTag, vbListBody, _, err := readBERTLV(pduRest)
	if err != nil || vbListTag != asn1Sequence {
		return 0, 0, nil, fmt.Errorf("invalid varbind list")
	}

	for len(vbListBody) > 0 {
		vbTag, vbBody, consumed, err := readBERTLV(vbListBody)
		if err != nil || vbTag != asn1Sequence {
			break
		}
		vbListBody = vbListBody[consumed:]

		oidTag, oidBytes, n1, err := readBERTLV(vbBody)
		if err != nil || oidTag != asn1OID {
			continue
		}
		oidStr, err := decodeOID(oidBytes)
		if err != nil {
			continue
		}
		valTag, valBytes, _, err := readBERTLV(vbBody[n1:])
		if err != nil {
			continue
		}
		varBinds = append(varBinds, SNMPVarBind{
			OID:   oidStr,
			Tag:   valTag,
			Value: valBytes,
		})
	}
	return reqID, errStatus, varBinds, nil
}

func exchangeRawUDP(ctx context.Context, ip string, pkt []byte, timeout time.Duration) ([]byte, error) {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "udp", net.JoinHostPort(ip, "161"))
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write(pkt); err != nil {
		return nil, err
	}
	buf := make([]byte, 8192)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func discoverSNMPv3Engine(ctx context.Context, ip string, sess *SNMPv3Session, timeout time.Duration) error {
	c := uint32(nextSNMPV3Counter())
	pkt, err := buildSNMPv3Packet(sess, pduGetRequest, c, c, nil, true)
	if err != nil {
		return err
	}
	raw, err := exchangeRawUDP(ctx, ip, pkt, timeout)
	if err != nil {
		return err
	}
	parsed, err := parseSNMPv3Packet(raw, nil)
	if err != nil {
		return err
	}
	if len(parsed.EngineID) == 0 {
		return fmt.Errorf("SNMPv3 engine discovery returned empty engineID")
	}
	return sess.setEngine(parsed.EngineID, parsed.EngineBoots, parsed.EngineTime)
}

func exchangeSNMPv3(ctx context.Context, ip string, sess *SNMPv3Session, pduTag byte, oids []string, timeout time.Duration) ([]SNMPVarBind, int, error) {
	if len(sess.EngineID) == 0 {
		if err := discoverSNMPv3Engine(ctx, ip, sess, timeout); err != nil {
			return nil, 0, err
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		c := uint32(nextSNMPV3Counter())
		pkt, err := buildSNMPv3Packet(sess, pduTag, c, c, oids, false)
		if err != nil {
			return nil, 0, err
		}
		raw, err := exchangeRawUDP(ctx, ip, pkt, timeout)
		if err != nil {
			return nil, 0, err
		}
		parsed, pErr := parseSNMPv3Packet(raw, sess)
		if pErr != nil {
			// Unauthenticated Report PDU (e.g. usmStatsNotInTimeWindows) may carry updated boots/time
			if unauth, uErr := parseSNMPv3Packet(raw, nil); uErr == nil && unauth.PDUTag == pduReport && len(unauth.EngineID) > 0 && attempt == 0 {
				if err := sess.setEngine(unauth.EngineID, unauth.EngineBoots, unauth.EngineTime); err == nil {
					continue
				}
			}
			return nil, 0, pErr
		}
		if parsed.PDUTag == pduReport {
			if len(parsed.EngineID) > 0 && attempt == 0 {
				if err := sess.setEngine(parsed.EngineID, parsed.EngineBoots, parsed.EngineTime); err == nil {
					continue
				}
			}
			return nil, 0, fmt.Errorf("SNMPv3 USM report PDU received")
		}
		return parsed.VarBinds, parsed.ErrStatus, nil
	}
	return nil, 0, fmt.Errorf("SNMPv3 exchange failed after retry")
}

func probeDeviceSNMPv3(ctx context.Context, ip string, cfg SNMPv3Config, enableWakeup bool) (*DiscoveredDevice, bool) {
	if !cfg.Enabled || strings.TrimSpace(cfg.Username) == "" {
		return nil, false
	}
	sess, err := newSNMPv3Session(cfg)
	if err != nil {
		return nil, false
	}
	client := &snmpClient{ip: ip, v3: sess}
	return probeDeviceWithSNMPClient(ctx, client, enableWakeup)
}
