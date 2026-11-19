package flv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// FLV tag types handled by the clipper. Besides the standard audio/video/script
// tags every other tag type is treated as an unknown tag and copied verbatim.

var (
	// ErrNotFLV means the input does not start with the "FLV" signature.
	ErrNotFLV = errors.New("flv: not an FLV file")
	// ErrTruncatedTag means the input ends in the middle of a tag or a
	// previous-tag-size field.
	ErrTruncatedTag = errors.New("flv: truncated tag")
	// ErrInvalidRange means startMs > endMs or one of them is negative.
	ErrInvalidRange = errors.New("flv: invalid clip range")
	// ErrNoKeyframe means the stream contains video but no decodable keyframe
	// exists at or before startMs (the clip cannot start on a video sample).
	ErrNoKeyframe = errors.New("flv: no keyframe at or before start time")
	// ErrEmptyRange means no media (audio/video) tag falls into the range.
	ErrEmptyRange = errors.New("flv: no media tag in target range")
	// ErrShortWrite means the writer accepted fewer bytes than requested.
	ErrShortWrite = errors.New("flv: short write")
)

// ClipStats reports what a clip did.
type ClipStats struct {
	// StartMs/EndMs are the actual unwrapped media timestamps (milliseconds,
	// relative to the source timeline) covered by the output. StartMs is the
	// keyframe the clip was extended to, which may be earlier than requested.
	StartMs int64
	EndMs   int64
	// BaseMs is the source timestamp that was rebased to 0 in the output.
	BaseMs int64
	// DurationMs equals EndMs - StartMs.
	DurationMs uint32
	// SeekedKeyframe is true when a video clip was extended backwards onto a
	// keyframe earlier than the requested start.
	SeekedKeyframe bool
	// Tags copied into the output (excluding the FLV header).
	AudioTags int
	VideoTags int
	// ScriptTags counts script tags, including the rewritten onMetaData if any.
	ScriptTags int
	// UnknownTags counts tag types other than audio(8)/video(9)/script(18).
	UnknownTags int
	// HasVideo/HasAudio report which media kinds the output contains.
	HasVideo bool
	HasAudio bool
	// VideoConfigEmitted/AudioConfigEmitted report whether AVC/HEVC or AAC
	// sequence headers were prepended to the output.
	VideoConfigEmitted bool
	AudioConfigEmitted bool
	// MetaDataPatched is true when an onMetaData duration field was rewritten.
	MetaDataPatched bool
}

// flvIndexEntry describes one parsed tag without keeping its payload in memory.
type flvIndexEntry struct {
	offset     int64 // absolute offset of the 11-byte tag header
	dataOffset int64 // absolute offset of the tag payload
	tagType    uint8
	dataSize   uint32
	// ts is the full 32-bit tag timestamp exactly as stored on disk.
	ts uint32
	// dts is ts unwrapped onto a monotonically non-decreasing int64 timeline.
	dts int64
}

const flvTimestampMod int64 = 1 << 32

// fullTimestamp joins the 24-bit timestamp and the extended byte.
func fullTimestamp(t FlvTag) uint32 {
	return uint32(t.TimestampExtended)<<24 | t.Timestamp
}

// isVideoKeyframe reports whether a video tag payload carries a seekable
// keyframe. Both legacy AVC/HEVC and enhanced-RTMP (ex header) layouts are
// handled.
func isVideoKeyframe(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	if payload[0]&0x80 != 0 {
		// enhanced flv: frame type is the 3 bits after the ex marker.
		return (payload[0]>>4)&0x07 == uint8(KEY_FRAME)
	}
	if payload[0]>>4 != uint8(KEY_FRAME) {
		return false
	}
	cid := FLV_VIDEO_CODEC_ID(payload[0] & 0x0F)
	if cid != FLV_AVC && cid != FLV_HEVC {
		// Legacy codecs (H.263 etc.): a keyframe tag is directly decodable.
		return true
	}
	// AVC/HEVC sequence headers are keyframe-tagged but are configuration,
	// not media; only NALU packets count as seek points.
	if len(payload) < 2 {
		return false
	}
	return payload[1] == AVC_NALU
}

