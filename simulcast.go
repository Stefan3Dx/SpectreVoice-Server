package main

// Simulcast: a streamer that sends its picture in layers (rid "h" full size, "m" half, "l" a quarter) lets each
// viewer get the one its connection takes. The server never transcodes: for every viewer it keeps one outgoing
// video track and copies into it the packets of one layer at a time, switching at a key frame and rewriting the
// sequence numbers and timestamps so the viewer sees a single, continuous stream.
//
// A viewer asks for a layer with {"type":"layer","q":"h"|"m"|"l"|"auto"}; "auto" (the default) picks by what the
// viewer's own RTCP says: its receiver reports (loss), REMB (bandwidth) and NACKs. The server tells the viewer
// {"type":"layer","layer":<sent>,"q":<asked>,"layers":[…]} whenever that changes.

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

// A layer: one of the streamer's encodings, by its rid.
type layer struct {
	rid   string
	ssrc  webrtc.SSRC
	bytes atomic.Int64 // since the last measure
	kbps  atomic.Int64 // measured, smoothed
	last  atomic.Int64 // unix nanos of the last packet
}

// alive: the layer sent something lately (an encoder may stop a layer it can't afford).
func (l *layer) alive(now time.Time) bool { return now.UnixNano()-l.last.Load() < 2*int64(time.Second) }

// simulcast is a room's layered picture.
type simulcast struct {
	mu      sync.RWMutex
	codec   webrtc.RTPCodecCapability
	id      string
	layers  map[string]*layer
	viewers map[*subscriber]*switcher
}

// layerRank orders rids best first; unknown rids go after the known ones, by name.
func layerRank(rid string) int {
	switch rid {
	case "h":
		return 0
	case "m":
		return 1
	case "l":
		return 2
	}
	return 3
}

// ordered gives the layers best first.
func (sc *simulcast) ordered() []*layer {
	sc.mu.RLock()
	out := make([]*layer, 0, len(sc.layers))
	for _, l := range sc.layers {
		out = append(out, l)
	}
	sc.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := layerRank(out[i].rid), layerRank(out[j].rid)
		if a != b {
			return a < b
		}
		return out[i].rid < out[j].rid
	})
	return out
}

func (sc *simulcast) rids() []string {
	ls := sc.ordered()
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.rid
	}
	return out
}

// sending: layer rid exists and sent something lately.
func (sc *simulcast) sending(rid string, now time.Time) bool {
	sc.mu.RLock()
	l := sc.layers[rid]
	sc.mu.RUnlock()
	return l != nil && l.alive(now)
}

// nearest: the layer closest to the one asked that is sending now (asked for "l" while only "h" and "m" come:
// "m"); "" when none is.
func (sc *simulcast) nearest(rid string, now time.Time) string {
	best, bestD := "", 1<<30
	for _, l := range sc.ordered() {
		if !l.alive(now) {
			continue
		}
		d := layerRank(l.rid) - layerRank(rid)
		if d < 0 {
			d = -d
		}
		if d < bestD {
			best, bestD = l.rid, d
		}
	}
	return best
}

// below: the sending layer next under [rid] ("" none).
func (sc *simulcast) below(rid string, now time.Time) string {
	for _, l := range sc.ordered() {
		if layerRank(l.rid) > layerRank(rid) && l.alive(now) {
			return l.rid
		}
	}
	return ""
}

// ====== The switcher: one layer at a time into a viewer's track ======

// switcher copies one layer's packets into a viewer's track, continuously numbered across layer changes.
type switcher struct {
	mu    sync.Mutex
	track *webrtc.TrackLocalStaticRTP
	mime  string
	cur   string // the layer being sent ("" none yet)
	want  string // the layer to switch to at its next key frame
	// Rewriting: the last numbers sent and the offsets added to the current layer's
	started    bool
	lastSeq    uint16
	lastTS     uint32
	seqOff     uint16
	tsOff      uint32
	lastInTS   uint32 // the current layer's own last timestamp (to learn the frame spacing)
	frameDelta uint32 // RTP ticks between frames (90 kHz), learned; 3000 = 30 fps
	switches   int
	wantAt     time.Time // when [want] was set (a switch whose key frame doesn't come is given up, autoTick)
	// What the viewer's own link lost, apart from what the streamer's upload did: the numbers sent lately (a NACK
	// for one never sent is a packet the server never had), and the numbers skipped since the last tick
	sentLog [1024]uint32 // seq+1 at seq%1024 (0: none)
	fwd     int
	gaps    int
}

// wasSent: the viewer's sequence number [seq] went out to it lately.
func (w *switcher) wasSent(seq uint16) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sentLog[seq%1024] == uint32(seq)+1
}

// upstreamLoss: the share of the numbers since the last call that never came from the streamer (0–100); resets.
func (w *switcher) upstreamLoss() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	f, g := w.fwd, w.gaps
	w.fwd, w.gaps = 0, 0
	if f+g == 0 {
		return 0
	}
	return float64(g) * 100 / float64(f+g)
}

