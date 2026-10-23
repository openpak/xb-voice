// SPDX-License-Identifier: AGPL-3.0-only

// Package xmatch is the codec of native service 6 (matchmaking, "xmatch"):
// POST /xmatch/xmatchhost.srf (create or update a session), xmatchclient.srf
// (search, and find-by-session-id) and xmatchhostdelete.srf, all
// Content-Type xon/6. The layouts follow the public Xombie codec and were
// cross-checked against the dashboard (internal research notes section 3.2).
// All integers are little endian. Decoding is strict and bounded: a malformed
// body is an error, never a guess.
package xmatch

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	ContentType = "xon/6"

	AddrLen       = 36
	IDLen         = 8
	KeyLen        = 16
	SessionHeader = 72
	SearchHeader  = 56
	ResultHeader  = 84 // 4 + 8 + 36 + 16 + 4*5
	ResultsHeader = 12
	DeleteLen     = 16
	MaxAttributes = 64
	MaxBody       = 8192
	FindByIDProc  = 0xFFFFFFFF // procedure_index the dashboard uses for XOnlineMatchSessionFindFromID (RE)
	attrTypeMask  = 0x00F00000
	attrTagMask   = 0x0000FFFF
	attrInt       = 0x00000000
	attrString    = 0x00100000
	attrBlob      = 0x00200000
	attrNull      = 0x00F00000
)

var ErrMalformed = errors.New("xmatch: malformed message")

// ID is a session id (XNKID).
type ID [IDLen]byte

// Key is the per-session key handed to every member (the "key exchange key").
type Key [KeyLen]byte

func (id ID) IsZero() bool { return id == ID{} }

// Addr is an XNADDR: LAN address, the public address the secure gateway saw,
// its port, the MAC and the machine's online id block.
type Addr [AddrLen]byte

func (a Addr) Ina() [4]byte       { return [4]byte(a[0:4]) }
func (a Addr) InaOnline() [4]byte { return [4]byte(a[4:8]) }
func (a Addr) PortOnline() uint16 { return binary.LittleEndian.Uint16(a[8:10]) }

// Redacted zeroes the MAC and the machine block, which identify a console, for logs.
func (a Addr) Redacted() Addr {
	for i := 10; i < AddrLen; i++ {
		a[i] = 0
	}
	return a
}

// Kind is an attribute's value type.
type Kind uint8

const (
	KindInt Kind = iota
	KindString
	KindBlob
	KindNull
)

// Attribute is one tagged value. The tag is the low 16 bits of its type word.
type Attribute struct {
	Tag  uint16
	Kind Kind
	Int  int64
	Str  string // without the NUL
	Blob []byte
}

func (a Attribute) encodedLen() int {
	switch a.Kind {
	case KindInt:
		return 12
	case KindString:
		return 4 + 2 + len(a.Str) + 1
	case KindBlob:
		return 4 + 2 + len(a.Blob)
	}
	return 4
}

func (a Attribute) appendTo(b []byte) []byte {
	switch a.Kind {
	case KindInt:
		b = binary.LittleEndian.AppendUint32(b, attrInt|uint32(a.Tag))
		return binary.LittleEndian.AppendUint64(b, uint64(a.Int))
	case KindString:
		b = binary.LittleEndian.AppendUint32(b, attrString|uint32(a.Tag))
		b = binary.LittleEndian.AppendUint16(b, uint16(len(a.Str)+1))
		b = append(b, a.Str...)
		return append(b, 0)
	case KindBlob:
		b = binary.LittleEndian.AppendUint32(b, attrBlob|uint32(a.Tag))
		b = binary.LittleEndian.AppendUint16(b, uint16(len(a.Blob)))
		return append(b, a.Blob...)
	}
	return binary.LittleEndian.AppendUint32(b, attrNull|uint32(a.Tag))
}

