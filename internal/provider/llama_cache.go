package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// resaveTokens is the cadence at which a conversation's slot KV cache is
// re-saved to disk: every resaveTokens NEW completion tokens. It keeps the
// on-disk cache (cache_conv_<id>.bin) fresh so a llama-server crash/restart
// loses at most ~8k tokens of a conversation's progress.
const resaveTokens = 8000

// cacheFile returns the on-disk KV-cache filename for a conversation —
// "cache_conv_<conversation_id>.bin" (the user's requested naming). The file
// lives in the llama server's --slot-save-path.
func cacheFile(convID string) string {
	return "cache_conv_" + convID + ".bin"
}

// slotCache is the shared per-llama-server state for the per-conversation KV
// cache. It is shared across every Llama instance pointing at the same server
// (a server's slots are a server-level resource, not a per-model one): the
// slot count, the conversation->slot assignment (stable within a run, so a
// conversation keeps its cache across its turns), the per-slot "last
// conversation" (decides when a slot must save its old conversation's cache
// and restore the new one's), and the per-conversation tokens-since-last-save
// (the 8k resave cadence).
//
// All of this is best-effort: a save/restore failure is logged and the request
// continues normally (the user's explicit requirement).
type slotCache struct {
	mu        sync.Mutex
	numSlots  int
	slotsSeen bool
	convSlot  map[string]int // conversation id -> assigned slot
	slotLast  map[int]string // slot -> last conversation id it served
	tokensNew map[string]int // conversation id -> completion tokens since last save
}

var (
	slotCaches   = map[string]*slotCache{}
	slotCachesMu sync.Mutex
)

// slotCacheFor returns the shared slotCache for a llama server base URL,
// creating it on first use.
func slotCacheFor(baseURL string) *slotCache {
	slotCachesMu.Lock()
	defer slotCachesMu.Unlock()
	sc, ok := slotCaches[baseURL]
	if !ok {
		sc = &slotCache{
			numSlots:  1,
			convSlot:  map[string]int{},
			slotLast:  map[int]string{},
			tokensNew: map[string]int{},
		}
		slotCaches[baseURL] = sc
	}
	return sc
}

// serverRoot returns the llama server root URL (the BaseURL minus the /v1
// suffix) — the native slot API (/slots/...) lives at the root, not under
// /v1 (where the OpenAI-compatible endpoints live).
func (l *Llama) serverRoot() string {
	root := strings.TrimRight(l.BaseURL, "/")
	root = strings.TrimSuffix(root, "/v1")
	return root
}

// slotCount queries the llama server's slot status (GET /slots) for its slot
// count, lazily (once per server). It returns 1 on failure (best-effort: a
// server we can't query is treated as single-slot). The HTTP call runs OUTSIDE
// the slotCache lock (never hold it across a network round-trip).
func (l *Llama) slotCount(ctx context.Context) int {
	sc := slotCacheFor(l.BaseURL)
	sc.mu.Lock()
	if sc.slotsSeen {
		n := sc.numSlots
		sc.mu.Unlock()
		return n
	}
	sc.slotsSeen = true
	sc.mu.Unlock()

	n := 1
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, l.serverRoot()+"/slots", nil)
	if err == nil {
		l.authorize(req)
		resp, err := l.HTTP.Do(req)
		if err == nil {
			defer resp.Body.Close()
			var slots []json.RawMessage
			if err := json.NewDecoder(resp.Body).Decode(&slots); err == nil && len(slots) > 0 {
				n = len(slots)
			}
		}
	}
	sc.mu.Lock()
	sc.numSlots = n
	sc.mu.Unlock()
	return n
}

// prepareSlotCache pins a conversation's turn to a slot and, if the slot's
// last conversation differs from this one, saves the old conversation's cache
// and restores this one's (both best-effort). It returns the slot to pin the
// request to (via id_slot), or -1 when there is no conversation (a meta-call,
// which carries no ConversationID and has no cache).
func (l *Llama) prepareSlotCache(ctx context.Context, convID string) int {
	if convID == "" {
		return -1
	}
	sc := slotCacheFor(l.BaseURL)
	numSlots := l.slotCount(ctx)
	sc.mu.Lock()
	slot := l.assignSlotLocked(sc, convID, numSlots)
	last := sc.slotLast[slot]
	needSave := last != "" && last != convID
	needRestore := last != convID
	sc.slotLast[slot] = convID
	sc.mu.Unlock()
	if needSave {
		l.saveSlotCache(ctx, slot, cacheFile(last))
	}
	if needRestore {
		l.restoreSlotCache(ctx, slot, cacheFile(convID))
	}
	return slot
}