// pendingFor: how long a switch has waited for its key frame (0: none pending).
func (w *switcher) pendingFor(now time.Time) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.want == w.cur || w.wantAt.IsZero() {
		return 0
	}
	return now.Sub(w.wantAt)
}

func newSwitcher(track *webrtc.TrackLocalStaticRTP, mime string) *switcher {
	return &switcher{track: track, mime: mime, frameDelta: 3000}
}

// choose sets the layer wanted; true when a switch is pending (a key frame of that layer is needed).
func (w *switcher) choose(rid string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if rid != w.want {
		w.wantAt = time.Now()
	}
	w.want = rid
	return rid != w.cur
}

// current: the layer being sent and the one wanted.
func (w *switcher) current() (cur, want string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cur, w.want
}

// push takes a packet of [rid]; it goes on to the viewer if that's the layer sent (or, at a key frame, the one
// wanted). The packet is changed in place. Returns whether the layer just switched.
func (w *switcher) push(rid string, p *rtp.Packet) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	switched := false
	if w.want != w.cur && rid == w.want && isKeyFrame(w.mime, p.Payload) {
		if w.started {
			w.seqOff = w.lastSeq + 1 - p.SequenceNumber
			w.tsOff = w.lastTS + w.frameDelta - p.Timestamp
		}
		w.cur = w.want
		w.lastInTS = p.Timestamp
		w.switches++
		switched = true
	}
	if rid != w.cur {
		return false
	}
	// Frame spacing, from the layer's own clock (consecutive, forward, under a second)
	if d := p.Timestamp - w.lastInTS; d > 0 && d < 90000 && w.started && !switched {
		w.frameDelta = d
	}
	if p.Timestamp != w.lastInTS && int32(p.Timestamp-w.lastInTS) > 0 {
		w.lastInTS = p.Timestamp
	}
	p.SequenceNumber += w.seqOff
	p.Timestamp += w.tsOff
	if !w.started || int16(p.SequenceNumber-w.lastSeq) > 0 {
		if w.started && !switched {
			w.gaps += int(p.SequenceNumber-w.lastSeq) - 1
		}
		w.lastSeq = p.SequenceNumber
	} else if w.gaps > 0 {
		w.gaps-- // a late one (a retransmission) fills a gap
	}
	w.fwd++
	w.sentLog[p.SequenceNumber%1024] = uint32(p.SequenceNumber) + 1
	if !w.started || int32(p.Timestamp-w.lastTS) > 0 {
		w.lastTS = p.Timestamp
	}
	w.started = true
	// The streamer's header extensions (its mid and rid) mean nothing to the viewer; the viewer's own are added on
	// the way out
	p.Header.Extension = false
	p.Header.Extensions = nil
	_ = w.track.WriteRTP(p)
	return switched
}

// isKeyFrame: whether an RTP payload starts a key frame (the only place a viewer can join a layer).
func isKeyFrame(mime string, payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	switch mime {
	case webrtc.MimeTypeH264:
		return h264KeyFrame(payload)
	case webrtc.MimeTypeVP8:
		var v codecs.VP8Packet
		if _, err := v.Unmarshal(payload); err != nil || len(v.Payload) == 0 {
			return false
		}
		return v.S == 1 && v.PID == 0 && v.Payload[0]&0x01 == 0
	case webrtc.MimeTypeVP9:
		var v codecs.VP9Packet
		if _, err := v.Unmarshal(payload); err != nil {
			return false
		}
		return v.B && !v.P
	case webrtc.MimeTypeAV1:
		// Aggregation header: Z (a continuation) must be 0, N (a new coded video sequence) 1
		return payload[0]&0x80 == 0 && payload[0]&0x08 != 0
	}
	return false
}

// h264KeyFrame: an IDR slice or an SPS at the start of a packet (single NAL, STAP-A, or the first FU-A piece).
func h264KeyFrame(p []byte) bool {
	const idr, sps, stapA, fuA = 5, 7, 24, 28
	switch t := p[0] & 0x1f; t {
	case idr, sps:
		return true
	case stapA:
		for i := 1; i+2 < len(p); {
			n := int(p[i])<<8 | int(p[i+1])
			i += 2
			if i >= len(p) {
				return false
			}
			if nt := p[i] & 0x1f; nt == idr || nt == sps {
				return true
			}
			i += n
		}
	case fuA:
		if len(p) < 2 {
			return false
		}
		return p[1]&0x80 != 0 && (p[1]&0x1f == idr || p[1]&0x1f == sps)
	}
	return false
}

// ====== Choosing a layer for a viewer ======

// What a viewer's RTCP said over the last second.
type linkSample struct {
	lossPct  float64 // from its receiver reports (0–100)
	rembKbps int     // its bandwidth estimate (0: none yet)
	nacks    int     // packets it asked again
}