func decodeAttribute(b []byte) (Attribute, error) {
	if len(b) < 4 {
		return Attribute{}, ErrMalformed
	}
	w := binary.LittleEndian.Uint32(b)
	a := Attribute{Tag: uint16(w & attrTagMask)}
	rest := b[4:]
	switch w & attrTypeMask {
	case attrInt:
		if len(rest) != 8 {
			return a, ErrMalformed
		}
		a.Kind, a.Int = KindInt, int64(binary.LittleEndian.Uint64(rest))
	case attrString:
		if len(rest) < 2 {
			return a, ErrMalformed
		}
		n := int(binary.LittleEndian.Uint16(rest))
		if n < 1 || len(rest)-2 != n || rest[len(rest)-1] != 0 {
			return a, ErrMalformed
		}
		a.Kind, a.Str = KindString, string(rest[2:len(rest)-1])
	case attrBlob:
		if len(rest) < 2 || int(binary.LittleEndian.Uint16(rest)) != len(rest)-2 {
			return a, ErrMalformed
		}
		a.Kind, a.Blob = KindBlob, append([]byte(nil), rest[2:]...)
	case attrNull:
		if len(rest) != 0 {
			return a, ErrMalformed
		}
		a.Kind = KindNull
	default:
		return a, ErrMalformed
	}
	return a, nil
}

// decodeAttributes reads the offset table and the attributes that follow a
// header of base bytes. total is the message's own total length.
func decodeAttributes(b []byte, base, n, total int) ([]Attribute, error) {
	if n < 0 || n > MaxAttributes {
		return nil, fmt.Errorf("%w: %d attributes", ErrMalformed, n)
	}
	start := base + 4*n
	if total < start || total > len(b) {
		return nil, ErrMalformed
	}
	offs := make([]int, n)
	for i := range offs {
		offs[i] = int(binary.LittleEndian.Uint32(b[base+4*i:]))
		if offs[i] < start || offs[i] > total || (i > 0 && offs[i] < offs[i-1]) {
			return nil, ErrMalformed
		}
	}
	out := make([]Attribute, n)
	for i := range offs {
		end := total
		if i+1 < n {
			end = offs[i+1]
		}
		a, err := decodeAttribute(b[offs[i]:end])
		if err != nil {
			return nil, err
		}
		out[i] = a
	}
	return out, nil
}

func appendAttributes(b []byte, base int, attrs []Attribute) []byte {
	off := base + 4*len(attrs)
	for _, a := range attrs {
		b = binary.LittleEndian.AppendUint32(b, uint32(off))
		off += a.encodedLen()
	}
	for _, a := range attrs {
		b = a.appendTo(b)
	}
	return b
}

// Session is the body of xmatchhost.srf. A zero SessionID creates.
type Session struct {
	SessionID                                            ID
	TitleID                                              uint32
	Host                                                 Addr
	PublicOpen, PrivateOpen, PublicFilled, PrivateFilled uint32
	Attributes                                           []Attribute
}

func DecodeSession(b []byte) (Session, error) {
	var s Session
	if len(b) < SessionHeader || len(b) > MaxBody {
		return s, ErrMalformed
	}
	total := int(binary.LittleEndian.Uint32(b))
	copy(s.SessionID[:], b[4:12])
	s.TitleID = binary.LittleEndian.Uint32(b[12:])
	copy(s.Host[:], b[16:16+AddrLen])
	s.PublicOpen = binary.LittleEndian.Uint32(b[52:])
	s.PrivateOpen = binary.LittleEndian.Uint32(b[56:])
	s.PublicFilled = binary.LittleEndian.Uint32(b[60:])
	s.PrivateFilled = binary.LittleEndian.Uint32(b[64:])
	n := int(binary.LittleEndian.Uint32(b[68:]))
	if total != len(b) {
		return s, fmt.Errorf("%w: length %d, body %d", ErrMalformed, total, len(b))
	}
	var err error
	s.Attributes, err = decodeAttributes(b, SessionHeader, n, total)
	return s, err
}

