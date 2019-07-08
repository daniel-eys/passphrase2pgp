// This is free and unencumbered software released into the public domain.

package main

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"math/bits"
	"os"
	"syscall"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/ed25519"
	"golang.org/x/crypto/ssh/terminal"
)

// Returns data encoded as an OpenPGP multiprecision integer.
func mpi(data []byte) []byte {
	// Chop off leading zeros
	for len(data) > 0 && data[0] == 0 {
		data = data[1:]
	}
	// Zero-length is a special case (should never actually happen)
	if len(data) == 0 {
		return []byte{0, 0}
	}
	c := len(data)*8 - bits.LeadingZeros8(data[0])
	mpi := []byte{byte(c >> 8), byte(c >> 0)}
	return append(mpi, data...)
}

// Print the message like fmt.Printf() and then os.Exit(1).
func fatal(format string, args ...interface{}) {
	buf := bytes.NewBufferString("passphrase2pgp: ")
	fmt.Fprintf(buf, format, args...)
	buf.WriteRune('\n')
	os.Stderr.Write(buf.Bytes())
	os.Exit(1)
}

// Read, confirm, and return a passphrase from the user.
func readPassphrase(repeat int) ([]byte, error) {
	prompt := []byte("passphrase: ")
	tail := []byte("\n")
	os.Stderr.Write(prompt)
	passphrase, err := terminal.ReadPassword(int(syscall.Stdin))
	if err != nil {
		return nil, err
	}
	os.Stderr.Write(tail)
	for i := 0; i < repeat; i++ {
		os.Stderr.Write(prompt)
		again, err := terminal.ReadPassword(int(syscall.Stdin))
		if err != nil {
			return nil, err
		}
		os.Stderr.Write(tail)
		if !bytes.Equal(again, passphrase) {
			return nil, errors.New("passphrases do not match")
		}
	}
	return passphrase, nil
}

// Derive a 32-byte seed from the given passphrase.
func kdf(passphrase []byte, time uint32) []byte {
	var memory uint32 = 1024 * 1024 // 1 GB
	var threads uint8 = 1
	var seedSize uint32 = ed25519.SeedSize
	return argon2.IDKey(passphrase, nil, time, memory, threads, seedSize)
}

// Returns a Secret-Key Packet for a key pair.
func newSecretKeyPacket(seckey, pubkey []byte, created int64) []byte {
	packet := []byte{
		0xc5, // packet header, new format, Secret-Key Packet (5)
		0,    // packet length
		0x04, // packet version, new (4)

		// Public Key
		// creation date
		byte(created >> 24),
		byte(created >> 16),
		byte(created >> 8),
		byte(created >> 0),
		22, // algorithm, EdDSA
		9,  // OID length
		// OID (1.3.6.1.4.1.11591.15.1)
		0x2b, 0x06, 0x01, 0x04, 0x01, 0xda, 0x47, 0x0f, 0x01,
		// public key length (always 263 bits)
		0x01, 0x07,
		0x40, // MPI prefix
		// public key (32 bytes)
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,

		// Secret Key
		0, // string-to-key, unencrypted
	}
	copy(packet[21:53], pubkey)

	// append MPI-encoded key
	secmpi := mpi(seckey)
	packet = append(packet, secmpi...)

	// compute and append checksum
	var checksum uint16
	for _, b := range secmpi {
		checksum += uint16(b)
	}
	packet = append(packet, []byte{
		byte(checksum >> 8), byte(checksum >> 0),
	}...)

	packet[1] = byte(len(packet) - 2)
	return packet
}

// Returns a User ID Packet for the given identity.
func newUserIDPacket(uid string) []byte {
	return append([]byte{
		0xcd,           // packet header, new format, User ID Packet (13)
		byte(len(uid)), // packet length
	}, []byte(uid)...)
}