func isVideoSequenceHeader(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	if payload[0]&0x80 != 0 {
		return payload[0]&0x0F == PacketTypeSequenceStart
	}
	cid := FLV_VIDEO_CODEC_ID(payload[0] & 0x0F)
	if cid != FLV_AVC && cid != FLV_HEVC {
		return false
	}
	return len(payload) >= 2 && payload[1] == AVC_SEQUENCE_HEADER
}

func isAudioSequenceHeader(payload []byte) bool {
	if len(payload) < 2 {
		return false
	}
	if payload[0]>>4 != uint8(FLV_AAC) {
		return false
	}
	return payload[1] == AAC_SEQUENCE_HEADER
}

// indexFlvFile parses every tag header, unwraps timestamps and validates the
// framing of the whole file. Payloads stay on disk; only offsets are retained.
func indexFlvFile(r io.ReadSeeker, size int64) (header []byte, entries []flvIndexEntry, err error) {
	if size < 9+4 {
		return nil, nil, ErrNotFLV
	}
	if _, err = r.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	header = make([]byte, 9)
	if _, err = io.ReadFull(r, header); err != nil {
		return nil, nil, err
	}
	if header[0] != 'F' || header[1] != 'L' || header[2] != 'V' {
		return nil, nil, ErrNotFLV
	}
	// PreviousTagSize0
	var prevSize [4]byte
	if _, err = io.ReadFull(r, prevSize[:]); err != nil {
		return nil, nil, ErrTruncatedTag
	}

	entries = make([]flvIndexEntry, 0, 256)
	var wrapRounds int64
	var lastDts int64
	var tagHdr [11]byte

	for {
		offset, e := r.Seek(0, io.SeekCurrent)
		if e != nil {
			return nil, nil, e
		}
		n, e := io.ReadFull(r, tagHdr[:])
		if e == io.EOF {
			break // clean end of file
		}
		if e == io.ErrUnexpectedEOF || (e == io.EOF && n > 0) {
			return nil, nil, ErrTruncatedTag
		}
		if e != nil {
			return nil, nil, e
		}
		var tag FlvTag
		tag.Decode(tagHdr[:])
		dataSize := int64(tag.DataSize)
		dataOffset := offset + 11
		if dataOffset+dataSize+4 > size {
			return nil, nil, ErrTruncatedTag
		}
		rawTs := fullTimestamp(tag)
		dts := int64(rawTs) + wrapRounds*flvTimestampMod
		// A drop larger than half the 32-bit timestamp space can only be a
		// wraparound in a well-formed interleaved stream.
		if len(entries) > 0 && dts < lastDts && lastDts-dts > flvTimestampMod/2 {
			wrapRounds++
			dts = int64(rawTs) + wrapRounds*flvTimestampMod
		}
		if dts < lastDts {
			// Out-of-order but not a wraparound: keep the unwrapped value;
			// clip selection is driven by timestamp, not by file order.
		}
		lastDts = dts
		entries = append(entries, flvIndexEntry{
			offset:     offset,
			dataOffset: dataOffset,
			tagType:    tag.TagType,
			dataSize:   tag.DataSize,
			ts:         rawTs,
			dts:        dts,
		})
		// Skip payload and the following PreviousTagSize (its value is
		// recomputed on output, so its content is irrelevant here).
		if _, e = r.Seek(dataOffset+dataSize+4, io.SeekStart); e != nil {
			return nil, nil, e
		}
	}
	return header, entries, nil
}

