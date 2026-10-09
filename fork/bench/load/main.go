//go:build bench

// Command load generates the traffic a gamer's PC produces and reports what a gamer
// would notice: stalls on a game's UDP stream, long-lived TCP sessions that break,
// failed connects, throughput. Runs against plain sockets, so it measures whatever
// the route table sends those sockets through (hev TUN, xray TUN, or nothing).
package main

import (
	"errors"
	"regexp"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	target   = flag.String("t", "10.99.1.1", "target host")
	games    = flag.Int("games", 1, "game UDP sessions (60 pps each)")
	longs    = flag.Int("long", 4, "long-lived TCP sessions (1 ping/s each)")
	churn    = flag.Int("churn", 8, "parallel short TCP connect/echo/close loops")
	dl       = flag.Int("dl", 0, "parallel bulk download streams")
	dur      = flag.Duration("d", 60*time.Second, "duration")
	report   = flag.Duration("r", 5*time.Second, "report interval")
	stallThr = flag.Duration("stall", 500*time.Millisecond, "UDP gap counted as a stall")
)

type gameStats struct {
	mu      sync.Mutex
	rtts    []time.Duration
	sent    int64
	recv    int64
	stalls  int64
	maxGap  time.Duration
	errs    int64
}

var (
	gs                         gameStats
	longBreaks, longReconnects atomic.Int64
	churnOK, churnFail         atomic.Int64
	churnLat                   atomic.Int64 // ns sum
	dlBytes                    atomic.Int64
	failMsgs                   sync.Map
)

func noteErr(kind string, err error) {
	key := kind + ": " + err.Error()
	v, _ := failMsgs.LoadOrStore(key, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

func main() {
	flag.Parse()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < *games; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); game(stop) }()
	}
	for i := 0; i < *longs; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); long(stop) }()
	}
	for i := 0; i < *churn; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); churnLoop(stop) }()
	}
	for i := 0; i < *dl; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); download(stop) }()
	}
	t0 := time.Now()
	tick := time.NewTicker(*report)
	end := time.After(*dur)
	var lastDl int64
loop:
	for {
		select {
		case <-tick.C:
			lastDl = printReport(t0, lastDl, false)
		case <-end:
			break loop
		}
	}
	close(stop)
	wg.Wait()
	printReport(t0, lastDl, true)
}

func printReport(t0 time.Time, lastDl int64, final bool) int64 {
	gs.mu.Lock()
	r := append([]time.Duration(nil), gs.rtts...)
	if !final {
		gs.rtts = gs.rtts[:0]
	}
	sent, recv, stalls, maxGap, gerr := gs.sent, gs.recv, gs.stalls, gs.maxGap, gs.errs
	gs.mu.Unlock()
	sort.Slice(r, func(i, j int) bool { return r[i] < r[j] })
	pct := func(p float64) time.Duration {
		if len(r) == 0 {
			return 0
		}
		return r[int(float64(len(r)-1)*p)]
	}
	d := dlBytes.Load()
	n := churnOK.Load()
	avg := time.Duration(0)
	if n > 0 {
		avg = time.Duration(churnLat.Load() / n)
	}
	tag := "t="
	if final {
		tag = "FINAL t="
	}
	fmt.Printf("%s%3.0fs game sent=%d recv=%d rtt p50=%v p99=%v max=%v stalls=%d maxgap=%v err=%d | long breaks=%d | churn ok=%d fail=%d avg=%v | dl=%.1fMB/s\n",
		tag, time.Since(t0).Seconds(), sent, recv, pct(0.5).Round(time.Microsecond*100), pct(0.99).Round(time.Microsecond*100),
		pct(1).Round(time.Microsecond*100), stalls, maxGap.Round(time.Millisecond), gerr,
		longBreaks.Load(), n, churnFail.Load(), avg.Round(time.Microsecond*100),
		float64(d-lastDl)/1e6/report.Seconds())
	if final {
		failMsgs.Range(func(k, v any) bool {
			fmt.Printf("  error x%d  %s\n", v.(*atomic.Int64).Load(), k)
			return true
		})
	}
	return d
}

