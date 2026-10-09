//go:build bench

// Command servers runs the "internet" side of the bench: echo, HTTP-ip, a TLS 1.3
// target for REALITY to borrow, and a 60 Hz "game server" over UDP.
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

func main() {
	bind := flag.String("bind", "10.99.1.1", "address to serve on")
	cert := flag.String("cert", "dest.crt", "TLS cert for the REALITY target")
	key := flag.String("key", "dest.key", "TLS key")
	flag.Parse()

	go tcpEcho(*bind + ":7")
	go udpEcho(*bind + ":7")
	go gameServer(*bind + ":27015")
	go httpIP(*bind + ":80")
	go tlsTarget(*bind+":443", *cert, *key)
	go discard(*bind + ":9")
	go source(*bind + ":19")
	go dnsServer(*bind+":53", net.ParseIP(*bind))
	select {}
}

func tcpEcho(addr string) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			continue
		}
		go func() { defer c.Close(); io.Copy(c, c) }()
	}
}

func discard(addr string) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			continue
		}
		go func() { defer c.Close(); io.Copy(io.Discard, c) }()
	}
}

// source streams bytes forever: a download.
func source(addr string) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	buf := make([]byte, 32*1024)
	for {
		c, err := l.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer c.Close()
			for {
				if _, err := c.Write(buf); err != nil {
					return
				}
			}
		}()
	}
}

func udpEcho(addr string) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Fatal(err)
	}
	buf := make([]byte, 65535)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		pc.WriteTo(buf[:n], from)
	}
}

// gameServer echoes every packet and, for each client seen in the last 5 s, pushes a
// 60 Hz state update — the traffic shape of a game session.
func gameServer(addr string) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Fatal(err)
	}
	type peer struct {
		addr net.Addr
		seen atomic.Int64
	}
	peers := map[string]*peer{}
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	go func() {
		tick := time.NewTicker(time.Second / 60)
		state := make([]byte, 120)
		var seq uint64
		for range tick.C {
			seq++
			copy(state, fmt.Sprintf("S%016d", seq))
			<-mu
			for k, p := range peers {
				if time.Since(time.Unix(0, p.seen.Load())) > 5*time.Second {
					delete(peers, k)
					continue
				}
				pc.WriteTo(state, p.addr)
			}
			mu <- struct{}{}
		}
	}()
	buf := make([]byte, 2048)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		<-mu
		p := peers[from.String()]
		if p == nil {
			p = &peer{addr: from}
			peers[from.String()] = p
		}
		p.seen.Store(time.Now().UnixNano())
		mu <- struct{}{}
		pc.WriteTo(buf[:n], from) // echo for RTT
	}
}

func httpIP(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		fmt.Fprint(w, host)
	})
	log.Fatal(http.ListenAndServe(addr, mux))
}

func tlsTarget(addr, cert, key string) {
	c, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{
		Addr:      addr,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{c}, MinVersion: tls.VersionTLS13},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "ok")
		}),
	}
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
