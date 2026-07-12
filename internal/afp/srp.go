package afp

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"fmt"
	"math/big"
)

// SRP UAM step markers (in the AFP payload, after the transaction context).
const (
	srpClientProof = 0x0003
	srpServerProof = 0x0004
)

const srpHashLen = sha1.Size // 20

// loginSRP performs the SRP-6a user authentication method as implemented
// by netatalk: SHA-1 hashes, an MGF1 key-derivation function, and the DH
// group advertised by the server (RFC 5054 group 2 in practice). The
// group parameters (N, g), salt, and the server's public value B all
// arrive in the first reply, so this code adapts to whatever prime the
// server chooses.
func (s *Session) loginSRP(ctx context.Context, username, password string) error {
	// Step 1: FPLogin with the username; the server replies with the SRP
	// parameters and its public ephemeral B.
	var w builder
	w.pascal(username)
	payload, code, err := s.login(ctx, uamSRP, w.b)
	if err != nil {
		return err
	}
	if code != ResAuthContinue {
		return resultErr("SRP login", code)
	}

	r := &reader{b: payload}
	context16 := r.u16("srp context")
	r.u16("srp group index")
	nBytes := r.take(int(r.u16("N length")), "N")
	gBytes := r.take(int(r.u16("g length")), "g")
	salt := append([]byte(nil), r.take(int(r.u16("salt length")), "salt")...)
	bBytes := r.take(int(r.u16("B length")), "B")
	if r.err != nil {
		return r.err
	}

	N := new(big.Int).SetBytes(nBytes)
	g := new(big.Int).SetBytes(gBytes)
	B := new(big.Int).SetBytes(bBytes)
	width := len(nBytes) // pad width for PAD() operations
	if N.Sign() == 0 || B.Sign() == 0 {
		return fmt.Errorf("afp: SRP: degenerate parameters from server")
	}

	// Our secret a and public A = g^a mod N.
	aBytes := make([]byte, width)
	if _, err := rand.Read(aBytes); err != nil {
		return err
	}
	a := new(big.Int).Mod(new(big.Int).SetBytes(aBytes), N)
	A := new(big.Int).Exp(g, a, N)

	// x = H(salt | H(username | ":" | password))
	inner := sha1Sum([]byte(username), []byte(":"), []byte(password))
	x := new(big.Int).SetBytes(sha1Sum(salt, inner))

	// k = H(N | PAD(g))
	k := new(big.Int).SetBytes(sha1Sum(nBytes, leftPad(g, width)))

	// u = H(PAD(A) | PAD(B))
	u := new(big.Int).SetBytes(sha1Sum(leftPad(A, width), leftPad(B, width)))
	if u.Sign() == 0 {
		return fmt.Errorf("afp: SRP: u == 0")
	}

	// S = (B - k*g^x)^(a + u*x) mod N
	gx := new(big.Int).Exp(g, x, N)
	kgx := new(big.Int).Mul(k, gx)
	base := new(big.Int).Sub(B, kgx)
	base.Mod(base, N) // math/big Mod is non-negative for positive N
	exp := new(big.Int).Add(a, new(big.Int).Mul(u, x))
	S := new(big.Int).Exp(base, exp, N)

	// K = MGF1-SHA1(strip(S), 40)
	K := mgf1SHA1(S.Bytes(), 40)

	// M1 = H( (H(N) xor H(g)) | H(username) | salt | strip(A) | strip(B) | K )
	hN := sha1Sum(N.Bytes())
	hg := sha1Sum(g.Bytes())
	xorNg := make([]byte, srpHashLen)
	for i := range xorNg {
		xorNg[i] = hN[i] ^ hg[i]
	}
	hUser := sha1Sum([]byte(username))
	m1 := sha1Sum(xorNg, hUser, salt, A.Bytes(), B.Bytes(), K)

	// Step 2: send A and our proof M1.
	var w2 builder
	w2.u16(srpClientProof)
	w2.u16(uint16(len(A.Bytes())))
	w2.bytes(A.Bytes())
	w2.u16(srpHashLen)
	w2.bytes(m1)

	payload, code, err = s.loginCont(ctx, context16, w2.b)
	if err != nil {
		return err
	}
	if err := resultErr("SRP login", code); err != nil {
		return err
	}

	// Verify the server's proof M2 = H(strip(A) | M1 | K).
	r = &reader{b: payload}
	if step := r.u16("srp step"); step != srpServerProof {
		return fmt.Errorf("afp: SRP: unexpected final step marker %#04x", step)
	}
	m2 := r.take(int(r.u16("M2 length")), "M2")
	if r.err != nil {
		return r.err
	}
	expected := sha1Sum(A.Bytes(), m1, K)
	if subtle.ConstantTimeCompare(m2, expected) != 1 {
		return fmt.Errorf("afp: SRP: server proof (M2) mismatch")
	}
	return nil
}

// sha1Sum returns SHA-1 over the concatenation of parts.
func sha1Sum(parts ...[]byte) []byte {
	h := sha1.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// mgf1SHA1 is the PKCS#1 MGF1 mask-generation function with SHA-1.
func mgf1SHA1(seed []byte, outLen int) []byte {
	out := make([]byte, 0, outLen)
	var counter [4]byte
	for c := uint32(0); len(out) < outLen; c++ {
		counter[0] = byte(c >> 24)
		counter[1] = byte(c >> 16)
		counter[2] = byte(c >> 8)
		counter[3] = byte(c)
		block := sha1Sum(seed, counter[:])
		out = append(out, block...)
	}
	return out[:outLen]
}