// clipFlv runs the full two-pass clip against an indexed seekable stream.
func clipFlv(r io.ReadSeeker, w io.Writer, startMs, endMs int64) (ClipStats, error) {
	var stats ClipStats
	if startMs < 0 || endMs < 0 || startMs > endMs {
		return stats, ErrInvalidRange
	}

	size, err := seekSize(r)
	if err != nil {
		return stats, err
	}
	header, entries, err := indexFlvFile(r, size)
	if err != nil {
		return stats, err
	}

	pr := &seekerPayloadReader{r: r}

	hasVideo := false
	for i := range entries {
		if TagType(entries[i].tagType) == VIDEO_TAG {
			hasVideo = true
		}
	}

	// Remember the latest configuration tags anywhere before the end boundary;
	// they are what makes the clipped file decodable on its own. The scan does
	// not assume monotonic timestamps, so an out-of-order tag cannot terminate
	// it early.
	var videoCfgIdx, audioCfgIdx = -1, -1
	for i := range entries {
		e := &entries[i]
		if e.dts > endMs {
			continue
		}
		switch TagType(e.tagType) {
		case VIDEO_TAG:
			p, perr := pr.readPayload(e)
			if perr != nil {
				return stats, perr
			}
			if isVideoSequenceHeader(p) {
				videoCfgIdx = i
			}
		case AUDIO_TAG:
			p, perr := pr.readPayload(e)
			if perr != nil {
				return stats, perr
			}
			if isAudioSequenceHeader(p) {
				audioCfgIdx = i
			}
		}
	}

	// Find the clip start.
	clipFirst := -1 // first emitted media tag index
	var baseDts int64
	seeked := false

	if hasVideo {
		// A decodable clip starts at a keyframe. The seek point is the last
		// keyframe at or before startMs; when startMs precedes every keyframe
		// but overlaps the stream start, the first keyframe is used. The
		// candidate is only accepted if at least one media tag at or after it
		// falls inside the window.
		kf := -1
		firstKf := -1
		for i := range entries {
			e := &entries[i]
			if TagType(e.tagType) != VIDEO_TAG {
				continue
			}
			p, perr := pr.readPayload(e)
			if perr != nil {
				return stats, perr
			}
			if !isVideoKeyframe(p) {
				continue
			}
			if firstKf < 0 {
				firstKf = i
			}
			if e.dts <= startMs {
				kf = i
			}
		}
		if firstKf < 0 {
			return stats, ErrNoKeyframe
		}
		if kf < 0 {
			if startMs <= entries[firstKf].dts && entries[firstKf].dts <= endMs {
				kf = firstKf
			}
		} else if !mediaTagInWindow(entries, kf, startMs, endMs) {
			// Seek point is the last keyframe but no media timestamp actually
			// falls inside [startMs, endMs]: nothing to export.
			return stats, ErrEmptyRange
		}
		if kf < 0 {
			return stats, ErrEmptyRange
		}
		clipFirst = kf
		baseDts = entries[kf].dts
		seeked = baseDts < startMs
	} else {
		// Pure audio (or empty): first audio tag at or after startMs.
		for i := range entries {
			e := &entries[i]
			if e.dts < startMs || e.dts > endMs {
				continue
			}
			if TagType(e.tagType) == AUDIO_TAG {
				clipFirst = i
				baseDts = e.dts
				break
			}
		}
		if clipFirst < 0 {
			return stats, ErrEmptyRange
		}
	}

	// Select media tags inside [clipFirst, endMs]. Sequence-header tags are
	// excluded here on purpose: they are emitted once at the head of the file
	// by the configuration step, even when they sit inside the media window.
	keep := make([]bool, len(entries))
	endDts := baseDts
	mediaCount := 0
	outVideo, outAudio := false, false
	for i := clipFirst; i < len(entries); i++ {
		if i == videoCfgIdx || i == audioCfgIdx {
			continue
		}
		e := &entries[i]
		if e.dts > endMs {
			continue
		}
		switch TagType(e.tagType) {
		case VIDEO_TAG, AUDIO_TAG:
			keep[i] = true
			mediaCount++
			if e.dts > endDts {
				endDts = e.dts
			}
			if TagType(e.tagType) == VIDEO_TAG {
				outVideo = true
			} else {
				outAudio = true
			}
		}
	}
	if mediaCount == 0 {
		return stats, ErrEmptyRange
	}
	// A video stream whose only selected media is config-less inter frames
	// cannot decode; the keyframe selection above prevents this, but keep the
	// guard explicit for future edits.
	if hasVideo && !outVideo {
		return stats, ErrNoKeyframe
	}

	// Metadata / script and unknown tags: retain every one whose timestamp is
	// inside (or before) the clip window, up to the end boundary, so usable
	// side data is not lost. Script tags conventionally sit at timestamp 0.
	var metadataIdx = -1
	for i := range entries {
		e := &entries[i]
		if e.dts > endMs {
			continue
		}
		switch TagType(e.tagType) {
		case SCRIPT_TAG:
			keep[i] = true
			if metadataIdx < 0 {
				if p, perr := pr.readPayload(e); perr == nil && isOnMetaData(p) {
					metadataIdx = i
				}
			}
		case AUDIO_TAG, VIDEO_TAG:
			// handled above
		default:
			keep[i] = true
		}
	}

	duration := uint32(endDts - baseDts)
	if int64(duration) != endDts-baseDts {
		return stats, fmt.Errorf("flv: clip duration %d exceeds 32-bit timestamp space", endDts-baseDts)
	}

	// ---- pass two: emit the file ----
	cw := &countingWriter{w: w}
	// FLV header: copy signature/version, recompute type flags from content.
	outHeader := make([]byte, 9)
	copy(outHeader, header[:9])
	outHeader[4] = 0
	if outAudio {
		outHeader[4] |= 0x04
	}
	if outVideo {
		outHeader[4] |= 0x01
	}
	outHeader[5], outHeader[6], outHeader[7] = 0, 0, 0
	binary.BigEndian.PutUint32(outHeader[5:9], 9)
	if err = cw.write(outHeader); err != nil {
		return stats, err
	}
	if err = cw.write(make([]byte, 4)); err != nil { // PreviousTagSize0
		return stats, err
	}

	emit := func(idx int, rebasedTs uint32, payloadPatch func([]byte) []byte) error {
		if idx < 0 {
			return nil
		}
		e := &entries[idx]
		payload, perr := pr.readPayload(e)
		if perr != nil {
			return perr
		}
		if payloadPatch != nil {
			payload = payloadPatch(payload)
		}
		return writeTag(cw, e.tagType, rebasedTs, payload)
	}

	// 1. Script tags first (onMetaData conventionally heads the file), then
	// retained unknown tags in their original file order.
	for i := range entries {
		e := &entries[i]
		if !keep[i] || TagType(e.tagType) != SCRIPT_TAG {
			continue
		}
		ts := rebaseTimestamp(e.dts, baseDts)
		var patch func([]byte) []byte
		if i == metadataIdx {
			patch = func(p []byte) []byte {
				np, ok := patchMetadataDuration(p, float64(duration)/1000.0)
				if ok {
					stats.MetaDataPatched = true
				}
				return np
			}
		}
		if err = emit(i, ts, patch); err != nil {
			return stats, err
		}
		stats.ScriptTags++
	}
	for i := range entries {
		e := &entries[i]
		if !keep[i] {
			continue
		}
		tt := TagType(e.tagType)
		if tt == AUDIO_TAG || tt == VIDEO_TAG || tt == SCRIPT_TAG {
			continue
		}
		if err = emit(i, rebaseTimestamp(e.dts, baseDts), nil); err != nil {
			return stats, err
		}
		stats.UnknownTags++
	}

	// 2. Configuration tags at timestamp 0.
	if videoCfgIdx >= 0 {
		if err = emit(videoCfgIdx, 0, nil); err != nil {
			return stats, err
		}
		stats.VideoConfigEmitted = true
	}
	if audioCfgIdx >= 0 {
		if err = emit(audioCfgIdx, 0, nil); err != nil {
			return stats, err
		}
		stats.AudioConfigEmitted = true
	}

	// 3. Media tags in original interleaved order. Only audio/video tags are
	// marked keep here: configuration tags were excluded above, and
	// script/unknown tags are emitted separately in step 1.
	for i := clipFirst; i < len(entries); i++ {
		e := &entries[i]
		if !keep[i] {
			continue
		}
		tt := TagType(e.tagType)
		if tt != AUDIO_TAG && tt != VIDEO_TAG {
			continue
		}
		ts := rebaseTimestamp(e.dts, baseDts)
		if err = emit(i, ts, nil); err != nil {
			return stats, err
		}
		switch tt {
		case VIDEO_TAG:
			stats.VideoTags++
		case AUDIO_TAG:
			stats.AudioTags++
		}
	}
	if err = cw.failed(); err != nil {
		return stats, err
	}

	stats.StartMs = baseDts
	stats.EndMs = endDts
	stats.BaseMs = baseDts
	stats.DurationMs = duration
	stats.SeekedKeyframe = seeked
	stats.HasVideo = outVideo
	stats.HasAudio = outAudio
	return stats, nil
}