// A layer the picker may choose, with its measured rate.
type layerInfo struct {
	rid  string
	kbps int
}

// The automatic rule's thresholds.
const (
	autoLossPct  = 8.0 // more loss than this: the layer is too much
	autoCleanPct = 2.0 // less than this (and few NACKs): the link is clean
	autoHeadroom = 1.2 // going up: the next layer needs its rate × this of estimated bandwidth
	autoRoom     = 1.3 // or the estimate this far over the current layer's rate (see step)
	// Staying: the estimate must not fall under the layer's rate × this. A receiver's estimate follows what comes
	// in (and the streamer's own upload's jitter), so the layer's rate × 1.2 was missed now and then on a link
	// with room to spare, and a 1440p viewer was moved down for nothing
	autoKeep     = 0.7
	autoStepDown = 3 * time.Second // bad for this long: one layer down
	autoStepUp   = 15 * time.Second
	// A viewer that never had trouble yet (on a smaller layer only because the best wasn't there when it came)
	autoStepUpFirst = 3 * time.Second
	autoNackBad     = 10 // NACKs a second that count as loss
	autoNackClean   = 2
)

// picker decides a viewer's layer in auto mode: down after 3 s of trouble, up after 15 s clean.
type picker struct {
	badSince  time.Time
	goodSince time.Time
	upAt      time.Time     // the last step up (one undone soon after makes the next try wait longer)
	upWait    time.Duration // how long clean before stepping up (autoStepUp, doubled after a failed try)
	downs     int           // steps down so far: before any, a viewer that started under the best (it wasn't
	// sending yet when the viewer came) goes up after autoStepUpFirst, not autoStepUp
}

// upWaitMax: the longest a viewer waits, clean, before trying the next layer up again.
const upWaitMax = 2 * time.Minute

// step gives the layer to send next, among [layers] (best first), being on [cur].
func (pk *picker) step(cur string, layers []layerInfo, s linkSample, now time.Time) string {
	if len(layers) == 0 {
		return cur
	}
	i := -1
	for k, l := range layers {
		if l.rid == cur {
			i = k
		}
	}
	if i < 0 {
		// Not on any known layer: start at the best
		pk.badSince, pk.goodSince = time.Time{}, time.Time{}
		return layers[0].rid
	}
	if !pk.upAt.IsZero() && now.Sub(pk.upAt) >= autoStepUp {
		// The last try up held: the next one waits the normal time again (the doubling is for tries that fail)
		pk.upAt, pk.upWait = time.Time{}, 0
	}
	tooMuch := s.lossPct > autoLossPct || s.nacks > autoNackBad ||
		(s.rembKbps > 0 && layers[i].kbps > 0 && float64(s.rembKbps) < float64(layers[i].kbps)*autoKeep)
	if tooMuch {
		pk.goodSince = time.Time{}
		if pk.badSince.IsZero() {
			pk.badSince = now
		}
		if now.Sub(pk.badSince) >= autoStepDown && i+1 < len(layers) {
			pk.badSince = time.Time{}
			// Just went up and it didn't hold: the next try waits twice as long
			if !pk.upAt.IsZero() && now.Sub(pk.upAt) < autoStepUp {
				pk.upWait = min(upWaitMax, 2*pk.wait())
			}
			pk.upAt = time.Time{}
			pk.downs++
			return layers[i+1].rid
		}
		return cur
	}
	pk.badSince = time.Time{}
	if i == 0 {
		pk.goodSince = time.Time{}
		return cur
	}
	up := layers[i-1]
	// Room for the next layer: its rate × 1.2 of estimate, or (the estimate of a receiver follows what it gets, up
	// to about 1.5 times that, so a layer four times bigger could never be "estimated") clearly more than this
	// layer takes. A try that doesn't hold steps down again in 3 s and waits longer before the next
	room := s.rembKbps == 0 || up.kbps == 0 || float64(s.rembKbps) >= float64(up.kbps)*autoHeadroom ||
		(layers[i].kbps > 0 && float64(s.rembKbps) >= float64(layers[i].kbps)*autoRoom)
	clean := s.lossPct <= autoCleanPct && s.nacks <= autoNackClean && room
	if !clean {
		pk.goodSince = time.Time{}
		return cur
	}
	if pk.goodSince.IsZero() {
		pk.goodSince = now
	}
	if now.Sub(pk.goodSince) >= pk.wait() {
		pk.goodSince = time.Time{}
		pk.upAt = now
		return up.rid
	}
	return cur
}

func (pk *picker) wait() time.Duration {
	if pk.downs == 0 {
		return autoStepUpFirst
	}
	if pk.upWait < autoStepUp {
		return autoStepUp
	}
	return pk.upWait
}

// validLayer: what a viewer may ask for.
func validLayer(q string) bool {
	switch q {
	case "auto", "h", "m", "l":
		return true
	}
	return false
}
