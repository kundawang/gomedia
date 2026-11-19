package ogg

import (
	"errors"

	"github.com/yapingcat/gomedia/go-codec"
)

// Chained Ogg demuxing (RFC 3534 section "Chained Bitstreams"):
//
// A physical Ogg stream is a sequence of chain segments. Every segment starts
// with one or more BOS pages (flag 0x02), one per logical stream, and every
// logical stream finishes with an EOS page (flag 0x04). A BOS page seen after
// the current segment has carried data pages therefore starts a new segment.
// BOS pages arriving before any data page belong to the same segment, which is
// how multiplexed logical streams keep their original order.
//
// Deterministic rules for malformed input:
//
//   - Duplicate BOS: a BOS page uses a serial number still active in the
//     current segment. Input returns ErrDuplicateBOS immediately.
//   - Missing EOS: a new segment begins (or Flush is called) while a stream
//     has not emitted EOS. The segment is still finalized: mid-stream parsing
//     continues and OnSegmentEnd reports MissingEOS=true; Flush returns
//     ErrMissingEOS for the unterminated tail segment.
//   - Truncated tail: partial page header/payload (or an unterminated
//     continued packet) is left when Flush is called. Flush finalizes the
//     segment with Truncated=true and returns ErrTruncatedPage.
//   - Empty segment: a segment whose streams never emit a media packet is
//     reported with Empty=true and skipped; parsing continues.
//   - Serial reuse: a serial used by an already retired segment is legal.
//     The stream is rebuilt from scratch and the reuse is recorded in
//     SegmentInfo.SerialReused; page sequence gaps are only tracked inside a
//     segment, so a reset sequence number is never reported as a lost page.
var (
	ErrDuplicateBOS  = errors.New("ogg: duplicate BOS page for an active logical stream")
	ErrMissingEOS    = errors.New("ogg: logical stream ended without an EOS page")
	ErrTruncatedPage = errors.New("ogg: truncated page at end of input")
)

// ChainFrame is one decoded media frame annotated with chain position.
//
// SegmentPts is the timestamp inside the segment in native granule units
// (audio: samples at the codec granule rate; video: frame index).
// SegmentPtsMs and TotalTimeMs are the same timestamp converted to
// milliseconds, the latter offset by the cumulative duration of every
// preceding segment.
type ChainFrame struct {
	Segment      int
	Serial       uint32
	StreamOrder  int
	CodecId      codec.CodecID
	Frame        []byte
	SegmentPts   uint64
	SegmentPtsMs uint64
	TotalTimeMs  uint64
	Lost         int
}

// StreamFormat describes a logical stream as parsed from its codec header.
//
// A StreamFormat is emitted once per logical stream per segment. Changed is
// set when the audio sample rate / channel count (or video dimensions) differ
// from the format reported by the previous segment.
type StreamFormat struct {
	Segment      int
	Serial       uint32
	StreamOrder  int
	CodecId      codec.CodecID
	SampleRate   uint32
	ChannelCount uint32
	Width        uint32
	Height       uint32
	FrameRate    uint32
	ExtraData    []byte
	Changed      bool
}

// SegmentInfo is emitted when a segment is finalized.
type SegmentInfo struct {
	Index        int
	Serials      []uint32
	DurationMs   uint64
	BaseTimeMs   uint64
	MissingEOS   bool
	Truncated    bool
	Empty        bool
	SerialReused []uint32
}

type chainSegment struct {
	index         int
	serials       []uint32
	reusedSerials []uint32
	hasData       bool
	baseTimeMs    uint64
	mediaPackets  uint64
}

// Chain callback semantics (the callbacks themselves are fields of Demuxer):
//
// OnSegmentBegin fires when a new chain segment starts. serial is the serial
// number of its first BOS page; further multiplexed streams are reported
// through OnStreamFormat in BOS order.
// OnSegmentEnd fires once per finalized segment, including empty and
// truncated ones.
// OnStreamFormat fires after a codec header has been parsed, once per logical
// stream per segment.
// OnChainFrame fires for every decoded media frame with segment-relative and
// cumulative timestamps. The legacy OnFrame/OnPacket callbacks keep firing as
// before, so single-segment callers are unaffected.
func (demuxer *Demuxer) beginSegment(bos *oggPage) {
	seg := &chainSegment{
		index:      demuxer.segCount,
		baseTimeMs: demuxer.baseTimeMs,
	}
	demuxer.segCount++
	demuxer.segment = seg
	if demuxer.OnSegmentBegin != nil {
		var serial uint32
		if bos != nil {
			serial = bos.streamId
		}
		demuxer.OnSegmentBegin(seg.index, serial)
	}
}