// assignSlotLocked returns a conversation's slot, assigning one if needed.
// The assignment is STABLE within a run (a conversation keeps its slot, so its
// cache is preserved across its turns) and roughly balanced (the least-loaded
// slot by assigned-conversation count is chosen). Call with sc.mu held.
func (l *Llama) assignSlotLocked(sc *slotCache, convID string, numSlots int) int {
	if numSlots < 1 {
		numSlots = 1
	}
	if s, ok := sc.convSlot[convID]; ok {
		return s
	}
	load := map[int]int{}
	for _, s := range sc.convSlot {
		load[s]++
	}
	best, bestLoad := 0, 1<<30
	for s := 0; s < numSlots; s++ {
		if load[s] < bestLoad {
			best, bestLoad = s, load[s]
		}
	}
	sc.convSlot[convID] = best
	return best
}

// noteTurnCompletion folds a turn's completion tokens into the conversation's
// tokens-since-last-save and re-saves its cache when the 8k cadence is hit
// (best-effort). slot is the slot the turn ran on (-1 = no pin, no-op).
func (l *Llama) noteTurnCompletion(ctx context.Context, convID string, slot int, completionTokens int) {
	if convID == "" || slot < 0 || completionTokens <= 0 {
		return
	}
	sc := slotCacheFor(l.BaseURL)
	sc.mu.Lock()
	sc.tokensNew[convID] += completionTokens
	shouldSave := sc.tokensNew[convID] >= resaveTokens
	if shouldSave {
		sc.tokensNew[convID] = 0
	}
	sc.mu.Unlock()
	if shouldSave {
		l.saveSlotCache(ctx, slot, cacheFile(convID))
	}
}

// saveSlotCache saves a slot's KV cache to a file (POST /slots/{id}?action=save).
// Best-effort: failures are logged, never fatal.
func (l *Llama) saveSlotCache(ctx context.Context, slot int, filename string) {
	l.slotAction(ctx, "save", slot, map[string]any{"filename": filename}, "save", filename)
}

// restoreSlotCache restores a slot's KV cache from a file
// (POST /slots/{id}?action=restore). Best-effort: a missing/mismatched file is
// a non-fatal failure (the slot then starts with a cold cache).
func (l *Llama) restoreSlotCache(ctx context.Context, slot int, filename string) {
	l.slotAction(ctx, "restore", slot, map[string]any{"filename": filename}, "restore", filename)
}

// deleteSlotCache deletes a slot's KV cache (POST /slots/{id}?action=delete) —
// it clears the slot's in-memory KV cache AND removes the on-disk cache file
// (the fork's delete action owns the filesystem). Best-effort: a deferred/
// unavailable slot or a server failure is logged, never fatal.
func (l *Llama) deleteSlotCache(ctx context.Context, slot int, convID string) {
	filename := cacheFile(convID)
	l.slotAction(ctx, "delete", slot, map[string]any{"filename": filename}, "delete", filename)
}

// DeleteLlamaConversationCache deletes a conversation's KV cache on the llama
// server at baseURL — the conversation-delete path. It clears bevin's slot
// tracking for the conversation (so it is never restored again) and issues the
// slot's delete action on the slot the conversation was pinned to, so the llama
// server releases the conversation's KV cache — both the in-memory slot cache
// and the on-disk cache file (the fork's delete action owns the filesystem). It
// is a no-op when the conversation was never pinned to a slot on this server.
// Best-effort: the delete failure is logged, never returned. Called once per
// distinct llama server (baseURL) when a conversation is deleted.
func DeleteLlamaConversationCache(ctx context.Context, baseURL, convID, apiKey string) {
	if convID == "" {
		return
	}
	sc := slotCacheFor(baseURL)
	sc.mu.Lock()
	slot, pinned := sc.convSlot[convID]
	if pinned {
		delete(sc.convSlot, convID)
		delete(sc.tokensNew, convID)
		// Reset the slot's "last conversation" so the next conversation to
		// run on this slot restores ITS OWN cache (not the deleted one's).
		if sc.slotLast[slot] == convID {
			sc.slotLast[slot] = ""
		}
	}
	sc.mu.Unlock()
	if !pinned {
		return
	}
	NewLlama(baseURL, apiKey).deleteSlotCache(ctx, slot, convID)
}

// slotAction issues a native slot API call at the server root:
// POST /slots/{slot}?action={save|restore|delete}. A non-nil body is sent as
// JSON (save/restore/delete carry {filename}). Best-effort: the
// failure is logged and swallowed (the request continues normally — the
// user's explicit requirement).
func (l *Llama) slotAction(ctx context.Context, action string, slot int, body map[string]any, verb, filename string) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var rd io.Reader
	if body != nil {
		payload, _ := json.Marshal(body)
		rd = strings.NewReader(string(payload))
	}
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, fmt.Sprintf("%s/slots/%d?action=%s", l.serverRoot(), slot, action), rd)
	if err != nil {
		return
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	l.authorize(req)
	resp, err := l.HTTP.Do(req)
	if err != nil {
		log.Printf("bevin: llama cache %s failed (slot %d, %s): %v", verb, slot, filename, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("bevin: llama cache %s failed (slot %d, %s): status %d", verb, slot, filename, resp.StatusCode)
	}
}
