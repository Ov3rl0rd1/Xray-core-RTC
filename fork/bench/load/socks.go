//go:build bench

package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"io"
	"net"
	"time"
)

var socksAddr = flag.String("socks", "", "use this SOCKS5 server (host:port) instead of plain sockets")

func socksHandshake(c net.Conn, cmd byte, ip net.IP, port int) (net.IP, int, error) {
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		return nil, 0, err
	}
	r := make([]byte, 2)
	if _, err := io.ReadFull(c, r); err != nil {
		return nil, 0, err
	}
	req := []byte{5, cmd, 0, 1}
	req = append(req, ip.To4()...)
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := c.Write(req); err != nil {
		return nil, 0, err
	}
	rep := make([]byte, 10)
	if _, err := io.ReadFull(c, rep); err != nil {
		return nil, 0, err
	}
	if rep[1] != 0 {
		return nil, 0, errors.New("socks reply " + string('0'+rep[1]))
	}
	return net.IP(rep[4:8]), int(binary.BigEndian.Uint16(rep[8:10])), nil
}

// dialTCP connects directly or through the SOCKS5 server.
func dialTCP(addr string, timeout time.Duration) (net.Conn, error) {
	if *socksAddr == "" {
		return net.DialTimeout("tcp", addr, timeout)
	}
	host, p, _ := net.SplitHostPort(addr)
	port, _ := net.LookupPort("tcp", p)
	c, err := net.DialTimeout("tcp", *socksAddr, timeout)
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(timeout))
	if _, _, err := socksHandshake(c, 1, net.ParseIP(host), port); err != nil {
		c.Close()
		return nil, err
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

// udpConn is a connected UDP socket, plain or wrapped in a SOCKS5 association.
type udpConn struct {
	net.Conn
	ctl  net.Conn
	head []byte
}

func dialUDP(addr string) (net.Conn, error) {
	if *socksAddr == "" {
		return net.Dial("udp", addr)
	}
	host, p, _ := net.SplitHostPort(addr)
	port, _ := net.LookupPort("udp", p)
	ctl, err := net.DialTimeout("tcp", *socksAddr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	bip, bport, err := socksHandshake(ctl, 3, net.IPv4zero, 0)
	if err != nil {
		ctl.Close()
		return nil, err
	}
	if bip.IsUnspecified() {
		bip = net.ParseIP("127.0.0.1")
	}
	u, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: bip, Port: bport})
	if err != nil {
		ctl.Close()
		return nil, err
	}
	head := []byte{0, 0, 0, 1}
	head = append(head, net.ParseIP(host).To4()...)
	head = binary.BigEndian.AppendUint16(head, uint16(port))
	return &udpConn{Conn: u, ctl: ctl, head: head}, nil
}

func (u *udpConn) Write(b []byte) (int, error) {
	_, err := u.Conn.Write(append(append([]byte(nil), u.head...), b...))
	return len(b), err
}

func (u *udpConn) Read(b []byte) (int, error) {
	buf := make([]byte, len(b)+262)
	n, err := u.Conn.Read(buf)
	if err != nil {
		return 0, err
	}
	if n < 10 {
		return 0, errors.New("short socks udp packet")
	}
	return copy(b, buf[10:n]), nil
}

func (u *udpConn) Close() error { u.ctl.Close(); return u.Conn.Close() }
