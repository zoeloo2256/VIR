package main

import (
	"context"
	"errors"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// BERMUDA Stealth Gateway NG — Master Edge Entrypoint & Runtime Orchestrator
// Dynamic cgroup v1/v2 Budgeting, 4-Stage Zero-Loss Drain, Go 1.24 Baseline
// Single-User Dedicated Profile — Ultra-Low Latency & High-Throughput Edge
// ---------------------------------------------------------------------------

const (
	defaultPort            = "8080"
	fallbackSelfMemMB      = 128
	fallbackXrayMemMB      = 550
	defaultGOMAXPROCS      = 2
	gatewayMemPercent      = 12
	xrayMemPercent         = 55
	httpDrainTimeout       = 10 * time.Second
	supervisorStopWait     = 8 * time.Second
	edgeKeepAlivePeriod    = 15 * time.Second
	drainPropagationWindow = 500 * time.Millisecond
)

// cgroupMemoryLimit reads cgroup v2 memory.max, falling back to v1 memory.limit_in_bytes.
func cgroupMemoryLimit() (uint64, bool) {
	candidates := []string{
		"/sys/fs/cgroup/memory.max",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes",
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(data))
		if s == "" || s == "max" {
			continue
		}
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil || n == 0 || n >= 1<<50 { // cgroup v1 reports ~9.22e18 when unlimited
			continue
		}
		return n, true
	}
	return 0, false
}

// deriveMemoryBudget dynamically allocates runtime memory based on effective cgroup limits.
// Allocates 12% to Go gateway, 55% to Xray-core child daemon, reserving 33% for kernel buffers.
func deriveMemoryBudget() (gwMB, xrayMB int) {
	gwMB, xrayMB = fallbackSelfMemMB, fallbackXrayMemMB
	if limit, ok := cgroupMemoryLimit(); ok {
		total := int(limit >> 20)
		gwMB = maxInt(48, total*gatewayMemPercent/100)
		xrayMB = maxInt(128, total*xrayMemPercent/100)
	}
	gwMB = getEnvInt("BERMUDA_SELF_MEM_MB", gwMB)
	xrayMB = getEnvInt("BERMUDA_XRAY_MEM_MB", xrayMB)
	return gwMB, xrayMB
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// applyMemoryCeiling configures Go's runtime soft memory limit (GOMEMLIMIT) and GC percentage.
func applyMemoryCeiling(selfMB int) {
	debug.SetMemoryLimit(int64(selfMB) << 20)
	debug.SetGCPercent(100)
}

// cgroupCPUQuota auto-detects CPU limits from cgroup v2 or v1 hierarchies.
func cgroupCPUQuota() (float64, bool) {
	if data, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		f := strings.Fields(string(data))
		if len(f) == 2 && f[0] != "max" {
			q, e1 := strconv.ParseFloat(f[0], 64)
			p, e2 := strconv.ParseFloat(f[1], 64)
			if e1 == nil && e2 == nil && q > 0 && p > 0 {
				return q / p, true
			}
		}
		return 0, false
	}
	qb, e1 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	pb, e2 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if e1 == nil && e2 == nil {
		q, e3 := strconv.ParseFloat(strings.TrimSpace(string(qb)), 64)
		p, e4 := strconv.ParseFloat(strings.TrimSpace(string(pb)), 64)
		if e3 == nil && e4 == nil && q > 0 && p > 0 {
			return q / p, true
		}
	}
	return 0, false
}

// applyGOMAXPROCS dynamically pins the Go scheduler to the container CPU quota.
func applyGOMAXPROCS() int {
	n := getEnvInt("BERMUDA_GOMAXPROCS", 0)
	if n == 0 {
		if q, ok := cgroupCPUQuota(); ok {
			n = int(math.Ceil(q))
		}
	}
	if n <= 0 {
		n = defaultGOMAXPROCS
	}
	if cpus := runtime.NumCPU(); n > cpus {
		n = cpus
	}
	runtime.GOMAXPROCS(n)
	return n
}

