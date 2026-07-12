package afp

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"

	"github.com/lex/goafp/internal/dsi"
)

// rfc5054Group2N is the 1536-bit safe prime from RFC 5054 Appendix A,
// which is exactly netatalk's srp_N_bytes.
const rfc5054Group2N = `
9DEF3CAF B939277A B1F12A86 17A47BBB DBA51DF4 99AC4C80 BEEEA961 4B19CC4D
5F4F5F55 6E27CBDE 51C6A94B E4607A29 15589903 BA0D0F84 380B655B B9A22E8D
CDF028A7 CEC67F0D 08134B1C B9798914 9B609E0B E3BAB63D 47548381 DBC5B1FC
764E3F4B 53DD9DA1 158BFD3E 2B9C8CF5 6EDF0195 39349627 DB2FD53D 24B7C486
65772E43 7D6C7F8C E442734A F7CCB7AE 837C264A E3A9BEB8 7F8A2FE9 B8B5292E
5A021FFF 5E91479E 8CE7A28C 2442C6F3 15180F93 499A234D CF76E3FE D135F9BB`

func srpReferenceN() []byte {
	b, err := hex.DecodeString(strings.Join(strings.Fields(rfc5054Group2N), ""))
	if err != nil {
		panic(err)
	}
	return b
}

// srpTestServer implements the server half of netatalk's SRP UAM (see
// etc/uams/uams_srp.c) so the client can be verified without a live server.
type srpTestServer struct {
	t        *testing.T
	nc       net.Conn
	username string
	password string

	N, g, v, b, B *big.Int
	salt          []byte
	width         int
}

func startSRPTestServer(t *testing.T, nc net.Conn, username, password string) *srpTestServer {
	t.Helper()
	nBytes := srpReferenceN()
	s := &srpTestServer{
		t: t, nc: nc, username: username, password: password,
		N:     new(big.Int).SetBytes(nBytes),
		g:     big.NewInt(2),
		width: len(nBytes),
		salt:  make([]byte, 16),
	}
	rand.Read(s.salt)
	// verifier v = g^x mod N, x = H(salt | H(user|:|pass))
	inner := sha1Sum([]byte(username), []byte(":"), []byte(password))
	x := new(big.Int).SetBytes(sha1Sum(s.salt, inner))
	s.v = new(big.Int).Exp(s.g, x, s.N)
	go s.loop()
	return s
}

func (s *srpTestServer) loop() {
	for {
		var hb [dsi.HeaderSize]byte
		if _, err := io.ReadFull(s.nc, hb[:]); err != nil {
			return
		}
		length := binary.BigEndian.Uint32(hb[8:12])
		payload := make([]byte, length)
		if _, err := io.ReadFull(s.nc, payload); err != nil {
			return
		}
		if hb[0] != 0 || hb[1] != dsi.CmdCommand {
			continue
		}
		reqID := binary.BigEndian.Uint16(hb[2:4])
		code, reply := s.handle(payload)
		s.send(reqID, code, reply)
	}
}

func (s *srpTestServer) send(reqID uint16, code Result, payload []byte) {
	var hb [dsi.HeaderSize]byte
	hb[0] = 1
	hb[1] = dsi.CmdCommand
	binary.BigEndian.PutUint16(hb[2:4], reqID)
	binary.BigEndian.PutUint32(hb[4:8], uint32(int32(code)))
	binary.BigEndian.PutUint32(hb[8:12], uint32(len(payload)))
	s.nc.Write(append(hb[:], payload...))
}

func (s *srpTestServer) handle(payload []byte) (Result, []byte) {
	switch payload[0] {
	case cmdLogin:
		_, rest := pascalParse(payload[1:]) // version
		uam, rest := pascalParse(rest)
		if uam != uamSRP {
			return ResBadUAM, nil
		}
		user, _ := pascalParse(rest)
		if user != s.username {
			return ResUserNotAuth, nil
		}
		return s.round1()
	case cmdLoginCont:
		return s.round2(payload[4:]) // skip command, pad, 2-byte id
	}
	return ResCallNotSupported, nil
}

