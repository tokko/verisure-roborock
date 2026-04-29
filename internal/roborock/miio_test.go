package roborock

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"strings"
	"testing"
)

// TestDeriveKeys verifies key=MD5(token), iv=MD5(key||token).
func TestDeriveKeys(t *testing.T) {
	t.Run("known token", func(t *testing.T) {
		token := []byte("0123456789abcdef") // 16 bytes
		k := deriveKeys(token)

		expectedKey := md5.Sum(token)
		if !bytes.Equal(k.key, expectedKey[:]) {
			t.Errorf("key mismatch:\n got=%x\nwant=%x", k.key, expectedKey[:])
		}

		ivInput := append(append([]byte{}, expectedKey[:]...), token...)
		expectedIV := md5.Sum(ivInput)
		if !bytes.Equal(k.iv, expectedIV[:]) {
			t.Errorf("iv mismatch:\n got=%x\nwant=%x", k.iv, expectedIV[:])
		}

		if len(k.key) != 16 {
			t.Errorf("key length = %d, want 16", len(k.key))
		}
		if len(k.iv) != 16 {
			t.Errorf("iv length = %d, want 16", len(k.iv))
		}
	})

	t.Run("zero token deterministic", func(t *testing.T) {
		token := make([]byte, 16)
		k1 := deriveKeys(token)
		k2 := deriveKeys(token)
		if !bytes.Equal(k1.key, k2.key) {
			t.Error("zero-token derivation not deterministic for key")
		}
		if !bytes.Equal(k1.iv, k2.iv) {
			t.Error("zero-token derivation not deterministic for iv")
		}
		// MD5 of 16 zero bytes is a known constant: 4ae71336e44bf9bf79d2752e234818a5
		want := []byte{0x4a, 0xe7, 0x13, 0x36, 0xe4, 0x4b, 0xf9, 0xbf, 0x79, 0xd2, 0x75, 0x2e, 0x23, 0x48, 0x18, 0xa5}
		if !bytes.Equal(k1.key, want) {
			t.Errorf("md5(16 zeros) = %x, want %x", k1.key, want)
		}
	})
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	token := bytes.Repeat([]byte{0xAB}, 16)
	k := deriveKeys(token)

	sizes := []int{0, 1, 16, 17, 100}
	for _, sz := range sizes {
		payload := make([]byte, sz)
		for i := range payload {
			payload[i] = byte(i)
		}
		f := frame{deviceID: 0xDEADBEEF, stamp: 0x12345678, payload: payload}

		buf, err := encode(f, k)
		if err != nil {
			t.Fatalf("size %d: encode err: %v", sz, err)
		}

		// Magic bytes at [0:2].
		if got := binary.BigEndian.Uint16(buf[0:2]); got != miioMagic {
			t.Errorf("size %d: magic = 0x%04x, want 0x%04x", sz, got, miioMagic)
		}
		// Total length at [2:4] equals len(buf).
		if got := int(binary.BigEndian.Uint16(buf[2:4])); got != len(buf) {
			t.Errorf("size %d: totalLen field = %d, want %d", sz, got, len(buf))
		}

		got, err := decode(buf, k)
		if err != nil {
			t.Fatalf("size %d: decode err: %v", sz, err)
		}
		if got.deviceID != f.deviceID {
			t.Errorf("size %d: deviceID = %x, want %x", sz, got.deviceID, f.deviceID)
		}
		if got.stamp != f.stamp {
			t.Errorf("size %d: stamp = %x, want %x", sz, got.stamp, f.stamp)
		}
		if !bytes.Equal(got.payload, payload) {
			t.Errorf("size %d: payload mismatch\n got=%x\nwant=%x", sz, got.payload, payload)
		}
	}
}

func TestChecksumVerification(t *testing.T) {
	token := bytes.Repeat([]byte{0x11}, 16)
	k := deriveKeys(token)
	f := frame{deviceID: 1, stamp: 2, payload: []byte(`{"hi":1}`)}

	buf, err := encode(f, k)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	t.Run("valid decode", func(t *testing.T) {
		good := append([]byte{}, buf...)
		if _, err := decode(good, k); err != nil {
			t.Errorf("decode of valid frame failed: %v", err)
		}
	})

	t.Run("flipped encrypted byte", func(t *testing.T) {
		bad := append([]byte{}, buf...)
		bad[miioHeaderSize] ^= 0x01 // flip first encrypted byte
		_, err := decode(bad, k)
		if err == nil || !strings.Contains(err.Error(), "checksum") {
			t.Errorf("expected checksum error, got %v", err)
		}
	})

	t.Run("flipped checksum byte", func(t *testing.T) {
		bad := append([]byte{}, buf...)
		bad[16] ^= 0x01 // flip first checksum byte
		_, err := decode(bad, k)
		if err == nil || !strings.Contains(err.Error(), "checksum") {
			t.Errorf("expected checksum error, got %v", err)
		}
	})
}

