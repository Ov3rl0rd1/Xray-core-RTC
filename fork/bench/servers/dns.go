//go:build bench

package main

import (
	"encoding/binary"
	"log"
	"net"
)

// dnsServer answers every A query with the target address and every SRV query with
// one record, which is enough to tell "answered by the core" from "forwarded" apart.
func dnsServer(addr string, a net.IP) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Fatal(err)
	}
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil || n < 12 {
			continue
		}
		q := buf[:n]
		// Walk the question name.
		i := 12
		for i < n && q[i] != 0 {
			i += int(q[i]) + 1
		}
		if i+5 > n {
			continue
		}
		qtype := binary.BigEndian.Uint16(q[i+1:])
		question := q[12 : i+5]
		resp := make([]byte, 0, 512)
		resp = append(resp, q[0], q[1], 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0)
		resp = append(resp, question...)
		switch qtype {
		case 1:
			resp[7] = 1
			resp = append(resp, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
			resp = append(resp, a.To4()...)
		case 33:
			resp[7] = 1
			target := []byte{4, 'g', 'a', 'm', 'e', 4, 't', 'e', 's', 't', 0}
			rd := []byte{0, 10, 0, 5, 0x63, 0xdd}
			rd = append(rd, target...)
			resp = append(resp, 0xc0, 12, 0, 33, 0, 1, 0, 0, 0, 60, 0, byte(len(rd)))
			resp = append(resp, rd...)
		}
		pc.WriteTo(resp, from)
	}
}
