package afp

import (
	"context"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"math/big"

	"golang.org/x/crypto/cast5"
)

// UAM names as they appear in server info.
const (
	uamNoAuth    = "No User Authent"
	uamCleartext = "Cleartxt Passwrd"
	uamDHX2      = "DHX2"
)

func (s *Session) loginGuest(ctx context.Context) error {
	_, code, err := s.login(ctx, uamNoAuth, nil)
	if err != nil {
		return err
	}
	return resultErr("guest login", code)
}

func (s *Session) loginCleartext(ctx context.Context, username, password string) error {
	var w builder
	w.pascal(username)
	// The 8-byte password field starts on an even boundary within the
	// FPLogin block. Account for the bytes that precede the auth info:
	// command byte plus the two Pascal strings.
	prefix := 1 + (1 + len(s.Version)) + (1 + len(uamCleartext))
	if (prefix+len(w.b))%2 == 1 {
		w.u8(0)
	}
	var pw [8]byte
	copy(pw[:], password)
	w.bytes(pw[:])

	_, code, err := s.login(ctx, uamCleartext, w.b)
	if err != nil {
		return err
	}
	return resultErr("cleartext login", code)
}

// DHX2 CAST5-CBC initialization vectors (shared with the DHX UAM).
var (
	dhxClientIV = []byte("LWallace")
	dhxServerIV = []byte("CJalbert")
)

// dhx2MaxKeyLen bounds the server-chosen DH modulus size (netatalk uses
// 64 bytes; Apple servers similar). Guards against absurd allocations.
const dhx2MaxKeyLen = 1024

const dhx2NonceLen = 16

// loginDHX2 performs the DHX2 user authentication method: a
// Diffie-Hellman exchange whose session key (MD5 of the shared secret)
// encrypts a nonce handshake and finally the password with CAST5-CBC.
// Layouts follow the AFP reference and the afpfs-ng/netatalk
// implementations.
func (s *Session) loginDHX2(ctx context.Context, username, password string) error {
	// Step 1: FPLogin with just the username; the server replies
	// AuthContinue with ID, g, length, p, and its public value Mb.
	var w builder
	w.pascal(username)
	payload, code, err := s.login(ctx, uamDHX2, w.b)
	if err != nil {
		return err
	}
	if code != ResAuthContinue {
		return resultErr("DHX2 login", code)
	}

	r := &reader{b: payload}
	id := r.u16("transaction id")
	g := new(big.Int).SetBytes(r.take(4, "g"))
	keyLen := int(r.u16("key length"))
	if r.err == nil && (keyLen == 0 || keyLen > dhx2MaxKeyLen) {
		return fmt.Errorf("afp: DHX2: unreasonable key length %d", keyLen)
	}
	p := new(big.Int).SetBytes(r.take(keyLen, "p"))
	mb := new(big.Int).SetBytes(r.take(keyLen, "Mb"))
	if r.err != nil {
		return r.err
	}
	if g.Sign() == 0 || p.BitLen() < 64 {
		return fmt.Errorf("afp: DHX2: degenerate DH parameters from server")
	}

	// Our secret Ra and public Ma = g^Ra mod p; shared K = Mb^Ra mod p.
	raBytes := make([]byte, keyLen)
	if _, err := rand.Read(raBytes); err != nil {
		return err
	}
	ra := new(big.Int).SetBytes(raBytes)
	ma := new(big.Int).Exp(g, ra, p)
	k := new(big.Int).Exp(mb, ra, p)

	// Session key: MD5 over K left-padded to the DH length.
	keyHash := md5.Sum(leftPad(k, keyLen))
	block, err := cast5.NewCipher(keyHash[:])
	if err != nil {
		return err
	}

	// Step 2: send Ma and our encrypted nonce.
	clientNonce := make([]byte, dhx2NonceLen)
	if _, err := rand.Read(clientNonce); err != nil {
		return err
	}
	step2 := make([]byte, 0, keyLen+dhx2NonceLen)
	step2 = append(step2, leftPad(ma, keyLen)...)
	step2 = append(step2, cbcEncrypt(block, dhxClientIV, clientNonce)...)

	payload, code, err = s.loginCont(ctx, id, step2)
	if err != nil {
		return err
	}
	if code != ResAuthContinue {
		return resultErr("DHX2 exchange", code)
	}

	// Reply: new ID, then E(clientNonce+1, serverNonce).
	r = &reader{b: payload}
	id = r.u16("transaction id")
	ct := r.take(dhx2NonceLen*2, "nonce block")
	if r.err != nil {
		return r.err
	}
	plain := cbcDecrypt(block, dhxServerIV, ct)

	wantNonce := incNonce(clientNonce)
	if subtle.ConstantTimeCompare(plain[:dhx2NonceLen], wantNonce) != 1 {
		return fmt.Errorf("afp: DHX2: server failed nonce check (wrong DH result?)")
	}
	serverNonce := plain[dhx2NonceLen:]

	// Step 3: send E(serverNonce+1, password zero-padded to 256).
	final := make([]byte, dhx2NonceLen+256)
	copy(final, incNonce(serverNonce))
	copy(final[dhx2NonceLen:], password)

	_, code, err = s.loginCont(ctx, id, cbcEncrypt(block, dhxClientIV, final))
	if err != nil {
		return err
	}
	return resultErr("DHX2 login", code)
}

// leftPad returns v as a big-endian buffer of exactly n bytes.
func leftPad(v *big.Int, n int) []byte {
	return v.FillBytes(make([]byte, n))
}

// incNonce returns nonce+1 as a fixed-width big-endian value.
func incNonce(nonce []byte) []byte {
	v := new(big.Int).SetBytes(nonce)
	v.Add(v, big.NewInt(1))
	out := make([]byte, len(nonce))
	// A wrap past 2^128 truncates, matching the reference behavior.
	b := v.Bytes()
	if len(b) > len(out) {
		b = b[len(b)-len(out):]
	}
	copy(out[len(out)-len(b):], b)
	return out
}

func cbcEncrypt(block cipher.Block, iv, plain []byte) []byte {
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, plain)
	return out
}

func cbcDecrypt(block cipher.Block, iv, ct []byte) []byte {
	out := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, ct)
	return out
}
