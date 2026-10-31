// blockmem measures how much heap one decoded block takes, relative to its
// msgpack size (sizing the scanner's backfill memory budget).
// usage: go run ./scripts/blockmem <block.msgpack>
package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/algorand/go-algorand-sdk/v2/client/v2/common/models"
	"github.com/algorand/go-algorand-sdk/v2/encoding/msgpack"
)

func main() {
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	var r models.BlockResponse
	if err := msgpack.Decode(raw, &r); err != nil {
		panic(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&m1)
	live := m1.HeapAlloc - m0.HeapAlloc
	fmt.Printf("wire %.2f MB, txns %d, decoded live heap %.2f MB, ratio %.1f, allocated during decode %.2f MB\n",
		float64(len(raw))/1e6, len(r.Block.Payset), float64(live)/1e6, float64(live)/float64(len(raw)), float64(m1.TotalAlloc-m0.TotalAlloc)/1e6)
	runtime.KeepAlive(r)
}