func (s Session) Encode() []byte {
	n := SessionHeader + 4*len(s.Attributes)
	for _, a := range s.Attributes {
		n += a.encodedLen()
	}
	b := binary.LittleEndian.AppendUint32(make([]byte, 0, n), uint32(n))
	b = append(b, s.SessionID[:]...)
	b = binary.LittleEndian.AppendUint32(b, s.TitleID)
	b = append(b, s.Host[:]...)
	for _, v := range [...]uint32{s.PublicOpen, s.PrivateOpen, s.PublicFilled, s.PrivateFilled, uint32(len(s.Attributes))} {
		b = binary.LittleEndian.AppendUint32(b, v)
	}
	return appendAttributes(b, SessionHeader, s.Attributes)
}

// Search is the body of xmatchclient.srf.
type Search struct {
	TitleID        uint32
	ProcedureIndex uint32
	Client         Addr
	NumUsers       uint16
	Flags          uint16
	Attributes     []Attribute
}

func DecodeSearch(b []byte) (Search, error) {
	var s Search
	if len(b) < SearchHeader || len(b) > MaxBody {
		return s, ErrMalformed
	}
	total := int(binary.LittleEndian.Uint32(b))
	s.TitleID = binary.LittleEndian.Uint32(b[4:])
	s.ProcedureIndex = binary.LittleEndian.Uint32(b[8:])
	copy(s.Client[:], b[12:12+AddrLen])
	s.NumUsers = binary.LittleEndian.Uint16(b[48:])
	s.Flags = binary.LittleEndian.Uint16(b[50:])
	n := int(binary.LittleEndian.Uint32(b[52:]))
	if total != len(b) {
		return s, fmt.Errorf("%w: length %d, body %d", ErrMalformed, total, len(b))
	}
	var err error
	s.Attributes, err = decodeAttributes(b, SearchHeader, n, total)
	return s, err
}

func (s Search) Encode() []byte {
	n := SearchHeader + 4*len(s.Attributes)
	for _, a := range s.Attributes {
		n += a.encodedLen()
	}
	b := binary.LittleEndian.AppendUint32(make([]byte, 0, n), uint32(n))
	b = binary.LittleEndian.AppendUint32(b, s.TitleID)
	b = binary.LittleEndian.AppendUint32(b, s.ProcedureIndex)
	b = append(b, s.Client[:]...)
	b = binary.LittleEndian.AppendUint16(b, s.NumUsers)
	b = binary.LittleEndian.AppendUint16(b, s.Flags)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(s.Attributes)))
	return appendAttributes(b, SearchHeader, s.Attributes)
}

// FindID reports the session id of a find-by-id search: procedure 0xFFFFFFFF
// with exactly one integer attribute (tag 0) whose 8 bytes are the id (RE of the
// dashboard's XOnlineMatchSessionFindFromID request builder).
func (s Search) FindID() (ID, bool) {
	if s.ProcedureIndex != FindByIDProc || len(s.Attributes) != 1 || s.Attributes[0].Kind != KindInt || s.Attributes[0].Tag != 0 {
		return ID{}, false
	}
	var id ID
	binary.LittleEndian.PutUint64(id[:], uint64(s.Attributes[0].Int))
	return id, !id.IsZero()
}

// FindRequest builds the find-by-id search a console sends (tests and xb-e2e).
func FindRequest(title uint32, client Addr, id ID) Search {
	return Search{TitleID: title, ProcedureIndex: FindByIDProc, Client: client, NumUsers: 1,
		Attributes: []Attribute{{Kind: KindInt, Int: int64(binary.LittleEndian.Uint64(id[:]))}}}
}

// Info is the reply to xmatchhost.srf: the session id and the key.
type Info struct {
	SessionID ID
	Key       Key
}

func (i Info) Encode() []byte {
	return append(append(make([]byte, 0, 24), i.SessionID[:]...), i.Key[:]...)
}

func DecodeInfo(b []byte) (Info, error) {
	var i Info
	if len(b) != IDLen+KeyLen {
		return i, ErrMalformed
	}
	copy(i.SessionID[:], b)
	copy(i.Key[:], b[IDLen:])
	return i, nil
}

