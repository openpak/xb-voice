package xmatch

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The search request vector of the Xombie codec's tests (a real client capture
// shape): title 0x4c41000b, procedure 1, one user, eight attributes.
const xombieSearch = `
88000000 0b00414c 01000000
 0a00020f c0a8015b 9073 0050f21f6453 0a00006400020000010000000000000000000000
0100 0000 08000000
58000000 5c000000 60000000 6c000000 78000000 7c000000 80000000 84000000
0000f000 0000f000 000000000000000000000000
000000000800000000000000
0000f000 0000f000 0000f000 0000f000`

func TestSearchVector(t *testing.T) {
	raw := unhex(t, xombieSearch)
	s, err := DecodeSearch(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.TitleID != 0x4c41000b || s.ProcedureIndex != 1 || s.NumUsers != 1 || len(s.Attributes) != 8 {
		t.Fatalf("%+v", s)
	}
	if s.Client.Ina() != [4]byte{10, 0, 2, 15} || s.Client.InaOnline() != [4]byte{192, 168, 1, 91} || s.Client.PortOnline() != 0x7390 {
		t.Fatalf("addr %v", s.Client)
	}
	if s.Attributes[0].Kind != KindNull || s.Attributes[2].Kind != KindInt || s.Attributes[3].Int != 8 {
		t.Fatalf("attrs %+v", s.Attributes)
	}
	if !bytes.Equal(s.Encode(), raw) {
		t.Fatalf("re-encode differs:\n%x\n%x", s.Encode(), raw)
	}
}

func TestAttributeVectors(t *testing.T) {
	for _, c := range []struct {
		hex string
		a   Attribute
	}{
		{"6745f000", Attribute{Tag: 0x4567, Kind: KindNull}},
		{"deab0000 efcdab8967452301", Attribute{Tag: 0xabde, Kind: KindInt, Int: 0x0123456789abcdef}},
		{"03001000 0900 6d6f6e6f6361736100", Attribute{Tag: 3, Kind: KindString, Str: "monocasa"}},
		{"05002000 0300 010203", Attribute{Tag: 5, Kind: KindBlob, Blob: []byte{1, 2, 3}}},
	} {
		raw := unhex(t, c.hex)
		got, err := decodeAttribute(raw)
		if err != nil || got.Tag != c.a.Tag || got.Kind != c.a.Kind || got.Int != c.a.Int || got.Str != c.a.Str || !bytes.Equal(got.Blob, c.a.Blob) {
			t.Fatalf("%s: %+v %v", c.hex, got, err)
		}
		if enc := c.a.appendTo(nil); !bytes.Equal(enc, raw) {
			t.Fatalf("%s: encoded %x", c.hex, enc)
		}
	}
}

func TestSessionRoundTripAndLimits(t *testing.T) {
	var host Addr
	host[4], host[5], host[6], host[7] = 203, 0, 113, 9
	s := Session{TitleID: 0xFFFE0000, Host: host, PublicOpen: 0, PrivateOpen: 15, PublicFilled: 1, PrivateFilled: 1}
	raw := s.Encode()
	if len(raw) != SessionHeader {
		t.Fatalf("a voice chat create carries no attributes: %d bytes", len(raw))
	}
	got, err := DecodeSession(raw)
	if err != nil || got.TitleID != 0xFFFE0000 || got.PrivateOpen != 15 || got.Host != host || !got.SessionID.IsZero() {
		t.Fatalf("%+v %v", got, err)
	}
	s.Attributes = []Attribute{{Tag: 1, Kind: KindString, Str: "hi"}, {Tag: 2, Kind: KindInt, Int: -7}}
	got, err = DecodeSession(s.Encode())
	if err != nil || len(got.Attributes) != 2 || got.Attributes[1].Int != -7 || got.Attributes[0].Str != "hi" {
		t.Fatalf("%+v %v", got, err)
	}

	// Malformed input is refused, never guessed at.
	bad := append([]byte(nil), raw...)
	bad[0]++ // length disagrees with the body
	for name, b := range map[string][]byte{
		"short":      raw[:SessionHeader-1],
		"length":     bad,
		"empty":      nil,
		"too many":   func() []byte { c := append([]byte(nil), raw...); c[68] = 200; return c }(),
		"offset out": func() []byte { c := s.Encode(); c[SessionHeader] = 0; return c }(),
	} {
		if _, err := DecodeSession(b); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

func TestFindByID(t *testing.T) {
	id := ID{0x80, 1, 2, 3, 4, 5, 6, 7}
	req := FindRequest(0xFFFE0000, Addr{}, id)
	got, err := DecodeSearch(req.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if fid, ok := got.FindID(); !ok || fid != id {
		t.Fatalf("find id %v %v", fid, ok)
	}
	got.ProcedureIndex = 1
	if _, ok := got.FindID(); ok {
		t.Fatal("only procedure 0xFFFFFFFF is a find by id")
	}
	req.Attributes[0].Int = 0
	if _, ok := req.FindID(); ok {
		t.Fatal("a zero id is not a session")
	}
}

func TestResultsRoundTrip(t *testing.T) {
	r := Result{SessionID: ID{9, 8, 7, 6, 5, 4, 3, 2}, Key: Key{1, 2, 3}, PrivateOpen: 15, PublicFilled: 1,
		Attributes: []Attribute{{Tag: 4, Kind: KindInt, Int: 1}}}
	r.Host[8] = 0x90
	raw := EncodeResults(0, []Result{r, r})
	got, err := DecodeResults(raw)
	if err != nil || len(got) != 2 || got[1].SessionID != r.SessionID || got[0].Key != r.Key || got[0].Host != r.Host || got[1].Attributes[0].Int != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	empty, err := DecodeResults(EncodeResults(0, nil))
	if err != nil || len(empty) != 0 {
		t.Fatalf("%v %v", empty, err)
	}
	if _, err := DecodeResults(raw[:len(raw)-1]); err == nil {
		t.Fatal("truncated results must be refused")
	}
}

func TestInfoAndDelete(t *testing.T) {
	i := Info{SessionID: ID{1}, Key: Key{2}}
	if len(i.Encode()) != 24 {
		t.Fatal("create reply is 24 bytes (RE)")
	}
	got, err := DecodeInfo(i.Encode())
	if err != nil || got != i {
		t.Fatal(got, err)
	}
	d := Delete{SessionID: ID{5, 6}, TitleID: 0xFFFE0000}
	raw := d.Encode()
	if len(raw) != 16 || raw[0] != 0x10 {
		t.Fatalf("%x", raw)
	}
	gd, err := DecodeDelete(raw)
	if err != nil || gd != d {
		t.Fatal(gd, err)
	}
	if _, err := DecodeDelete(raw[:15]); err == nil {
		t.Fatal("short delete refused")
	}
}