// teardown executes the ordered, 4-stage graceful drain sequence without premature termination.
func teardown(srv *http.Server, gw *Gateway, sup *Supervisor, supCancel context.CancelFunc, graceful bool) {
	gw.SetDraining()
	if graceful {
		log.Println("[Gateway] Stage 1/4: Health flipped to 503 (traffic shedding)...")
		time.Sleep(drainPropagationWindow)
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), httpDrainTimeout)
	defer cancel()

	if graceful {
		if err := srv.Shutdown(drainCtx); err != nil {
			log.Printf("[Gateway] Stage 2/4: HTTP server drain timeout (%v); forcing socket closure", err)
			_ = srv.Close()
		} else {
			log.Println("[Gateway] Stage 2/4: HTTP connections drained successfully")
		}
		// Allow in-flight hijacked / spliced WebSocket transfers to conclude naturally
		gw.WaitTunnels(drainCtx)
	} else {
		_ = srv.Close()
	}

	log.Println("[Gateway] Stage 3/4: Force-closing remaining hijacked sockets and releasing loopback transport...")
	gw.CloseTunnels()
	gw.CloseIdleBackendConns()

	log.Println("[Gateway] Stage 4/4: Tearing down child Xray process group...")
	supCancel()
	sup.Stop(supervisorStopWait)
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.Println("[Gateway] Initializing BERMUDA Stealth Gateway NG...")

	// 1. Calculate dynamic memory ceilings and scheduler quotas from cgroup hierarchy
	gwMB, xrayMB := deriveMemoryBudget()
	applyMemoryCeiling(gwMB)
	procs := applyGOMAXPROCS()

	// Export derived parameters so supervisor propagates them to the child daemon
	_ = os.Setenv("BERMUDA_XRAY_MEM_MB", strconv.Itoa(xrayMB))
	_ = os.Setenv("BERMUDA_XRAY_GOMAXPROCS", strconv.Itoa(procs))
	log.Printf("[Runtime] Dynamic memory ceiling: Gateway=%dMiB, Xray=%dMiB (GOGC=100) | GOMAXPROCS=%d",
		gwMB, xrayMB, procs)

	port := getEnv("PORT", defaultPort)

	// 2. Instantiate supervisor and run non-fatal preflight syntax validation
	sup := NewSupervisor()
	if err := sup.Preflight(); err != nil {
		log.Printf("[Gateway] Warning: Supervisor preflight issue: %v. Continuing to start...", err)
	}

	// 3. Instantiate reverse proxy edge engine
	gw := NewGateway(sup)

	// 4. Decoupled Context Architecture:
	// sigCtx handles termination signals from Railway PaaS / Docker.
	// supCtx governs the Xray child supervisor independently, preventing premature shutdown.
	sigCtx, stopSig := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSig()
	supCtx, supCancel := context.WithCancel(context.Background())

	// 5. Run supervisor loop in a dedicated background goroutine
	supErrCh := make(chan error, 1)
	go func() {
		supErrCh <- sup.Run(supCtx)
	}()

	// 6. Configure HTTP edge server with line-rate socket tuning
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 5 * time.Second,  // Protects against Slowloris header trickle attacks
		IdleTimeout:       120 * time.Second, // Matches loopback transport idle timeout
		MaxHeaderBytes:    32 << 10,          // 32 KiB header limit
		ConnState:         gw.TrackConnState,
	}

	// 7. Bind TCP listener using modern Go 1.23+ KeepAliveConfig
	lc := net.ListenConfig{
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     edgeKeepAlivePeriod,
			Interval: edgeKeepAlivePeriod,
			Count:    4,
		},
	}
	ln, err := lc.Listen(context.Background(), "tcp", srv.Addr)
	if err != nil {
		log.Fatalf("[Gateway] Fatal: Cannot bind listener on %s: %v", srv.Addr, err)
	}

	serverErrCh := make(chan error, 1)
	go func() {
		log.Printf("[Gateway] Edge listener active on :%s (PID %d, GOMAXPROCS=%d)", port, os.Getpid(), procs)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- err
		}
	}()

	// 8. Await termination signals or fatal process errors
	select {
	case err := <-serverErrCh:
		log.Printf("[Gateway] Fatal: HTTP server failure: %v", err)
		teardown(srv, gw, sup, supCancel, false)
		os.Exit(1)
	case err := <-supErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("[Gateway] Fatal: Supervisor halted unexpectedly: %v", err)
		}
		teardown(srv, gw, sup, supCancel, false)
		os.Exit(1)
	case <-sigCtx.Done():
		log.Println("[Gateway] Termination signal intercepted. Commencing graceful teardown...")
	}

	// 9. Execute graceful drain sequence
	teardown(srv, gw, sup, supCancel, true)
	log.Println("[Gateway] BERMUDA Stealth Gateway shutdown complete. Ports released cleanly. Exit 0.")
}