func (s *srpTestServer) round1() (Result, []byte) {
	bBytes := make([]byte, s.width)
	rand.Read(bBytes)
	s.b = new(big.Int).Mod(new(big.Int).SetBytes(bBytes), s.N)
	k := new(big.Int).SetBytes(sha1Sum(leftPad(s.N, s.width), leftPad(s.g, s.width)))
	// B = (k*v + g^b) mod N
	gb := new(big.Int).Exp(s.g, s.b, s.N)
	kv := new(big.Int).Mul(k, s.v)
	s.B = new(big.Int).Mod(new(big.Int).Add(kv, gb), s.N)

	var w builder
	w.u16(0) // context
	w.u16(0x0002)
	w.u16(uint16(s.width))
	w.bytes(leftPad(s.N, s.width))
	w.u16(1)
	w.bytes(s.g.Bytes())
	w.u16(uint16(len(s.salt)))
	w.bytes(s.salt)
	w.u16(uint16(s.width))
	w.bytes(leftPad(s.B, s.width))
	return ResAuthContinue, w.b
}

func (s *srpTestServer) round2(auth []byte) (Result, []byte) {
	r := &reader{b: auth}
	if step := r.u16("step"); step != srpClientProof {
		s.t.Errorf("client sent step %#x, want client-proof", step)
		return ResParamErr, nil
	}
	A := new(big.Int).SetBytes(r.take(int(r.u16("A len")), "A"))
	m1 := r.take(int(r.u16("M1 len")), "M1")
	if r.err != nil {
		s.t.Errorf("parsing client proof: %v", r.err)
		return ResParamErr, nil
	}

	u := new(big.Int).SetBytes(sha1Sum(leftPad(A, s.width), leftPad(s.B, s.width)))
	// S = (A * v^u)^b mod N
	vu := new(big.Int).Exp(s.v, u, s.N)
	Avu := new(big.Int).Mul(A, vu)
	S := new(big.Int).Exp(Avu, s.b, s.N)
	K := mgf1SHA1(S.Bytes(), 40)

	hN := sha1Sum(s.N.Bytes())
	hg := sha1Sum(s.g.Bytes())
	xorNg := make([]byte, srpHashLen)
	for i := range xorNg {
		xorNg[i] = hN[i] ^ hg[i]
	}
	hUser := sha1Sum([]byte(s.username))
	m1Expected := sha1Sum(xorNg, hUser, s.salt, A.Bytes(), s.B.Bytes(), K)
	if string(m1) != string(m1Expected) {
		return SRPAuthFailure, nil
	}

	m2 := sha1Sum(A.Bytes(), m1, K)
	var w builder
	w.u16(srpServerProof)
	w.u16(srpHashLen)
	w.bytes(m2)
	return ResNoErr, w.b
}

// SRPAuthFailure is the code Apple/netatalk return for a bad SRP proof.
const SRPAuthFailure Result = -6754

func newSRPSession(t *testing.T, username, password string) *Session {
	t.Helper()
	clientEnd, serverEnd := net.Pipe()
	t.Cleanup(func() { serverEnd.Close() })
	startSRPTestServer(t, serverEnd, username, password)
	conn := dsi.NewConn(clientEnd)
	t.Cleanup(func() { conn.Close() })
	return NewSession(conn)
}

func srpServerInfo() *ServerInfo {
	return &ServerInfo{
		AFPVersions: []string{"AFP3.4"},
		UAMs:        []string{uamSRP},
	}
}

func TestLoginSRP(t *testing.T) {
	s := newSRPSession(t, "alice", "hunter2")
	if err := s.Login(testCtx(t), srpServerInfo(), "alice", "hunter2"); err != nil {
		t.Fatalf("SRP login: %v", err)
	}
}

func TestLoginSRPWrongPassword(t *testing.T) {
	s := newSRPSession(t, "alice", "hunter2")
	err := s.Login(testCtx(t), srpServerInfo(), "alice", "wrong")
	if err == nil {
		t.Fatal("SRP login with wrong password succeeded")
	}
	// The server rejects M1, so the client's own M2 check never passes;
	// either way it must be an error.
	var afpErr *Error
	if errors.As(err, &afpErr) && afpErr.Code != SRPAuthFailure {
		t.Logf("got AFP error %v (acceptable)", afpErr)
	}
}