func rebaseTimestamp(dts, baseDts int64) uint32 {
	v := dts - baseDts
	if v < 0 {
		// Tags before the rebasing origin (e.g. timestamp-0 metadata) clamp
		// to 0 rather than wrapping.
		return 0
	}
	return uint32(v)
}

// mediaTagInWindow reports whether an audio/video tag exists in [startMs,
// endMs] at or after index from (the chosen seek point).
func mediaTagInWindow(entries []flvIndexEntry, from int, startMs, endMs int64) bool {
	for i := from; i < len(entries); i++ {
		tt := TagType(entries[i].tagType)
		if tt != AUDIO_TAG && tt != VIDEO_TAG {
			continue
		}
		if entries[i].dts < startMs || entries[i].dts > endMs {
			continue
		}
		return true
	}
	return false
}

// seekSize returns the total length of a seekable stream.
func seekSize(r io.ReadSeeker) (int64, error) {
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err = r.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	return size, nil
}

type seekerPayloadReader struct {
	r io.ReadSeeker
}

func (p *seekerPayloadReader) readPayload(e *flvIndexEntry) ([]byte, error) {
	if _, err := p.r.Seek(e.dataOffset, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, e.dataSize)
	if _, err := io.ReadFull(p.r, buf); err != nil {
		return nil, ErrTruncatedTag
	}
	return buf, nil
}