// Returns a Signature Packet binding a Secret-Key Packet and User ID Packet.
func newKeySignaturePacket(key ed25519.PrivateKey, skpacket, idpacket []byte, created int64) []byte {

	keyid := keyid(skpacket)
	sigpacket := []byte{
		0xc2,  // packet header, new format, Signature Packet (2)
		0,     // packet length
		0x04,  // packet version, new (4)
		0x13,  // signature type, Positive certification of a User ID
		22,    // public-key algorithm, EdDSA
		8,     // hash algorithm, SHA-256
		0, 16, // hashed subpacket data length
		// Signature Creation Time subpacket (length=5, type=2)
		5, 2,
		byte(created >> 24),
		byte(created >> 16),
		byte(created >> 8),
		byte(created >> 0),
		// Issuer subpacket (length=9, type=16)
		9, 16,
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, // no unhashed subpacket data
		0, 0, // hash value preview
	}

	// Fill out Issuer subpacket
	copy(sigpacket[16:24], keyid[12:])

	// Compute digest to be signed
	h := sha256.New()
	h.Write([]byte{0x99, 0, 51})
	h.Write(skpacket[2:53]) // public key portion
	h.Write([]byte{0xb4, 0, 0, 0})
	h.Write(idpacket[1:])
	h.Write(sigpacket[2:24])              // trailer
	h.Write([]byte{4, 0xff, 0, 0, 0, 22}) // final trailer
	sigsum := h.Sum(nil)
	sig := ed25519.Sign(key, sigsum)

	// Fill out hash preview
	sigpacket[26] = sigsum[0]
	sigpacket[27] = sigsum[1]

	// Fill out signature
	r := sig[:32]
	sigpacket = append(sigpacket, mpi(r)...)
	m := sig[32:]
	sigpacket = append(sigpacket, mpi(m)...)

	sigpacket[1] = byte(len(sigpacket)) - 2
	return sigpacket
}

func newSecretSubkeyPacket(seckey, pubkey []byte, created int64) []byte {
	packet := []byte{
		0xc7, // packet header, new format, Secret-Subkey Packet (7)
		0,    // packet length
		0x04, // packet version, new (4)

		// Public Key
		// creation date
		byte(created >> 24),
		byte(created >> 16),
		byte(created >> 8),
		byte(created >> 0),
		18, // algorithm, Elliptic Curve
		10, // OID length
		// OID (1.3.6.1.4.1.3029.1.5.1)
		0x2b, 0x06, 0x01, 0x04, 0x01, 0x97, 0x55, 0x01, 0x05, 0x01,
		// public key length (always 263 bits)
		0x01, 0x07,
		0x40, // MPI prefix
		// public key (32 bytes)
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		// KDF parameters
		3,    // length
		0x01, // reserved (1)
		0x08, // SHA-256
		0x07, // AES-128? (spec is incorrect)

		// Secret Key
		0, // string-to-key, unencrypted
	}
	copy(packet[22:54], pubkey)

	// append MPI-encoded key
	secmpi := mpi(reverse(seckey))
	packet = append(packet, secmpi...)

	// compute and append checksum
	var checksum uint16
	for _, b := range secmpi {
		checksum += uint16(b)
	}
	packet = append(packet, []byte{
		byte(checksum >> 8), byte(checksum >> 0),
	}...)

	packet[1] = byte(len(packet) - 2)
	return packet
}

// Returns the Key ID from a Secret-Key Packet.
func keyid(skpacket []byte) []byte {
	h := sha1.New()
	h.Write([]byte{0x99, 0, 51}) // "packet" length = 51
	h.Write(skpacket[2:53])      // public key portion
	return h.Sum(nil)
}

// Return the Curve25519 public key for a secret key.
func x25519(seckey []byte) []byte {
	var xpubkey [32]byte
	var xseckey [32]byte
	copy(xseckey[:], seckey)
	curve25519.ScalarBaseMult(&xpubkey, &xseckey)
	return xpubkey[:]
}

// Return a reversed copy.
func reverse(b []byte) []byte {
	c := make([]byte, len(b))
	for i, v := range b {
		c[len(c)-i-1] = v
	}
	return c
}