func TestDecodeErrors(t *testing.T) {
	k := deriveKeys(make([]byte, 16))

	t.Run("empty buffer", func(t *testing.T) {
		_, err := decode([]byte{}, k)
		if err == nil || !strings.Contains(err.Error(), "frame too short") {
			t.Errorf("got %v, want frame too short", err)
		}
	})

	t.Run("buffer smaller than header", func(t *testing.T) {
		_, err := decode(make([]byte, miioHeaderSize-1), k)
		if err == nil || !strings.Contains(err.Error(), "frame too short") {
			t.Errorf("got %v, want frame too short", err)
		}
	})

	t.Run("wrong magic", func(t *testing.T) {
		buf := make([]byte, miioHeaderSize)
		binary.BigEndian.PutUint16(buf[0:2], 0xFFFF)
		binary.BigEndian.PutUint16(buf[2:4], miioHeaderSize)
		_, err := decode(buf, k)
		if err == nil || !strings.Contains(err.Error(), "bad magic") {
			t.Errorf("got %v, want bad magic", err)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		buf := make([]byte, miioHeaderSize+8)
		binary.BigEndian.PutUint16(buf[0:2], miioMagic)
		binary.BigEndian.PutUint16(buf[2:4], miioHeaderSize+32) // claims more than provided
		_, err := decode(buf, k)
		if err == nil || !strings.Contains(err.Error(), "truncated") {
			t.Errorf("got %v, want truncated", err)
		}
	})
}

func TestHelloPacket(t *testing.T) {
	pkt := helloPacket()
	if len(pkt) != 32 {
		t.Errorf("len = %d, want 32", len(pkt))
	}
	if got := binary.BigEndian.Uint16(pkt[0:2]); got != miioMagic {
		t.Errorf("magic = 0x%04x, want 0x%04x", got, miioMagic)
	}
	for i := 4; i < 32; i++ {
		if pkt[i] != 0xFF {
			t.Errorf("byte[%d] = 0x%02x, want 0xFF", i, pkt[i])
		}
	}
}

func TestPKCS7(t *testing.T) {
	t.Run("round trip various sizes", func(t *testing.T) {
		for _, sz := range []int{0, 1, 15, 16, 17} {
			data := make([]byte, sz)
			for i := range data {
				data[i] = byte(i + 1)
			}
			padded := pkcs7Pad(data, 16)
			if len(padded)%16 != 0 {
				t.Errorf("size %d: padded len %d not multiple of 16", sz, len(padded))
			}
			out, err := pkcs7Unpad(padded)
			if err != nil {
				t.Errorf("size %d: unpad err: %v", sz, err)
				continue
			}
			if !bytes.Equal(out, data) {
				t.Errorf("size %d: round trip mismatch\n got=%x\nwant=%x", sz, out, data)
			}
		}
	})

	t.Run("invalid padding byte 0", func(t *testing.T) {
		data := make([]byte, 16) // last byte is 0
		_, err := pkcs7Unpad(data)
		if err == nil {
			t.Error("expected error for padding byte 0")
		}
	})

	t.Run("padding byte > blockSize", func(t *testing.T) {
		data := make([]byte, 16)
		data[15] = 17
		_, err := pkcs7Unpad(data)
		if err == nil {
			t.Error("expected error for padding byte 17")
		}
	})

	t.Run("empty", func(t *testing.T) {
		_, err := pkcs7Unpad(nil)
		if err == nil {
			t.Error("expected error for empty data")
		}
	})
}

func TestAESEncryptDecrypt(t *testing.T) {
	k := deriveKeys(bytes.Repeat([]byte{0x42}, 16))

	t.Run("round trip", func(t *testing.T) {
		plain := []byte("hello roborock")
		ct, err := aesEncrypt(k.key, k.iv, plain)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		out, err := aesDecrypt(k.key, k.iv, ct)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if !bytes.Equal(out, plain) {
			t.Errorf("got %q, want %q", out, plain)
		}
	})

	t.Run("wrong key does not panic", func(t *testing.T) {
		plain := []byte("secret message goes here")
		ct, err := aesEncrypt(k.key, k.iv, plain)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		wrongKey := bytes.Repeat([]byte{0x99}, 16)
		// Either returns garbage successfully or returns padding error — both are fine.
		out, err := aesDecrypt(wrongKey, k.iv, ct)
		if err == nil && bytes.Equal(out, plain) {
			t.Error("decryption with wrong key returned plaintext")
		}
	})
}
