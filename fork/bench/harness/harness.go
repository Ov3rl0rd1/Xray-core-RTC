//go:build bench

// Command harness hosts xray-core in-process through libxray's own logic
// (lib_api.go / lib_live.go are copies of libxray/api.go and live.go), and adds
// the lifecycle chaos the app produces: restarts, resets, live rule and outbound
// changes. Commands on stdin: conns | close <ids|*> | reload <file> | swap <file> | restart | stats
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/xtls/xray-core/transport/internet"
)

func main() {
	configPath := flag.String("c", "", "config file")
	restartEvery := flag.Duration("restart", 0, "stop+start the instance this often (0 = never)")
	resetEvery := flag.Duration("reset", 0, "call resetConnections this often (0 = never)")
	statsEvery := flag.Duration("stats", 5*time.Second, "telemetry interval")
	duration := flag.Duration("d", 0, "exit after this long (0 = run until signalled)")
	flag.Parse()

	raw, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg := string(raw)
	if err := startInstance(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}
	fmt.Println("started", version())

	t0 := time.Now()
	var restarts, resets, closed int
	tick := func(d time.Duration) <-chan time.Time {
		if d <= 0 {
			return nil
		}
		return time.NewTicker(d).C
	}
	restartC, resetC, statsC := tick(*restartEvery), tick(*resetEvery), tick(*statsEvery)
	var endC <-chan time.Time
	if *duration > 0 {
		endC = time.After(*duration)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	cmds := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			cmds <- strings.TrimSpace(sc.Text())
		}
	}()

	restart := func() {
		t := time.Now()
		if err := stopInstance(); err != nil {
			fmt.Println("stop error:", err)
		}
		if err := startInstance(cfg); err != nil {
			fmt.Println("restart error:", err)
			os.Exit(3)
		}
		restarts++
		fmt.Printf("restart #%d took %v\n", restarts, time.Since(t).Round(time.Millisecond))
	}
	stats := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		internet.ControllersLock.Lock()
		ctl := len(internet.Controllers)
		internet.ControllersLock.Unlock()
		fmt.Printf("t=%4.0fs goroutines=%d heap=%dMB sys=%dMB restarts=%d resets=%d(closed %d) controllers=%d\n",
			time.Since(t0).Seconds(), runtime.NumGoroutine(), m.HeapAlloc>>20, m.Sys>>20,
			restarts, resets, closed, ctl)
	}

	for {
		select {
		case <-restartC:
			restart()
		case <-resetC:
			closed += resetConnections()
			resets++
		case <-statsC:
			stats()
		case c := <-cmds:
			f := strings.Fields(c)
			if len(f) == 0 {
				continue
			}
			switch f[0] {
			case "conns":
				var list []map[string]any
				json.Unmarshal([]byte(connectionsJSON()), &list)
				by := map[string]int{}
				for _, e := range list {
					by[fmt.Sprint(e["net"], "/", e["outbound"])]++
				}
				fmt.Printf("conns n=%d by=%v\n", len(list), by)
				if len(f) > 1 {
					b, _ := json.Marshal(list)
					fmt.Println(string(b))
				}
			case "close":
				fmt.Println("closed", closeConnections(strings.Join(f[1:], ",")))
			case "reload", "swap":
				b, err := os.ReadFile(f[1])
				if err != nil {
					fmt.Println(err)
					continue
				}
				if f[0] == "reload" {
					err = reloadRouting(string(b))
				} else {
					err = replaceOutbound(string(b))
				}
				fmt.Println(f[0], "->", err)
			case "restart":
				restart()
			case "stats":
				stats()
			}
		case <-endC:
			stopInstance()
			debug.FreeOSMemory()
			time.Sleep(2 * time.Second)
			fmt.Printf("done: goroutines after stop=%d\n", runtime.NumGoroutine())
			return
		case <-sig:
			stopInstance()
			return
		}
	}
}
