package afp

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/lex/goafp/internal/dsi"
	"golang.org/x/crypto/cast5"
)

// afpTestServer speaks just enough DSI+AFP to unit-test the login flows,
// including the server side of the DHX2 handshake.
type afpTestServer struct {
	t        *testing.T
	nc       net.Conn
	username string
	password string

	// DHX2 state.
	keyLen      int
	p, g, b     *big.Int
	key         [16]byte
	clientNonce []byte
	serverNonce []byte
	loginID     uint16
	step        int
}

func startAFPTestServer(t *testing.T, nc net.Conn, username, password string) *afpTestServer {
	t.Helper()
	s := &afpTestServer{
		t: t, nc: nc,
		username: username, password: password,
		keyLen:  64,
		loginID: 42,
	}
	go s.loop()
	return s
}

func (s *afpTestServer) loop() {
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
			continue // ignore CloseSession etc.
		}
		reqID := binary.BigEndian.Uint16(hb[2:4])
		code, reply := s.handle(payload)
		s.send(reqID, code, reply)
	}
}

func (s *afpTestServer) send(reqID uint16, code Result, payload []byte) {
	var hb [dsi.HeaderSize]byte
	hb[0] = 1 // reply
	hb[1] = dsi.CmdCommand
	binary.BigEndian.PutUint16(hb[2:4], reqID)
	binary.BigEndian.PutUint32(hb[4:8], uint32(int32(code)))
	binary.BigEndian.PutUint32(hb[8:12], uint32(len(payload)))
	s.nc.Write(append(hb[:], payload...))
}

func pascalParse(b []byte) (string, []byte) {
	if len(b) == 0 {
		return "", nil
	}
	n := int(b[0])
	if 1+n > len(b) {
		return "", nil
	}
	return string(b[1 : 1+n]), b[1+n:]
}

func (s *afpTestServer) handle(payload []byte) (Result, []byte) {
	switch payload[0] {
	case cmdLogin:
		_, rest := pascalParse(payload[1:]) // AFP version
		uam, rest := pascalParse(rest)
		switch uam {
		case uamNoAuth:
			return ResNoErr, nil
		case uamDHX2:
			user, _ := pascalParse(rest)
			if user != s.username {
				return ResUserNotAuth, nil
			}
			return s.dhx2Step1()
		default:
			return ResBadUAM, nil
		}
	case cmdLoginCont:
		id := binary.BigEndian.Uint16(payload[2:4])
		if id != s.loginID {
			s.t.Errorf("login cont with id %d, want %d", id, s.loginID)
			return ResParamErr, nil
		}
		auth := payload[4:]
		if s.step == 1 {
			return s.dhx2Step2(auth)
		}
		return s.dhx2Step3(auth)
	case cmdLogout:
		return ResNoErr, nil
	case cmdGetSrvrParms:
		var w builder
		w.u32(0) // server time
		w.u8(2)
		w.u8(0x80) // has password
		w.pascal("Secret")
		w.u8(0)
		w.pascal("Public")
		return ResNoErr, w.b
	}
	return ResCallNotSupported, nil
}

func (s *afpTestServer) dhx2Step1() (Result, []byte) {
	var err error
	s.p, err = rand.Prime(rand.Reader, 8*s.keyLen)
	if err != nil {
		s.t.Fatal(err)
	}
	s.g = big.NewInt(5)
	bBytes := make([]byte, s.keyLen)
	rand.Read(bBytes)
	s.b = new(big.Int).SetBytes(bBytes)
	mb := new(big.Int).Exp(s.g, s.b, s.p)

	var w builder
	w.u16(s.loginID)
	w.bytes(leftPad(s.g, 4))
	w.u16(uint16(s.keyLen))
	w.bytes(leftPad(s.p, s.keyLen))
	w.bytes(leftPad(mb, s.keyLen))
	s.step = 1
	return ResAuthContinue, w.b
}

