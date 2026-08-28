package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/dlp"
	"github.com/nidjbs/go-ai-gateway/internal/routing"
)

// dlpContentKeys are JSON fields carrying model output text in
// OpenAI-compatible payloads.
var dlpContentKeys = map[string]bool{"content": true, "text": true, "delta": true}

// dlpApplyNonStreaming masks sensitive text under the content-bearing keys
// of a response JSON document; in reject mode it returns the document
// unchanged and the caller acts on hits.
func dlpApplyNonStreaming(data []byte, d *dlp.Detector) ([]byte, []dlp.Hit) {
	if !d.Enabled() || len(data) == 0 {
		return data, nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // keep big integers lossless
	if err := dec.Decode(&v); err != nil {
		return data, nil // not JSON we can walk; leave untouched
	}
	newVal, hits, changed := dlpWalkValue(v, d)
	if !changed {
		return data, hits
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(newVal); err != nil {
		return data, hits
	}
	out := bytes.TrimRight(buf.Bytes(), "\n")
	return out, hits
}

// dlpWalkValue recursively visits maps/arrays, scanning strings under a
// content-bearing key (replacing them in mask mode).
func dlpWalkValue(v any, d *dlp.Detector) (any, []dlp.Hit, bool) {
	switch t := v.(type) {
	case map[string]any:
		changed := false
		var hits []dlp.Hit
		for k, val := range t {
			if s, ok := val.(string); ok && dlpContentKeys[k] {
				if hs := d.Scan(s); len(hs) > 0 {
					hits = append(hits, hs...)
					if d.MaskMode() {
						t[k], _ = d.Mask(s)
					}
					changed = true
				}
				continue
			}
			nv, nh, nc := dlpWalkValue(val, d)
			if nc {
				changed = true
				t[k] = nv
			}
			if len(nh) > 0 {
				hits = append(hits, nh...)
			}
		}
		return t, hits, changed
	case []any:
		changed := false
		var hits []dlp.Hit
		for i, item := range t {
			nv, nh, nc := dlpWalkValue(item, d)
			if nc {
				changed = true
				t[i] = nv
			}
			if len(nh) > 0 {
				hits = append(hits, nh...)
			}
		}
		return t, hits, changed
	default:
		return v, nil, false
	}
}

// dlpResponseBody applies DLP to a completed response body: mask mode
// returns the masked body, reject mode writes the rejection and returns
// proceed=false.
func (h handler) dlpResponseBody(w http.ResponseWriter, r *http.Request, started time.Time, endpoint, alias string, candidate routing.Candidate, respBody []byte) ([]byte, bool) {
	if h.rt().dlpDetector == nil || !h.rt().dlpDetector.Enabled() || len(respBody) == 0 {
		return respBody, true
	}
	masked, hits := dlpApplyNonStreaming(respBody, h.rt().dlpDetector)
	if len(hits) == 0 {
		return respBody, true
	}
	if h.rt().dlpDetector.RejectMode() {
		h.writeDLPRejectNonStreaming(w, r, started, endpoint, alias, candidate, hits)
		return respBody, false
	}
	h.logDLPHits(r.Context(), endpoint, alias, hits, "mask")
	return masked, true
}

// dlpHitPatterns returns distinct hit pattern IDs.
func dlpHitPatterns(hits []dlp.Hit) []string {
	if len(hits) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(hits))
	var out []string
	for _, h := range hits {
		if !seen[h.Pattern] {
			seen[h.Pattern] = true
			out = append(out, h.Pattern)
		}
	}
	return out
}