// Result is one session in a search reply.
type Result struct {
	SessionID                                            ID
	Host                                                 Addr
	Key                                                  Key
	PublicOpen, PrivateOpen, PublicFilled, PrivateFilled uint32
	Attributes                                           []Attribute
}

// EncodeResults builds the reply to xmatchclient.srf.
func EncodeResults(flags uint16, rs []Result) []byte {
	var body []byte
	for _, r := range rs {
		n := ResultHeader + 4*len(r.Attributes)
		for _, a := range r.Attributes {
			n += a.encodedLen()
		}
		body = binary.LittleEndian.AppendUint32(body, uint32(n))
		body = append(body, r.SessionID[:]...)
		body = append(body, r.Host[:]...)
		body = append(body, r.Key[:]...)
		for _, v := range [...]uint32{r.PublicOpen, r.PrivateOpen, r.PublicFilled, r.PrivateFilled, uint32(len(r.Attributes))} {
			body = binary.LittleEndian.AppendUint32(body, v)
		}
		body = appendAttributes(body, ResultHeader, r.Attributes)
	}
	b := binary.LittleEndian.AppendUint32(nil, uint32(ResultsHeader+len(body)))
	b = binary.LittleEndian.AppendUint16(b, uint16(len(rs)))
	b = binary.LittleEndian.AppendUint16(b, flags)
	b = binary.LittleEndian.AppendUint32(b, 0) // logging threshold
	return append(b, body...)
}

// DecodeResults parses a search reply (used by tests and the console-side client in xb-e2e).
func DecodeResults(b []byte) ([]Result, error) {
	if len(b) < ResultsHeader {
		return nil, ErrMalformed
	}
	total := int(binary.LittleEndian.Uint32(b))
	count := int(binary.LittleEndian.Uint16(b[4:]))
	if total != len(b) || count > 64 {
		return nil, ErrMalformed
	}
	rest := b[ResultsHeader:]
	var out []Result
	for i := 0; i < count; i++ {
		if len(rest) < ResultHeader {
			return nil, ErrMalformed
		}
		n := int(binary.LittleEndian.Uint32(rest))
		if n < ResultHeader || n > len(rest) {
			return nil, ErrMalformed
		}
		r := Result{}
		copy(r.SessionID[:], rest[4:12])
		copy(r.Host[:], rest[12:12+AddrLen])
		copy(r.Key[:], rest[48:64])
		r.PublicOpen = binary.LittleEndian.Uint32(rest[64:])
		r.PrivateOpen = binary.LittleEndian.Uint32(rest[68:])
		r.PublicFilled = binary.LittleEndian.Uint32(rest[72:])
		r.PrivateFilled = binary.LittleEndian.Uint32(rest[76:])
		na := int(binary.LittleEndian.Uint32(rest[80:]))
		var err error
		if r.Attributes, err = decodeAttributes(rest[:n], ResultHeader, na, n); err != nil {
			return nil, err
		}
		out = append(out, r)
		rest = rest[n:]
	}
	if len(rest) != 0 {
		return nil, ErrMalformed
	}
	return out, nil
}

// Delete is the body of xmatchhostdelete.srf (RE, FUN_0011aebe).
type Delete struct {
	SessionID ID
	TitleID   uint32
}

func DecodeDelete(b []byte) (Delete, error) {
	var d Delete
	if len(b) != DeleteLen || binary.LittleEndian.Uint32(b) != DeleteLen {
		return d, ErrMalformed
	}
	copy(d.SessionID[:], b[4:12])
	d.TitleID = binary.LittleEndian.Uint32(b[12:])
	return d, nil
}

func (d Delete) Encode() []byte {
	b := binary.LittleEndian.AppendUint32(nil, DeleteLen)
	b = append(b, d.SessionID[:]...)
	return binary.LittleEndian.AppendUint32(b, d.TitleID)
}
