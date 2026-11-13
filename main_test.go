package main

import (
	"strings"
	"testing"
)

func TestGenerateValidPixPayload(t *testing.T) {
	payload := GenerateValidPixPayload("18.234.567/0001-89", "STAR COPIAS", "SAO PAULO", 150.50, "PM001")
	if !strings.HasPrefix(payload, "000201010212") {
		t.Errorf("invalid EMVCo PIX prefix: %s", payload)
	}
	if !strings.Contains(payload, "5406150.50") {
		t.Errorf("expected formatted amount 150.50 in PIX payload: %s", payload)
	}
	if len(payload) < 8 || payload[len(payload)-8:len(payload)-4] != "6304" {
		t.Errorf("expected CRC16 field 6304 at end of payload: %s", payload)
	}
	base := payload[:len(payload)-4]
	crc := payload[len(payload)-4:]
	if got := crc16CCITT(base); got != crc {
		t.Errorf("CRC16 mismatch: got %s, payload had %s", got, crc)
	}
}

func TestServerEncodeOIDAndBERLength(t *testing.T) {
	oid, err := encodeOID("1.3.6.1.2.1.1.1.0")
	if err != nil {
		t.Fatalf("encodeOID failed: %v", err)
	}
	if len(oid) != 8 || oid[0] != 0x2B {
		t.Errorf("unexpected encoded OID: %x", oid)
	}
	length, readBytes, err := parseBERLength([]byte{0x81, 0x90})
	if err != nil || length != 144 || readBytes != 2 {
		t.Errorf("parseBERLength got (%d, %d, %v), want (144, 2, nil)", length, readBytes, err)
	}
}