// handleBOS applies the chain rules to a BOS page and returns the freshly
// created logical stream. A new oggStream (and therefore a new codec parser)
// is always built, so granule positions and parser state never leak across
// segment boundaries.
func (demuxer *Demuxer) handleBOS(page *oggPage) (*oggStream, error) {
	if _, active := demuxer.streams[page.streamId]; active {
		return nil, ErrDuplicateBOS
	}
	// A BOS after data pages closes the previous segment. BOS pages seen
	// before the first data page join the current multiplexed segment.
	if demuxer.segment != nil && demuxer.segment.hasData {
		demuxer.endSegment(false)
	}
	if demuxer.segment == nil {
		demuxer.beginSegment(page)
	}
	reused := demuxer.retired[page.streamId]
	stream := demuxer.newStream(page)
	if reused {
		demuxer.segment.reusedSerials = append(demuxer.segment.reusedSerials, page.streamId)
	}
	return stream, nil
}

func (demuxer *Demuxer) newStream(page *oggPage) *oggStream {
	stream := &oggStream{
		currentPage: page,
		streamId:    page.streamId,
		cache:       make([]byte, 0, 1024),
		cid:         codec.CODECID_UNRECOGNIZED,
		segment:     demuxer.segment,
	}
	if demuxer.segment != nil {
		stream.order = len(demuxer.segment.serials)
		demuxer.segment.serials = append(demuxer.segment.serials, page.streamId)
	}
	demuxer.streams[page.streamId] = stream
	return stream
}

func (demuxer *Demuxer) endSegment(truncated bool) {
	seg := demuxer.segment
	if seg == nil {
		return
	}
	var durationMs uint64
	missingEOS := false
	for _, stream := range demuxer.streams {
		if !stream.hasEOS {
			missingEOS = true
		}
		seg.mediaPackets += stream.mediaPackets
		if ms := streamDurationMs(stream); ms > durationMs {
			durationMs = ms
		}
	}
	info := &SegmentInfo{
		Index:        seg.index,
		Serials:      append([]uint32(nil), seg.serials...),
		DurationMs:   durationMs,
		BaseTimeMs:   seg.baseTimeMs,
		MissingEOS:   missingEOS,
		Truncated:    truncated,
		Empty:        seg.mediaPackets == 0,
		SerialReused: append([]uint32(nil), seg.reusedSerials...),
	}
	if demuxer.OnSegmentEnd != nil {
		demuxer.OnSegmentEnd(info)
	}
	demuxer.baseTimeMs += durationMs
	for serial := range demuxer.streams {
		demuxer.retired[serial] = true
	}
	demuxer.streams = make(map[uint32]*oggStream)
	demuxer.segment = nil
}

// Flush must be called after the last Input. It finalizes a segment still open
// at end of input and reports the deterministic tail errors described on the
// ErrDuplicateBOS/ErrMissingEOS/ErrTruncatedPage variables.
func (demuxer *Demuxer) Flush() error {
	if len(demuxer.headCache) > 0 {
		if demuxer.segment != nil {
			demuxer.endSegment(true)
		}
		return ErrTruncatedPage
	}
	if demuxer.state == DEMUX_PAGE_PAYLOAD && demuxer.currentStream != nil {
		page := demuxer.currentStream.currentPage
		if len(page.cache) > 0 || len(demuxer.currentStream.cache) > 0 {
			if demuxer.segment != nil {
				demuxer.endSegment(true)
			}
			return ErrTruncatedPage
		}
	}
	if demuxer.segment != nil {
		missingEOS := false
		for _, stream := range demuxer.streams {
			if !stream.hasEOS {
				missingEOS = true
			}
		}
		demuxer.endSegment(false)
		if missingEOS {
			return ErrMissingEOS
		}
	}
	return nil
}