// writeTag serializes one tag (header + payload + previous tag size).
func writeTag(cw *countingWriter, tagType uint8, ts uint32, payload []byte) error {
	tag := FlvTag{
		TagType:           tagType,
		DataSize:          uint32(len(payload)),
		Timestamp:         ts & 0x00FFFFFF,
		TimestampExtended: uint8(ts >> 24),
		StreamID:          0,
	}
	hdr := tag.Encode()
	if err := cw.write(hdr); err != nil {
		return err
	}
	if err := cw.write(payload); err != nil {
		return err
	}
	var prev [4]byte
	binary.BigEndian.PutUint32(prev[:], FLVTAG_SIZE+uint32(len(payload)))
	return cw.write(prev[:])
}

// countingWriter turns a short write into ErrShortWrite and surfaces the
// underlying writer error unchanged.
type countingWriter struct {
	w   io.Writer
	n   int64
	err error
}

func (c *countingWriter) write(p []byte) error {
	if c.err != nil {
		return c.err
	}
	n, err := c.w.Write(p)
	if err == nil && n != len(p) {
		c.err = ErrShortWrite
		return c.err
	}
	if err != nil {
		c.err = err
	}
	c.n += int64(n)
	return err
}

func (c *countingWriter) failed() error { return c.err }

// ClipFlv exports the [startMs, endMs] millisecond window of the FLV in r into
// w without transcoding. The video start is extended backwards to the nearest
// decodable keyframe; AAC/AVC (or HEVC) sequence headers are prepended so the
// output stands alone. Timestamps are rebased close to zero while preserving
// audio/video relative timing, metadata and unknown tags are retained, and the
// onMetaData duration is rewritten when present. The source stream is never
// modified.
func ClipFlv(r io.ReadSeeker, w io.Writer, startMs, endMs uint32) (ClipStats, error) {
	return clipFlv(r, w, int64(startMs), int64(endMs))
}

// ClipFlvFile is the file convenience wrapper around ClipFlv. dst is created
// (or truncated) and always closed; src is opened read-only.
func ClipFlvFile(srcPath, dstPath string, startMs, endMs uint32) (ClipStats, error) {
	var stats ClipStats
	src, err := os.Open(srcPath)
	if err != nil {
		return stats, err
	}
	defer src.Close()
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return stats, err
	}
	closed := false
	defer func() {
		if !closed {
			dst.Close()
		}
	}()
	stats, err = ClipFlv(src, dst, startMs, endMs)
	cerr := dst.Close()
	closed = true
	if err == nil && cerr != nil {
		err = cerr
	}
	return stats, err
}
