package socks5

import (
	"bufio"
	"bytes"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSocks5ClientHandshake(t *testing.T) {
	// Mock server responses
	readBuffer := &bytes.Buffer{}
	readBuffer.Write([]byte{Version, MethodUserPass})
	readBuffer.Write([]byte{Version, 0x00 /* STATUS of SUCCESS */})
	readBuffer.Write([]byte{Version, 0x00 /* STATUS of SUCCESS */, 0x00 /* RSV */})
	readBuffer.Write([]byte{AtypIPv4, 0x1, 0x2, 0x3, 0x4, 0x0, 0x0 /* IPv4: 1.2.3.4:0 */})
	reader := bufio.NewReader(bytes.NewReader(readBuffer.Bytes()))

	writeBuffer := &bytes.Buffer{}
	writer := bufio.NewWriter(writeBuffer)

	io := bufio.NewReadWriter(reader, writer)

	addr, err := ClientHandshake(io, Addr{AtypIPv4, 1, 2, 3, 4, 0, 80}, CmdConnect, &User{
		Username: "test",
		Password: "6ab49d8b-a009-44e4-bd53-fbdb48fbe7eb",
	})

	assert.Nil(t, err, "Failed to perform SOCKS5 client handshake: %v", err)
	assert.Equal(t, "1.2.3.4:0", addr.String(), "Incorrect address obtained from SOCKS5 client handshake")
}

func TestAddrRejectsInvalidAndShortValues(t *testing.T) {
	for _, addr := range []Addr{nil, {}, {0xff, 0, 0, 0}, {AtypIPv4, 1, 2}, {AtypIPv6, 1, 2}, {AtypDomainName, 5, 'a'}} {
		assert.False(t, addr.Valid())
		assert.Empty(t, addr.String())
	}
}

func TestDecodeUDPPacketRejectsShortAddress(t *testing.T) {
	addr, payload, err := DecodeUDPPacket([]byte{0, 0, 0, AtypIPv4, 1})
	assert.Error(t, err)
	assert.Nil(t, addr)
	assert.Nil(t, payload)
}

func TestSerializeAddrRejectsLongDomain(t *testing.T) {
	assert.Nil(t, SerializeAddr(string(bytes.Repeat([]byte{'a'}, 256)), netip.Addr{}, 80))
}
