package roborock

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/binary"
	"fmt"
)

// miIO binary protocol constants.
const (
	miioMagic      = uint16(0x2131)
	miioHeaderSize = 32
)

// frame is a decoded miIO protocol frame.
type frame struct {
	deviceID uint32
	stamp    uint32 // device's own timestamp, NOT system clock
	payload  []byte // decrypted JSON
}

// keys holds the AES key, IV, and raw token derived from a device token.
type keys struct {
	key   []byte // md5(token)     — AES-128 key
	iv    []byte // md5(key+token) — AES CBC IV
	token []byte // raw 16-byte token — used verbatim in checksum
}

// deriveKeys computes the AES key and IV from a raw 16-byte token.
func deriveKeys(token []byte) keys {
	k := md5sum(token)
	iv := md5sum(append(k, token...))
	// Keep a copy of the raw token: miIO checksum = MD5(header + raw_token + payload).
	raw := make([]byte, len(token))
	copy(raw, token)
	return keys{key: k, iv: iv, token: raw}
}

func md5sum(b []byte) []byte {
	h := md5.Sum(b)
	return h[:]
}

// encode builds a miIO binary frame ready to send over UDP.
func encode(f frame, k keys) ([]byte, error) {
	encrypted, err := aesEncrypt(k.key, k.iv, f.payload)
	if err != nil {
		return nil, err
	}

	totalLen := miioHeaderSize + len(encrypted)
	buf := make([]byte, totalLen)

	binary.BigEndian.PutUint16(buf[0:2], miioMagic)
	binary.BigEndian.PutUint16(buf[2:4], uint16(totalLen))
	binary.BigEndian.PutUint32(buf[4:8], 0) // unknown field
	binary.BigEndian.PutUint32(buf[8:12], f.deviceID)
	binary.BigEndian.PutUint32(buf[12:16], f.stamp)
	// Checksum bytes 16-31: initially zero for calculation.
	copy(buf[32:], encrypted)

	checksum := computeChecksum(buf, k.token, encrypted)
	copy(buf[16:32], checksum)

	return buf, nil
}

// decode parses and decrypts a received miIO frame.
func decode(buf []byte, k keys) (frame, error) {
	if len(buf) < miioHeaderSize {
		return frame{}, fmt.Errorf("miio: frame too short (%d bytes)", len(buf))
	}
	if binary.BigEndian.Uint16(buf[0:2]) != miioMagic {
		return frame{}, fmt.Errorf("miio: bad magic 0x%04x", binary.BigEndian.Uint16(buf[0:2]))
	}
	totalLen := int(binary.BigEndian.Uint16(buf[2:4]))
	if len(buf) < totalLen {
		return frame{}, fmt.Errorf("miio: frame truncated (got %d, want %d)", len(buf), totalLen)
	}

	f := frame{
		deviceID: binary.BigEndian.Uint32(buf[8:12]),
		stamp:    binary.BigEndian.Uint32(buf[12:16]),
	}

	if totalLen == miioHeaderSize {
		// Hello / handshake response — no payload.
		return f, nil
	}

	encrypted := buf[miioHeaderSize:totalLen]

	// Verify checksum.
	wantChecksum := make([]byte, 16)
	copy(wantChecksum, buf[16:32])
	// Zero the checksum field in buf for verification.
	for i := 16; i < 32; i++ {
		buf[i] = 0
	}
	gotChecksum := computeChecksum(buf, k.token, encrypted)
	for i := 16; i < 32; i++ {
		buf[i] = wantChecksum[i-16]
	}
	for i := range gotChecksum {
		if gotChecksum[i] != wantChecksum[i] {
			return frame{}, fmt.Errorf("miio: checksum mismatch")
		}
	}

	var err error
	f.payload, err = aesDecrypt(k.key, k.iv, encrypted)
	if err != nil {
		return frame{}, fmt.Errorf("miio: decrypt: %w", err)
	}
	return f, nil
}

// helloPacket returns the 32-byte handshake packet.
func helloPacket() []byte {
	pkt := make([]byte, miioHeaderSize)
	binary.BigEndian.PutUint16(pkt[0:2], miioMagic)
	binary.BigEndian.PutUint16(pkt[2:4], miioHeaderSize)
	// All remaining bytes are 0xFF per spec.
	for i := 4; i < miioHeaderSize; i++ {
		pkt[i] = 0xFF
	}
	return pkt
}

// computeChecksum returns MD5(header_with_zeroed_checksum + token + payload).
func computeChecksum(header, token, payload []byte) []byte {
	h := md5.New()
	h.Write(header[:miioHeaderSize])
	h.Write(token)
	h.Write(payload)
	return h.Sum(nil)
}

// aesEncrypt encrypts plaintext with AES-128-CBC and PKCS7 padding.
func aesEncrypt(key, iv, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	padded := pkcs7Pad(plaintext, aes.BlockSize)
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)
	return ciphertext, nil
}

// aesDecrypt decrypts AES-128-CBC ciphertext and removes PKCS7 padding.
func aesDecrypt(key, iv, ciphertext []byte) ([]byte, error) {
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length %d not a multiple of block size", len(ciphertext))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, ciphertext)
	return pkcs7Unpad(plaintext)
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	padding := make([]byte, pad)
	for i := range padding {
		padding[i] = byte(pad)
	}
	return append(data, padding...)
}

func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty data")
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(data) {
		return nil, fmt.Errorf("invalid padding byte %d", pad)
	}
	return data[:len(data)-pad], nil
}