func game(stop chan struct{}) {
	c, err := dialUDP(net.JoinHostPort(*target, "27015"))
	if err != nil {
		noteErr("game dial", err)
		return
	}
	defer c.Close()
	go func() {
		buf := make([]byte, 2048)
		last := time.Now()
		for {
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, err := c.Read(buf)
			now := time.Now()
			gap := now.Sub(last)
			if err != nil {
				select {
				case <-stop:
					return
				default:
				}
				gs.mu.Lock()
				gs.errs++
				if gap > *stallThr {
					gs.stalls++
				}
				if gap > gs.maxGap {
					gs.maxGap = gap
				}
				gs.mu.Unlock()
				noteErr("game read", errors.New(strip(err.Error())))
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}
			last = now
			gs.mu.Lock()
			gs.recv++
			if gap > *stallThr {
				gs.stalls++
			}
			if gap > gs.maxGap {
				gs.maxGap = gap
			}
			if n >= 9 && buf[0] == 'P' {
				ts := int64(binary.BigEndian.Uint64(buf[1:9]))
				gs.rtts = append(gs.rtts, time.Duration(time.Now().UnixNano()-ts))
			}
			gs.mu.Unlock()
		}
	}()
	t := time.NewTicker(time.Second / 60)
	defer t.Stop()
	pkt := make([]byte, 100)
	pkt[0] = 'P'
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			binary.BigEndian.PutUint64(pkt[1:9], uint64(time.Now().UnixNano()))
			if _, err := c.Write(pkt); err != nil {
				noteErr("game write", err)
			}
			gs.mu.Lock()
			gs.sent++
			gs.mu.Unlock()
		}
	}
}

func long(stop chan struct{}) {
	for {
		c, err := dialTCP(net.JoinHostPort(*target, "7"), 5*time.Second)
		if err != nil {
			noteErr("long dial", err)
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
				continue
			}
		}
		buf := make([]byte, 64)
		broke := false
		for !broke {
			select {
			case <-stop:
				c.Close()
				return
			case <-time.After(time.Second):
			}
			c.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := c.Write([]byte("ping-ping-ping-ping")); err != nil {
				noteErr("long write", errors.New(strip(err.Error())))
				broke = true
				break
			}
			if _, err := io.ReadFull(c, buf[:19]); err != nil {
				noteErr("long read", errors.New(strip(err.Error())))
				broke = true
			}
		}
		c.Close()
		longBreaks.Add(1)
	}
}

func churnLoop(stop chan struct{}) {
	payload := make([]byte, 1024)
	buf := make([]byte, 1024)
	for {
		select {
		case <-stop:
			return
		default:
		}
		t := time.Now()
		c, err := dialTCP(net.JoinHostPort(*target, "7"), 5*time.Second)
		if err != nil {
			churnFail.Add(1)
			noteErr("churn dial", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		_, err = c.Write(payload)
		if err == nil {
			_, err = io.ReadFull(c, buf)
		}
		c.Close()
		if err != nil {
			churnFail.Add(1)
			noteErr("churn io", errors.New(strip(err.Error())))
			continue
		}
		churnOK.Add(1)
		churnLat.Add(int64(time.Since(t)))
		time.Sleep(50 * time.Millisecond)
	}
}

func download(stop chan struct{}) {
	buf := make([]byte, 64*1024)
	for {
		c, err := dialTCP(net.JoinHostPort(*target, "19"), 5*time.Second)
		if err != nil {
			noteErr("dl dial", err)
			time.Sleep(time.Second)
			continue
		}
		for {
			select {
			case <-stop:
				c.Close()
				return
			default:
			}
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, err := c.Read(buf)
			dlBytes.Add(int64(n))
			if err != nil {
				noteErr("dl read", err)
				break
			}
		}
		c.Close()
	}
}

func init() { _ = os.Stdout }

var portRE = regexp.MustCompile(`:\d+->`)

// strip removes the ephemeral port so identical failures group together.
func strip(s string) string { return portRE.ReplaceAllString(s, ":*->") }
