package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"math/rand"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/grafana/pyroscope-go"
)

var globalMu sync.Mutex

// leakBucket acumula memória propositalmente (visível em alloc/inuse memory)
var leakBucket [][]byte

// orderCache simula leak de MUITOS objetos pequenos: um cache sem eviction.
// Cada Order é pequena (~100 bytes + strings + slice), então em BYTES o impacto
// é modesto, mas em CONTAGEM de objetos explode -> aparece no inuse_objects.
var orderCache = map[string]*Order{}

type Order struct {
	ID        string
	Items     []string
	CreatedAt time.Time
}

func main() {
	// Habilita profiles de mutex e block (desabilitados por padrão no Go)
	runtime.SetMutexProfileFraction(5)
	runtime.SetBlockProfileRate(5)

	serverAddr := getEnv("PYROSCOPE_SERVER", "http://localhost:4040")

	profiler, err := pyroscope.Start(pyroscope.Config{
		ApplicationName: "poc.checkout-service",
		ServerAddress:   serverAddr,
		Logger:          pyroscope.StandardLogger,
		Tags: map[string]string{
			"region":      "sa-east-1",
			"environment": "poc",
			"version":     "1.0.0",
		},
		ProfileTypes: []pyroscope.ProfileType{
			pyroscope.ProfileCPU,
			pyroscope.ProfileAllocObjects,
			pyroscope.ProfileAllocSpace,
			pyroscope.ProfileInuseObjects,
			pyroscope.ProfileInuseSpace,
			pyroscope.ProfileGoroutines,
			pyroscope.ProfileMutexCount,
			pyroscope.ProfileMutexDuration,
			pyroscope.ProfileBlockCount,
			pyroscope.ProfileBlockDuration,
		},
	})
	if err != nil {
		log.Fatalf("erro ao iniciar pyroscope: %v", err)
	}
	defer profiler.Stop()

	log.Printf("pyroscope profiling ativo -> %s (app: poc.checkout-service)", serverAddr)

	// Simula requisições a 3 "endpoints" com custos diferentes
	go simulateTraffic("POST /checkout", 70*time.Millisecond, checkoutHandler)
	go simulateTraffic("GET /products", 40*time.Millisecond, productsHandler)
	go simulateTraffic("GET /search", 120*time.Millisecond, searchHandler)

	// Goroutine que segura o lock global de propósito
	go lockHog()

	// Goroutine com leak de muitos objetos pequenos (cache sem eviction)
	go leakyCache()

	select {}
}

// simulateTraffic chama o handler num loop com tags de contexto nos profiles
func simulateTraffic(endpoint string, every time.Duration, handler func(context.Context)) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for range ticker.C {
		// TagWrapper adiciona labels dinâmicas aos profiles (visíveis como filtros na UI)
		pyroscope.TagWrapper(context.Background(), pyroscope.Labels("endpoint", endpoint), handler)
	}
}

// --- Handlers falsos com perfis de custo diferentes ---

func checkoutHandler(_ context.Context) {
	computeHash(3_000_000) // CPU pesada
	allocMemory(2 << 20)   // 2MB alocados por chamada
	takeLock(5 * time.Millisecond)
}

func productsHandler(_ context.Context) {
	computeHash(800_000)
	allocMemory(256 << 10)
	takeLock(time.Millisecond)
}

func searchHandler(_ context.Context) {
	computeHash(1_500_000)
	fibonacci(28) // CPU recursiva (stack profunda interessante no flamegraph)
	allocMemory(512 << 10)
}

// --- Funções de carga ---

func computeHash(iterations int) {
	data := []byte("pyroscope-poc")
	for i := 0; i < iterations; i++ {
		sum := sha256.Sum256(data)
		data = sum[:]
	}
}

func fibonacci(n int) int {
	if n < 2 {
		return n
	}
	return fibonacci(n-1) + fibonacci(n-2)
}

func allocMemory(size int) {
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte(rand.Intn(256))
	}
	leakBucket = append(leakBucket, buf)
	// "libera" de vez em quando para inuse não explodir infinitamente
	if len(leakBucket) > 50 {
		leakBucket = leakBucket[25:]
	}
}

func takeLock(d time.Duration) {
	globalMu.Lock()
	defer globalMu.Unlock()
	time.Sleep(d)
}

func lockHog() {
	for {
		globalMu.Lock()
		time.Sleep(3 * time.Millisecond)
		globalMu.Unlock()
		time.Sleep(time.Millisecond)
	}
}

// leakyCache insere Orders num map que nunca é limpo -> goroutine de leak.
// ~500 pedidos/seg, cada um com struct + string + slice = vários objetos pequenos.
func leakyCache() {
	for {
		order := &Order{
			ID:        fmt.Sprintf("order-%d", rand.Int63()),
			Items:     []string{"item-a", "item-b", "item-c"},
			CreatedAt: time.Now(),
		}
		orderCache[order.ID] = order // nunca remove
		time.Sleep(2 * time.Millisecond)
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