// SegmentCount returns the number of chain segments started so far.
func (demuxer *Demuxer) SegmentCount() int {
	return demuxer.segCount
}

// CumulativeTimeMs returns the playback time accumulated over all finalized
// segments. During an open segment, add the current frame's SegmentPtsMs.
func (demuxer *Demuxer) CumulativeTimeMs() uint64 {
	return demuxer.baseTimeMs
}

func streamDurationMs(stream *oggStream) uint64 {
	switch stream.cid {
	case codec.CODECID_AUDIO_OPUS:
		// Opus granule positions always run at 48 kHz, regardless of the
		// input sample rate advertised in OpusHead.
		if stream.hasEOS && stream.eosGranule > 0 {
			return stream.eosGranule * 1000 / 48000
		}
		return stream.accSamples * 1000 / 48000
	case codec.CODECID_VIDEO_VP8:
		if vp8, ok := stream.parser.(*vp8Demuxer); ok && vp8.frameRate > 0 {
			return stream.frameIndex * 1000 / uint64(vp8.frameRate)
		}
	}
	return 0
}

// emitStreamFormat is called right after a codec header packet was parsed.
func (demuxer *Demuxer) emitStreamFormat(stream *oggStream) {
	format := &StreamFormat{
		Segment:     stream.segment.index,
		Serial:      stream.streamId,
		StreamOrder: stream.order,
		CodecId:     stream.cid,
	}
	switch stream.cid {
	case codec.CODECID_AUDIO_OPUS:
		opus, _ := stream.parser.(*opusDemuxer)
		format.SampleRate = uint32(opus.ctx.SampleRate)
		format.ChannelCount = uint32(opus.ctx.ChannelCount)
		format.ExtraData = opus.extradata
		if demuxer.lastAudioSR != 0 || demuxer.lastAudioCH != 0 {
			format.Changed = demuxer.lastAudioSR != format.SampleRate ||
				demuxer.lastAudioCH != format.ChannelCount
		}
		demuxer.lastAudioSR = format.SampleRate
		demuxer.lastAudioCH = format.ChannelCount
	case codec.CODECID_VIDEO_VP8:
		vp8, _ := stream.parser.(*vp8Demuxer)
		format.Width = uint32(vp8.width)
		format.Height = uint32(vp8.height)
		format.FrameRate = vp8.frameRate
		format.ExtraData = vp8.extradata
		if demuxer.lastVideoW != 0 || demuxer.lastVideoH != 0 {
			format.Changed = demuxer.lastVideoW != format.Width ||
				demuxer.lastVideoH != format.Height
		}
		demuxer.lastVideoW = format.Width
		demuxer.lastVideoH = format.Height
	default:
		return
	}
	if demuxer.OnStreamFormat != nil {
		demuxer.OnStreamFormat(format)
	}
}

// emitChainFrame annotates a decoded media frame with its chain position and
// advances the per-segment timeline. State is segment local because a new
// oggStream is built for every segment.
func (demuxer *Demuxer) emitChainFrame(stream *oggStream, frame []byte, lost int) {
	cf := &ChainFrame{
		Segment:     stream.segment.index,
		Serial:      stream.streamId,
		StreamOrder: stream.order,
		CodecId:     stream.cid,
		Frame:       frame,
		Lost:        lost,
	}
	switch stream.cid {
	case codec.CODECID_AUDIO_OPUS:
		cf.SegmentPts = stream.accSamples
		cf.SegmentPtsMs = stream.accSamples * 1000 / 48000
		stream.accSamples += codec.OpusPacketDuration(frame)
	case codec.CODECID_VIDEO_VP8:
		cf.SegmentPts = stream.frameIndex
		if vp8, ok := stream.parser.(*vp8Demuxer); ok && vp8.frameRate > 0 {
			cf.SegmentPtsMs = stream.frameIndex * 1000 / uint64(vp8.frameRate)
		}
		stream.frameIndex++
	}
	stream.mediaPackets++
	cf.TotalTimeMs = stream.segment.baseTimeMs + cf.SegmentPtsMs
	if demuxer.OnChainFrame != nil {
		demuxer.OnChainFrame(cf)
	}
}
