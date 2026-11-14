// Command fakebeacon is a minimal beacon API for end-to-end tests that have
// no real network: a healthy chain that finalizes two epochs behind the
// head and does not serve the validator liveness endpoint.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func main() {
	listen := flag.String("listen", ":5052", "listen address")
	secondsPerSlot := flag.Uint64("seconds-per-slot", 2, "slot duration")
	slotsPerEpoch := flag.Uint64("slots-per-epoch", 4, "epoch length")

	flag.Parse()

	// Start a few epochs in, so there is something to finalize.
	genesis := time.Now().Add(-time.Duration(*secondsPerSlot**slotsPerEpoch*8) * time.Second)

	slot := func() uint64 {
		return uint64(time.Since(genesis)/time.Second) / *secondsPerSlot
	}

	write := func(w http.ResponseWriter, data any) {
		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			log.Print(err)
		}
	}

	root := "0x" + strings.Repeat("11", 32)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /eth/v1/beacon/genesis", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]string{
			"genesis_time":            strconv.FormatInt(genesis.Unix(), 10),
			"genesis_validators_root": root,
			"genesis_fork_version":    "0x00000000",
		})
	})
	mux.HandleFunc("GET /eth/v1/config/spec", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]string{
			"SECONDS_PER_SLOT": strconv.FormatUint(*secondsPerSlot, 10),
			"SLOTS_PER_EPOCH":  strconv.FormatUint(*slotsPerEpoch, 10),
		})
	})
	mux.HandleFunc("GET /eth/v1/beacon/headers/head", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{
			"root":      root,
			"canonical": true,
			"header": map[string]any{
				"message": map[string]string{
					"slot": strconv.FormatUint(slot(), 10), "proposer_index": "0",
					"parent_root": root, "state_root": root, "body_root": root,
				},
				"signature": "0x00",
			},
		})
	})
	mux.HandleFunc("GET /eth/v1/beacon/states/head/finality_checkpoints",
		func(w http.ResponseWriter, _ *http.Request) {
			epoch := slot() / *slotsPerEpoch

			cp := func(e uint64) map[string]string {
				return map[string]string{"epoch": strconv.FormatUint(e, 10), "root": root}
			}

			write(w, map[string]any{
				"previous_justified": cp(epoch - 2),
				"current_justified":  cp(epoch - 1),
				"finalized":          cp(epoch - 2),
			})
		})

	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	log.Printf("fakebeacon on %s", *listen)
	log.Fatal(srv.ListenAndServe())
}