// Return a Curve25519 keypair from a seed.
func newCurve25519Keys(seed []byte) (seckey, pubkey []byte) {
	h := sha256.New()
	h.Write(seed)
	seckey = h.Sum(nil)
	seckey[0] &= 248
	seckey[31] &= 127
	seckey[31] |= 64
	pubkey = x25519(seckey)
	return
}

func newSubkeySignaturePacket(key ed25519.PrivateKey, skpacket, sskpacket []byte, created int64) []byte {
	keyid := keyid(skpacket)
	sigpacket := []byte{
		0xc2,  // packet header, new format, Signature Packet (2)
		0,     // packet length
		0x04,  // packet version, new (4)
		0x18,  // signature type, Subkey Binding Signature
		22,    // public-key algorithm, EdDSA
		8,     // hash algorithm, SHA-256
		0, 16, // hashed subpacket data length
		// Signature Creation Time subpacket (length=5, type=2)
		5, 2,
		byte(created >> 24),
		byte(created >> 16),
		byte(created >> 8),
		byte(created >> 0),
		// Issuer subpacket (length=9, type=16)
		9, 16,
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, // no unhashed subpacket data
		0, 0, // hash value preview
	}

	// Fill out Issuer subpacket
	copy(sigpacket[16:24], keyid[12:])

	// Compute digest to be signed
	h := sha256.New()
	h.Write([]byte{0x99, 0, 51})
	h.Write(skpacket[2:53]) // public key portion
	h.Write([]byte{0x99, 0, 56})
	h.Write(sskpacket[2:58])
	h.Write(sigpacket[2:24])              // trailer
	h.Write([]byte{4, 0xff, 0, 0, 0, 22}) // final trailer
	sigsum := h.Sum(nil)
	sig := ed25519.Sign(key, sigsum)

	// Fill out hash preview
	sigpacket[26] = sigsum[0]
	sigpacket[27] = sigsum[1]

	// Fill out signature
	r := sig[:32]
	sigpacket = append(sigpacket, mpi(r)...)
	m := sig[32:]
	sigpacket = append(sigpacket, mpi(m)...)

	sigpacket[1] = byte(len(sigpacket)) - 2
	return sigpacket
}

func main() {
	uid := flag.String("uid", "", "key user ID (required)")
	repeat := flag.Uint("repeat", 1, "number of repeated passphrase prompts")
	created := flag.Int64("date", 0, "creation date (unix epoch seconds)")
	paranoid := flag.Bool("paranoid", false, "paranoid mode")
	now := flag.Bool("now", false, "use current time as creation date")

	flag.Parse()

	if *uid == "" {
		fatal("missing User ID (-uid) option")
	}
	if *now {
		*created = time.Now().Unix()
	}

	// Derive a key from the passphrase
	passphrase, err := readPassphrase(int(*repeat))
	if err != nil {
		fatal("%s", err)
	}
	var time uint32
	if *paranoid {
		time = 32
	} else {
		time = 8
	}
	seed := kdf(passphrase, time)
	key := ed25519.NewKeyFromSeed(seed)
	seckey := key[:32]
	pubkey := key[32:]

	// Secret-Key Packet
	skpacket := newSecretKeyPacket(seckey, pubkey, *created)
	os.Stdout.Write(skpacket)

	// User ID Packet
	idpacket := newUserIDPacket(*uid)
	os.Stdout.Write(idpacket)

	// Signature Packet
	sigpacket := newKeySignaturePacket(key, skpacket, idpacket, *created)
	os.Stdout.Write(sigpacket)

	// Secret-Subkey Packet
	subseckey, subpubkey := newCurve25519Keys(seed)
	sskpacket := newSecretSubkeyPacket(subseckey, subpubkey, *created)
	os.Stdout.Write(sskpacket)

	ssigpacket := newSubkeySignaturePacket(key, skpacket, sskpacket, *created)
	os.Stdout.Write(ssigpacket)
}