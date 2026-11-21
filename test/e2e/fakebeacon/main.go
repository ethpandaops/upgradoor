// Command fakebeacon is a minimal beacon API for end-to-end tests that have
// no real network: a healthy chain that finalizes two epochs behind the
// head. Every slot has a block, and validators 0 to -validators attest
// perfectly: each sits in the one committee of one slot per epoch, and the
// block of the next slot carries the committee's attestation with every vote.
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Field names of the beacon API that occur in several answers.
const (
	keySlot      = "slot"
	keySignature = "signature"
)

func main() {
	listen := flag.String("listen", ":5052", "listen address")
	secondsPerSlot := flag.Uint64("seconds-per-slot", 2, "slot duration")
	slotsPerEpoch := flag.Uint64("slots-per-epoch", 4, "epoch length")
	validators := flag.Uint64("validators", 64, "number of validators, from index 0, that attest")

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
	// The root of a block is its slot number, so the chain can be walked by
	// parent root.
	blockRoot := func(slot uint64) string { return fmt.Sprintf("0x%064x", slot) }

	parentRoot := func(slot uint64) string {
		if slot == 0 {
			return "0x" + strings.Repeat("00", 32)
		}

		return blockRoot(slot - 1)
	}

	mux.HandleFunc("GET /eth/v1/beacon/headers/head", func(w http.ResponseWriter, _ *http.Request) {
		head := slot()

		write(w, map[string]any{
			"root":      blockRoot(head),
			"canonical": true,
			"header": map[string]any{
				"message": map[string]string{
					keySlot: strconv.FormatUint(head, 10), "proposer_index": "0",
					"parent_root": parentRoot(head), "state_root": root, "body_root": root,
				},
				keySignature: "0x00",
			},
		})
	})
	// committee returns the one committee of a slot: the validators whose
	// index falls on the slot's position in the epoch.
	committee := func(at uint64) []string {
		members := make([]string, 0, *validators / *slotsPerEpoch + 1)

		for v := at % *slotsPerEpoch; v < *validators; v += *slotsPerEpoch {
			members = append(members, strconv.FormatUint(v, 10))
		}

		return members
	}

	// attestation is the full aggregate of a slot's committee, in the shape
	// since Electra: committee 0 selected, every member's bit set, and the
	// bitlist's length bit after them.
	attestation := func(at uint64) map[string]any {
		size := len(committee(at))
		bits := make([]byte, size/8+1)

		for i := 0; i <= size; i++ {
			bits[i/8] |= 1 << (i % 8)
		}

		return map[string]any{
			"aggregation_bits": "0x" + hex.EncodeToString(bits),
			"committee_bits":   "0x0100000000000000",
			"data":             map[string]string{keySlot: strconv.FormatUint(at, 10), "index": "0"},
			keySignature:       "0x00",
		}
	}

	mux.HandleFunc("GET /eth/v2/beacon/blocks/{root}", func(w http.ResponseWriter, r *http.Request) {
		at, err := strconv.ParseUint(strings.TrimPrefix(r.PathValue("root"), "0x"), 16, 64)
		if err != nil || at > slot() {
			http.Error(w, `{"code":404,"message":"block not found"}`, http.StatusNotFound)

			return
		}

		attestations := []any{}
		if at > 0 && *validators > 0 {
			attestations = append(attestations, attestation(at-1))
		}

		write(w, map[string]any{
			"message": map[string]any{
				keySlot: strconv.FormatUint(at, 10), "proposer_index": "0",
				"parent_root": parentRoot(at), "state_root": root,
				"body": map[string]any{"attestations": attestations},
			},
			keySignature: "0x00",
		})
	})
	mux.HandleFunc("GET /eth/v1/beacon/states/head/committees",
		func(w http.ResponseWriter, r *http.Request) {
			epoch, err := strconv.ParseUint(r.URL.Query().Get("epoch"), 10, 64)
			if err != nil {
				http.Error(w, `{"code":400,"message":"bad epoch"}`, http.StatusBadRequest)

				return
			}

			committees := make([]any, 0, *slotsPerEpoch)

			for at := epoch * *slotsPerEpoch; at < (epoch+1)**slotsPerEpoch; at++ {
				committees = append(committees, map[string]any{
					"index": "0", keySlot: strconv.FormatUint(at, 10), "validators": committee(at),
				})
			}

			write(w, committees)
		})
	mux.HandleFunc("GET /eth/v1/node/syncing", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"is_syncing": false, "sync_distance": "0"})
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