func (s *afpTestServer) dhx2Step2(auth []byte) (Result, []byte) {
	if len(auth) != s.keyLen+dhx2NonceLen {
		s.t.Errorf("step2 auth block is %d bytes, want %d", len(auth), s.keyLen+dhx2NonceLen)
		return ResParamErr, nil
	}
	ma := new(big.Int).SetBytes(auth[:s.keyLen])
	k := new(big.Int).Exp(ma, s.b, s.p)
	s.key = md5.Sum(leftPad(k, s.keyLen))
	block, _ := cast5.NewCipher(s.key[:])

	s.clientNonce = cbcDecrypt(block, dhxClientIV, auth[s.keyLen:])
	s.serverNonce = make([]byte, dhx2NonceLen)
	rand.Read(s.serverNonce)

	plain := append(incNonce(s.clientNonce), s.serverNonce...)
	s.step = 2
	return ResAuthContinue, append(
		binary.BigEndian.AppendUint16(nil, s.loginID),
		cbcEncrypt(block, dhxServerIV, plain)...)
}

func (s *afpTestServer) dhx2Step3(auth []byte) (Result, []byte) {
	block, _ := cast5.NewCipher(s.key[:])
	plain := cbcDecrypt(block, dhxClientIV, auth)
	if len(plain) != dhx2NonceLen+256 {
		s.t.Errorf("step3 plaintext is %d bytes, want %d", len(plain), dhx2NonceLen+256)
		return ResParamErr, nil
	}
	if !bytes.Equal(plain[:dhx2NonceLen], incNonce(s.serverNonce)) {
		s.t.Error("client failed server-nonce check")
		return ResUserNotAuth, nil
	}
	pw := bytes.TrimRight(plain[dhx2NonceLen:], "\x00")
	if string(pw) != s.password {
		return ResUserNotAuth, nil
	}
	return ResNoErr, nil
}

func newTestSession(t *testing.T, username, password string) *Session {
	t.Helper()
	clientEnd, serverEnd := net.Pipe()
	t.Cleanup(func() { serverEnd.Close() })
	startAFPTestServer(t, serverEnd, username, password)
	conn := dsi.NewConn(clientEnd)
	t.Cleanup(func() { conn.Close() })
	return NewSession(conn)
}

func testServerInfo() *ServerInfo {
	return &ServerInfo{
		AFPVersions: []string{"AFP2.2", "AFP3.1", "AFP3.3"},
		UAMs:        []string{uamNoAuth, uamDHX2},
	}
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestLoginGuest(t *testing.T) {
	s := newTestSession(t, "", "")
	if err := s.Login(testCtx(t), testServerInfo(), "", ""); err != nil {
		t.Fatalf("guest login: %v", err)
	}
	if s.Version != "AFP3.3" {
		t.Errorf("negotiated version %q, want AFP3.3", s.Version)
	}
}

func TestLoginDHX2(t *testing.T) {
	s := newTestSession(t, "alice", "s3cret")
	if err := s.Login(testCtx(t), testServerInfo(), "alice", "s3cret"); err != nil {
		t.Fatalf("DHX2 login: %v", err)
	}
	if err := s.Logout(testCtx(t)); err != nil {
		t.Fatalf("logout: %v", err)
	}
}

func TestLoginDHX2WrongPassword(t *testing.T) {
	s := newTestSession(t, "alice", "s3cret")
	err := s.Login(testCtx(t), testServerInfo(), "alice", "wrong")
	var afpErr *Error
	if err == nil {
		t.Fatal("login with wrong password succeeded")
	}
	if !errors.As(err, &afpErr) || afpErr.Code != ResUserNotAuth {
		t.Fatalf("login error = %v, want user-not-authenticated", err)
	}
}

func TestListVolumes(t *testing.T) {
	s := newTestSession(t, "", "")
	if err := s.Login(testCtx(t), testServerInfo(), "", ""); err != nil {
		t.Fatal(err)
	}
	vols, err := s.ListVolumes(testCtx(t))
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(vols) != 2 || vols[0].Name != "Public" || vols[1].Name != "Secret" {
		t.Errorf("volumes = %+v", vols)
	}
	if vols[0].HasPassword || !vols[1].HasPassword {
		t.Errorf("password flags wrong: %+v", vols)
	}
}

func TestPickVersion(t *testing.T) {
	if _, err := pickVersion([]string{"AFPVersion 2.1", "AFP2.2"}); err == nil {
		t.Error("want error for AFP 2.x-only server")
	}
	v, err := pickVersion([]string{"AFP3.1", "AFP3.4", "AFP3.2"})
	if err != nil || v != "AFP3.4" {
		t.Errorf("pickVersion = %q, %v", v, err)
	}
}
