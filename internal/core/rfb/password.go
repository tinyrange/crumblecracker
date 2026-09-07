package rfb

import (
	"crypto/des"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"io"
)

const securityVNCAuthentication = 2

// PasswordSecurity implements the standard VNC challenge-response handshake.
// The protocol only uses the first eight bytes of a password.
type PasswordSecurity string

func (PasswordSecurity) Type() uint8 {
	return securityVNCAuthentication
}

func (s PasswordSecurity) Handshake(conn io.ReadWriter) error {
	password := []byte(s)
	if len(password) == 0 {
		return fmt.Errorf("VNC password is empty")
	}
	if len(password) > 8 {
		return fmt.Errorf("VNC passwords are limited to 8 bytes")
	}
	challenge := make([]byte, 16)
	if _, err := rand.Read(challenge); err != nil {
		return fmt.Errorf("generate VNC challenge: %w", err)
	}
	if _, err := conn.Write(challenge); err != nil {
		return err
	}
	response := make([]byte, len(challenge))
	if _, err := io.ReadFull(conn, response); err != nil {
		return err
	}
	expected, err := passwordChallengeResponse(string(s), challenge)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(response, expected) != 1 {
		if err := binary.Write(conn, binary.BigEndian, uint32(1)); err != nil {
			return err
		}
		reason := "authentication failed"
		if err := binary.Write(conn, binary.BigEndian, uint32(len(reason))); err != nil {
			return err
		}
		_, _ = io.WriteString(conn, reason)
		return fmt.Errorf("VNC authentication failed")
	}
	return binary.Write(conn, binary.BigEndian, uint32(0))
}

func passwordChallengeResponse(password string, challenge []byte) ([]byte, error) {
	if len(password) == 0 {
		return nil, fmt.Errorf("VNC password is empty")
	}
	if len(password) > 8 {
		return nil, fmt.Errorf("VNC passwords are limited to 8 bytes")
	}
	if len(challenge) != 16 {
		return nil, fmt.Errorf("VNC challenge is %d bytes, want 16", len(challenge))
	}
	key := make([]byte, 8)
	for index, value := range []byte(password) {
		key[index] = reverseByte(value)
	}
	cipher, err := des.NewCipher(key)
	if err != nil {
		return nil, err
	}
	response := make([]byte, len(challenge))
	cipher.Encrypt(response[:8], challenge[:8])
	cipher.Encrypt(response[8:], challenge[8:])
	return response, nil
}

func reverseByte(value byte) byte {
	value = value>>4 | value<<4
	value = (value&0xcc)>>2 | (value&0x33)<<2
	return (value&0xaa)>>1 | (value&0x55)<<1
}
